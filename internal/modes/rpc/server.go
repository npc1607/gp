package rpc

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gp/internal/agent"
	"gp/internal/ai"
	_ "gp/internal/ai/providers/anthropic"
	"gp/internal/ai/providers/openai"
	"gp/internal/core/auth"
	"gp/internal/core/resources"
	"gp/internal/core/session"
	"gp/internal/core/settings"
	"gp/internal/core/storage"
	"gp/internal/core/storage/sqlite"
	"gp/internal/extensions/js"
	"gp/internal/mcp"
	"gp/internal/modes/subagent"
	"gp/internal/tools"
	"gp/internal/tools/builtin"
)

// RunnerOptions configures the RPC/AGP server.
type RunnerOptions struct {
	ModelName     string
	BaseURL       string
	APIKey        string
	Cwd           string
	DBPath        string
	Storage       storage.Storage
	ToolAllowlist []string
	Provider      ai.Provider
	SettingsMgr   *settings.Manager
	Settings      *settings.Settings
	HomeDir       string
	AgentVersion  string
}

type questionReply struct {
	answer  string
	skipped bool
}

// Server serves AGP (Agent Gateway Protocol) requests over stdio or unix/tcp sockets.
type Server struct {
	opts          RunnerOptions
	store         storage.Storage
	ownStore      bool
	mu            sync.Mutex
	sessionGen    map[string]string
	cancels       map[string]context.CancelFunc
	approvals     map[string]chan bool
	approvalSeq   atomic.Uint64
	questions     map[string]chan questionReply
	questionSeq   atomic.Uint64
	auditCache    *auditCache
	approvedCache *auditCache
}

// NewServer creates a new AGP RPC Server.
func NewServer(opts RunnerOptions) *Server {
	return &Server{
		opts:          opts,
		sessionGen:    make(map[string]string),
		cancels:       make(map[string]context.CancelFunc),
		approvals:     make(map[string]chan bool),
		questions:     make(map[string]chan questionReply),
		auditCache:    newAuditCache(30 * time.Minute),
		approvedCache: newAuditCache(5 * time.Minute),
	}
}

func (s *Server) ensureStore() storage.Storage {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		return s.store
	}
	if s.opts.Storage != nil {
		s.store = s.opts.Storage
		return s.store
	}
	if s.opts.DBPath != "" {
		st, err := sqlite.Open(s.opts.DBPath)
		if err != nil {
			slog.Warn("failed to open sqlite db for rpc sessions; falling back to in-memory", "db", s.opts.DBPath, "err", err)
			return nil
		}
		s.store = st
		s.ownStore = true
		return s.store
	}
	return nil
}

// Close releases resources owned by the server.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ownStore && s.store != nil {
		err := s.store.Close()
		s.store = nil
		return err
	}
	return nil
}

// ServeStdio serves AGP JSONL frames over stdin/stdout.
func (s *Server) ServeStdio(ctx context.Context) error {
	defer func() { _ = s.Close() }()
	return s.serveStream(ctx, os.Stdin, os.Stdout)
}

// ServeListen serves AGP JSONL frames over a unix:// or tcp:// listener.
func (s *Server) ServeListen(ctx context.Context, endpoint string) error {
	defer func() { _ = s.Close() }()

	network, addr, err := parseEndpoint(endpoint)
	if err != nil {
		return err
	}

	if network == "unix" {
		_ = os.Remove(addr)
	}

	ln, err := net.Listen(network, addr)
	if err != nil {
		return fmt.Errorf("listen %s %s: %w", network, addr, err)
	}
	defer func() {
		_ = ln.Close()
		if network == "unix" {
			_ = os.Remove(addr)
		}
	}()

	slog.Info("AGP server listening", "network", network, "addr", addr)

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept connection: %w", err)
		}
		go func(c net.Conn) {
			defer func() { _ = c.Close() }()
			if err := s.serveStream(ctx, c, c); err != nil {
				slog.Debug("connection closed with error", "err", err)
			}
		}(conn)
	}
}

func parseEndpoint(endpoint string) (string, string, error) {
	switch {
	case strings.HasPrefix(endpoint, "unix://"):
		return "unix", strings.TrimPrefix(endpoint, "unix://"), nil
	case strings.HasPrefix(endpoint, "tcp://"):
		return "tcp", strings.TrimPrefix(endpoint, "tcp://"), nil
	default:
		return "", "", fmt.Errorf("unsupported endpoint %q (expected unix://... or tcp://...)", endpoint)
	}
}

func (s *Server) serveStream(ctx context.Context, r io.Reader, w io.Writer) error {
	// 每轮运行独立协程执行，读取循环保持可读：这样运行中投递的 abort 才能被及时处理。
	// 连接断开时先取消子 ctx，再等待在途运行收尾，避免协程泄漏。
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()

	var writeMu sync.Mutex
	emit := func(ev Event) {
		data, err := json.Marshal(ev)
		if err != nil {
			return
		}
		data = append(data, '\n')
		writeMu.Lock()
		_, _ = w.Write(data)
		if flusher, ok := w.(interface{ Flush() error }); ok {
			_ = flusher.Flush()
		}
		writeMu.Unlock()
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		if ctx.Err() != nil {
			return nil
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var req Request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			emit(Event{
				Event:   EventError,
				Code:    "INVALID_JSON",
				Message: err.Error(),
			})
			continue
		}

		s.dispatch(ctx, emit, req, &wg)
	}

	return scanner.Err()
}

func (s *Server) dispatch(ctx context.Context, emit func(Event), req Request, wg *sync.WaitGroup) {
	switch req.Op {
	case OpHello:
		emit(Event{
			ID:           req.ID,
			Event:        EventHelloAck,
			AgentName:    "gp",
			AgentVersion: s.opts.AgentVersion,
		})
	case OpPing:
		emit(Event{ID: req.ID, Event: EventPong})
	case OpResetSession:
		s.mu.Lock()
		if req.SessionID != "" {
			s.sessionGen[req.SessionID] = fmt.Sprintf("%s_sess_%s", req.SessionID, newUUIDv4())
		}
		s.mu.Unlock()
		emit(Event{ID: req.ID, Event: EventResetAck, SessionID: req.SessionID})
	case OpListSessions:
		s.handleListSessions(emit, req)
	case OpResumeSession:
		s.handleResumeSession(emit, req)
	case OpDeleteSession:
		s.handleDeleteSession(emit, req)
	case OpAbort:
		s.mu.Lock()
		cancel, ok := s.cancels[req.TargetID]
		s.mu.Unlock()
		if ok && cancel != nil {
			cancel()
		}
		emit(Event{ID: req.ID, Event: EventAbortAck})
	case OpApprovalReply:
		s.resolveApproval(req.ApprovalID, req.Approved)
	case OpQuestionReply:
		s.resolveQuestion(req.QuestionID, req.Answer, req.Skipped)
	case OpRun:
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleRun(ctx, emit, req)
		}()
	default:
		emit(Event{
			ID:      req.ID,
			Event:   EventError,
			Code:    "UNSUPPORTED_OP",
			Message: fmt.Sprintf("unsupported op %q", req.Op),
		})
	}
}

func newUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (s *Server) effectiveSessionID(sessionID string) string {
	if sessionID == "" {
		return fmt.Sprintf("sess_%s", newUUIDv4())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur := s.sessionGen[sessionID]; cur != "" {
		return cur
	}
	return sessionID
}

func sessionBelongsToChat(chatSessionID, targetID string) bool {
	if chatSessionID == "" {
		return targetID != ""
	}
	return targetID == chatSessionID || strings.HasPrefix(targetID, chatSessionID+"_")
}

func (s *Server) handleListSessions(emit func(Event), req Request) {
	store := s.ensureStore()
	lister, ok := store.(storage.SessionLister)
	if !ok {
		emit(Event{ID: req.ID, Event: EventError, Code: "UNSUPPORTED", Message: "storage does not support listing sessions"})
		return
	}
	all, err := lister.ListSessions(100)
	if err != nil {
		emit(Event{ID: req.ID, Event: EventError, Code: "STORAGE_ERROR", Message: err.Error()})
		return
	}
	var out []SessionSummary
	for _, item := range all {
		if strings.Contains(item.SessionID, ":sub:") {
			continue
		}
		if req.SessionID != "" && !sessionBelongsToChat(req.SessionID, item.SessionID) {
			continue
		}
		out = append(out, SessionSummary{
			SessionID:    item.SessionID,
			MessageCount: item.MessageCount,
			FirstPrompt:  item.FirstPrompt,
			UpdatedAt:    item.UpdatedAt.Local().Format("2006-01-02 15:04:05"),
		})
		if len(out) >= 20 {
			break
		}
	}
	emit(Event{
		ID:        req.ID,
		Event:     EventSessionsList,
		SessionID: s.effectiveSessionID(req.SessionID),
		Sessions:  out,
	})
}

func (s *Server) handleResumeSession(emit func(Event), req Request) {
	targetID := strings.TrimSpace(req.TargetID)
	if targetID == "" || !sessionBelongsToChat(req.SessionID, targetID) {
		emit(Event{ID: req.ID, Event: EventError, Code: "INVALID_SESSION", Message: "目标会话不存在或不属于当前群聊"})
		return
	}
	s.mu.Lock()
	s.sessionGen[req.SessionID] = targetID
	s.mu.Unlock()
	emit(Event{ID: req.ID, Event: EventResumeAck, SessionID: targetID})
}

func (s *Server) handleDeleteSession(emit func(Event), req Request) {
	targetID := strings.TrimSpace(req.TargetID)
	if targetID == "" || !sessionBelongsToChat(req.SessionID, targetID) {
		emit(Event{ID: req.ID, Event: EventError, Code: "INVALID_SESSION", Message: "目标会话不存在或不属于当前群聊"})
		return
	}
	store := s.ensureStore()
	deleter, ok := store.(storage.SessionDeleter)
	if !ok {
		emit(Event{ID: req.ID, Event: EventError, Code: "UNSUPPORTED", Message: "storage does not support deleting sessions"})
		return
	}
	if err := deleter.DeleteSession(targetID); err != nil {
		emit(Event{ID: req.ID, Event: EventError, Code: "STORAGE_ERROR", Message: err.Error()})
		return
	}
	s.mu.Lock()
	if cur := s.sessionGen[req.SessionID]; cur == targetID || (cur == "" && targetID == req.SessionID) {
		s.sessionGen[req.SessionID] = fmt.Sprintf("%s_sess_%s", req.SessionID, newUUIDv4())
	}
	s.mu.Unlock()
	emit(Event{ID: req.ID, Event: EventDeleteAck, SessionID: targetID})
}

func (s *Server) handleRun(ctx context.Context, emit func(Event), req Request) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		emit(Event{ID: req.ID, Event: EventError, Code: "EMPTY_PROMPT", Message: "prompt is required"})
		return
	}

	runCtx := ctx
	if req.Options != nil && req.Options.TimeoutSec > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(req.Options.TimeoutSec)*time.Second)
		defer cancel()
	}

	cancellableCtx, abortCancel := context.WithCancel(runCtx)
	s.mu.Lock()
	s.cancels[req.ID] = abortCancel
	s.mu.Unlock()
	defer func() {
		abortCancel()
		s.mu.Lock()
		delete(s.cancels, req.ID)
		s.mu.Unlock()
	}()

	cwd := s.opts.Cwd
	if req.Options != nil && req.Options.WorkDir != "" {
		cwd = req.Options.WorkDir
	}
	if cwd == "" {
		if dir, err := os.Getwd(); err == nil {
			cwd = dir
		} else {
			cwd = "."
		}
	}

	cfg := s.resolveSettings(cwd)
	homeDir := s.opts.HomeDir
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}

	modelName := s.resolveModelName(cfg, req.Options)
	baseURL := s.resolveBaseURL(cfg)
	apiKey := s.resolveAPIKey(cfg)

	authStore := auth.NewStore(homeDir)
	if inferred := auth.InferProviderFromModel(modelName); inferred != "" && apiKey == "" {
		if k, u, _, ok := authStore.ResolveCredential(inferred); ok {
			apiKey = k
			if u != "" {
				baseURL = u
			}
		}
	}

	prov := s.opts.Provider
	if prov == nil {
		prov = ai.CreateProviderFromPlugins("", apiKey, baseURL, modelName)
		if prov == nil {
			prov = openai.NewProvider(apiKey, baseURL)
		}
	}

	var allowlist []string
	if req.Options != nil && req.Options.Policy != nil && len(req.Options.Policy.Tools) > 0 {
		allowlist = req.Options.Policy.Tools
	} else if req.Options != nil && len(req.Options.Tools) > 0 {
		allowlist = req.Options.Tools
	} else {
		allowlist = s.opts.ToolAllowlist
	}

	reg, err := s.resolveRegistry(cwd, homeDir, cfg, allowlist)
	if err != nil {
		emit(Event{ID: req.ID, Event: EventError, Code: "REGISTRY_ERROR", Message: err.Error()})
		return
	}

	var policy ExecutionPolicy
	var caller CallerContext
	if req.Options != nil {
		if req.Options.Policy != nil {
			policy = *req.Options.Policy
		}
		if req.Options.Caller != nil {
			caller = *req.Options.Caller
		}
	}
	if policy.WorkDir == "" {
		policy.WorkDir = cwd
	}
	if len(policy.Tools) == 0 {
		policy.Tools = allowlist
	}
	if caller.SenderID == "" && req.Metadata != nil {
		caller.SenderID = req.Metadata["sender_id"]
	}

	reg = applyToolPolicy(reg, policy)
	if allowlistIncludes(policy.Tools, "ask_user_question") {
		replaced := tools.NewRegistry()
		for _, t := range reg.List() {
			if strings.EqualFold(t.Name(), "ask_user_question") {
				continue
			}
			_ = replaced.Register(t)
		}
		_ = replaced.Register(&askUserQuestionTool{
			ask: func(qCtx context.Context, question string, options []string) (string, bool, error) {
				return s.requestQuestion(qCtx, req.ID, emit, question, options)
			},
		})
		reg = replaced
	}

	var guardAgentDirs []string
	if homeDir != "" {
		guardAgentDirs = append(guardAgentDirs,
			filepath.Join(homeDir, ".gp"),
			filepath.Join(homeDir, ".agents"),
		)
	}
	if cfg != nil && len(cfg.SkillDirs) > 0 {
		guardAgentDirs = append(guardAgentDirs, cfg.SkillDirs...)
	}

	reg = wrapRegistryWithRiskGuard(reg, &riskGuard{
		prov:          prov,
		modelName:     modelName,
		userPrompt:    prompt,
		workDir:       cwd,
		agentDirs:     guardAgentDirs,
		policy:        policy,
		caller:        caller,
		sessionID:     req.SessionID,
		reqID:         req.ID,
		emit:          emit,
		cache:         s.auditCache,
		approvedCache: s.approvedCache,
		requestApproval: func(apCtx context.Context, toolName, reason string, rawArgs json.RawMessage) (bool, error) {
			return s.requestApproval(apCtx, req.ID, emit, toolName, reason, rawArgs)
		},
	})

	_ = subagent.WireRunnerWithHooks(reg, prov, modelName, cwd, subagent.Hooks{
		OnLifecycle: func(ev subagent.Event) {
			agentLabel := fmt.Sprintf("%s (%s)", ev.Role, ev.ChildID)
			switch ev.Type {
			case "start":
				emit(Event{
					ID:        req.ID,
					Event:     EventSubagentStart,
					AgentName: agentLabel,
					Text:      ev.TaskPrompt,
				})
			case "end":
				outEv := Event{
					ID:        req.ID,
					Event:     EventSubagentEnd,
					AgentName: agentLabel,
					Text:      ev.Output,
					Summary:   ev.Duration.Round(time.Millisecond).String(),
				}
				if ev.Err != nil {
					outEv.Message = ev.Err.Error()
				}
				emit(outEv)
			}
		},
	})

	ag := agent.NewAgent("agp-rpc", "assistant", prov, modelName, reg)
	ag.SetCwd(cwd)

	resLoader := resources.NewResourceLoader(cwd, homeDir)
	if cfg != nil {
		resLoader.SetExtraSkillDirs(cfg.SkillDirs)
		resLoader.SetDisabledSkills(cfg.DisabledSkills)
		resLoader.SetUnregisteredSkills(cfg.UnregisteredSkills)
	}
	if sysPrompt := resLoader.BuildSystemPrompt(cfg.SystemPrompt); sysPrompt != "" {
		ag.SetSystemPrompt(sysPrompt)
	}

	effSessionID := s.effectiveSessionID(req.SessionID)
	if store := s.ensureStore(); store != nil {
		st, err := session.NewSessionTree(effSessionID, store)
		if err != nil {
			slog.Warn("failed to load session tree", "session_id", effSessionID, "err", err)
		} else {
			// If this is a brand-new parallel sub-session (e.g. "<chat_id>:sub:<id>"),
			// seed recent context from the parent session so it doesn't lose group memory.
			if idx := strings.Index(effSessionID, ":sub:"); idx > 0 && len(st.GetLinearContext()) == 0 {
				parentID := s.effectiveSessionID(effSessionID[:idx])
				if parentTree, pErr := session.NewSessionTree(parentID, store); pErr == nil {
					parentMsgs := parentTree.GetLinearContext()
					if len(parentMsgs) > 12 {
						parentMsgs = parentMsgs[len(parentMsgs)-12:]
					}
					for _, m := range parentMsgs {
						st.AppendMessage(m)
					}
				}
			}
			ag.SetSessionTree(st)
			ag.SetStorage(store)
		}
	}

	emit(Event{ID: req.ID, Event: EventStarted, SessionID: req.SessionID})

	ag.SetStreamHandler(func(chunk ai.StreamChunk) {
		if chunk.ThinkingDelta != "" {
			emit(Event{ID: req.ID, Event: EventThinking, Text: chunk.ThinkingDelta})
		}
		if chunk.ToolCallDelta != nil && chunk.ToolCallDelta.Function.Name != "" {
			emit(Event{ID: req.ID, Event: EventToolStart, Tool: chunk.ToolCallDelta.Function.Name})
		}
		if chunk.DeltaText != "" {
			emit(Event{ID: req.ID, Event: EventDelta, Text: chunk.DeltaText})
		}
	})

	var usageMu sync.Mutex
	var turnUsage ai.TokenUsage
	ag.SetEventHandler(func(ev agent.AgentEvent) {
		switch ev.Type {
		case agent.EventToolProgress:
			emit(Event{
				ID:      req.ID,
				Event:   EventToolProgress,
				Tool:    ev.ToolName,
				Text:    ev.Result,
				Stalled: ev.Stalled,
			})
		case agent.EventUsage:
			if ev.Usage == nil {
				return
			}
			usageMu.Lock()
			turnUsage.InputTokens += ev.Usage.InputTokens
			turnUsage.OutputTokens += ev.Usage.OutputTokens
			turnUsage.CacheReadTokens += ev.Usage.CacheReadTokens
			turnUsage.TotalTokens += ev.Usage.TotalTokens
			usageMu.Unlock()
		}
	})

	ans, err := ag.Prompt(cancellableCtx, prompt)
	if err != nil {
		emit(Event{ID: req.ID, Event: EventError, Code: "EXEC_ERROR", Message: err.Error()})
		return
	}

	usageMu.Lock()
	total := turnUsage
	usageMu.Unlock()

	final := Event{
		ID:        req.ID,
		Event:     EventFinal,
		SessionID: req.SessionID,
		Text:      ans,
	}
	if total.InputTokens > 0 || total.OutputTokens > 0 {
		final.Usage = &Usage{
			InputTokens:     int(total.InputTokens),
			OutputTokens:    int(total.OutputTokens),
			CacheReadTokens: int(total.CacheReadTokens),
		}
	}
	emit(final)
}

func (s *Server) resolveApproval(approvalID string, approved bool) {
	s.mu.Lock()
	ch, ok := s.approvals[approvalID]
	s.mu.Unlock()
	if ok && ch != nil {
		select {
		case ch <- approved:
		default:
		}
	}
}

func (s *Server) requestApproval(ctx context.Context, reqID string, emit func(Event), toolName, reason string, rawArgs json.RawMessage) (bool, error) {
	id := fmt.Sprintf("apr_%d", s.approvalSeq.Add(1))
	ch := make(chan bool, 1)

	s.mu.Lock()
	s.approvals[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.approvals, id)
		s.mu.Unlock()
	}()

	var argsMap map[string]any
	_ = json.Unmarshal(rawArgs, &argsMap)

	emit(Event{
		ID:         reqID,
		Event:      EventApprovalRequest,
		ApprovalID: id,
		Tool:       toolName,
		Text:       reason,
		Args:       argsMap,
	})

	// Keep a 5s buffer over gateway's 60s approval timeout so the gateway always
	// expires first, updates the approval card UI, and sends the rejection cleanly.
	timer := time.NewTimer(65 * time.Second)
	defer timer.Stop()

	select {
	case approved := <-ch:
		return approved, nil
	case <-timer.C:
		return false, fmt.Errorf("等待管理员确认超时（65s）")
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (s *Server) resolveQuestion(questionID, answer string, skipped bool) {
	s.mu.Lock()
	ch, ok := s.questions[questionID]
	s.mu.Unlock()
	if ok && ch != nil {
		select {
		case ch <- questionReply{answer: answer, skipped: skipped}:
		default:
		}
	}
}

func (s *Server) requestQuestion(ctx context.Context, reqID string, emit func(Event), question string, options []string) (string, bool, error) {
	id := fmt.Sprintf("ask_%d", s.questionSeq.Add(1))
	ch := make(chan questionReply, 1)

	s.mu.Lock()
	s.questions[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.questions, id)
		s.mu.Unlock()
	}()

	emit(Event{
		ID:         reqID,
		Event:      EventQuestionRequest,
		QuestionID: id,
		Question:   question,
		Options:    options,
	})

	timer := time.NewTimer(90 * time.Second)
	defer timer.Stop()

	select {
	case rep := <-ch:
		return rep.answer, rep.skipped, nil
	case <-timer.C:
		return "", true, nil
	case <-ctx.Done():
		return "", true, ctx.Err()
	}
}

func effectiveBashMode(policy ExecutionPolicy) string {
	mode := strings.ToLower(strings.TrimSpace(policy.BashMode))
	if mode != "" {
		return mode
	}
	if policy.AllowBash != nil && !*policy.AllowBash {
		return "disabled"
	}
	return "full"
}

func applyToolPolicy(reg *tools.Registry, policy ExecutionPolicy) *tools.Registry {
	if reg == nil {
		return nil
	}
	bashDisabled := effectiveBashMode(policy) == "disabled"
	writeDisabled := policy.AllowWrite != nil && !*policy.AllowWrite

	var mcpAllowed map[string]struct{}
	hasMCPRule := false
	for _, c := range policy.Capabilities {
		if strings.EqualFold(strings.TrimSpace(c.Kind), "mcp") {
			hasMCPRule = true
			if mcpAllowed == nil {
				mcpAllowed = make(map[string]struct{})
			}
			for _, a := range c.Allow {
				if key := strings.ToLower(strings.TrimSpace(a)); key != "" {
					mcpAllowed[key] = struct{}{}
				}
			}
		}
	}

	if !bashDisabled && !writeDisabled && !hasMCPRule {
		return reg
	}
	filtered := tools.NewRegistry()
	for _, t := range reg.List() {
		lower := strings.ToLower(t.Name())
		if bashDisabled && lower == "bash" {
			continue
		}
		if writeDisabled && (lower == "write" || lower == "edit") {
			continue
		}
		if hasMCPRule && strings.HasPrefix(lower, "mcp__") {
			matched := false
			for pat := range mcpAllowed {
				if strings.HasSuffix(pat, "*") {
					if strings.HasPrefix(lower, strings.TrimSuffix(pat, "*")) {
						matched = true
						break
					}
				} else if lower == pat {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		_ = filtered.Register(t)
	}
	return filtered
}

func allowlistIncludes(allowlist []string, target string) bool {
	if len(allowlist) == 0 {
		return true
	}
	for _, item := range allowlist {
		if strings.EqualFold(strings.TrimSpace(item), target) {
			return true
		}
	}
	return false
}

type askUserQuestionTool struct {
	ask func(ctx context.Context, question string, options []string) (string, bool, error)
}

func (t *askUserQuestionTool) Name() string { return "ask_user_question" }

func (t *askUserQuestionTool) Description() string {
	return "向当前用户发起带可选按钮的交互式询问（用户也可直接发文字或跳过），用于在关键分支需要用户确认或选择时收集答案。"
}

func (t *askUserQuestionTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"question": {"type": "string", "description": "需要向用户确认或提问的清晰问题"},
			"options": {"type": "array", "items": {"type": "string"}, "description": "可选的快捷选项列表（最多 5 个）"}
		},
		"required": ["question"]
	}`)
}

func (t *askUserQuestionTool) ReplayMode() tools.ReplayMode { return tools.ReplaySafe }

func (t *askUserQuestionTool) Execute(ctx *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
	var args struct {
		Question  string   `json:"question"`
		Options   []string `json:"options"`
		Questions []struct {
			Question string `json:"question"`
			Options  []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return &tools.ToolResult{Content: fmt.Sprintf("参数解析失败: %v", err), IsError: true}, nil
	}
	q := strings.TrimSpace(args.Question)
	opts := args.Options
	if q == "" && len(args.Questions) > 0 {
		q = strings.TrimSpace(args.Questions[0].Question)
		for _, o := range args.Questions[0].Options {
			if lbl := strings.TrimSpace(o.Label); lbl != "" {
				opts = append(opts, lbl)
			}
		}
	}
	if q == "" {
		return &tools.ToolResult{Content: "缺少必填参数 question", IsError: true}, nil
	}

	parentCtx := context.Background()
	if ctx != nil && ctx.Context != nil {
		parentCtx = ctx.Context
	}
	ans, skipped, err := t.ask(parentCtx, q, opts)
	if err != nil {
		return nil, err
	}
	if skipped || strings.TrimSpace(ans) == "" {
		return &tools.ToolResult{
			Content: "用户选择跳过或 90s 未回复，请按最合理的默认方案继续执行或向用户说明。",
		}, nil
	}
	return &tools.ToolResult{
		Content: fmt.Sprintf("用户回复：%s", strings.TrimSpace(ans)),
	}, nil
}

func (s *Server) resolveRegistry(cwd, homeDir string, cfg *settings.Settings, allowlist []string) (*tools.Registry, error) {
	reg, err := builtin.NewDefaultRegistry()
	if err != nil {
		return nil, fmt.Errorf("initialize builtin tools: %w", err)
	}

	if len(allowlist) == 0 && cfg != nil && len(cfg.DefaultTools) > 0 {
		reg = reg.ScopedRegistry(cfg.DefaultTools)
	}

	_, _ = mcp.AutoConnectAndRegister(context.Background(), cwd, homeDir, reg)
	_, _ = js.LoadExtensions(cwd, homeDir, reg)

	if len(allowlist) > 0 {
		reg = reg.ScopedRegistry(allowlist)
	}
	return reg, nil
}

func (s *Server) resolveSettings(cwd string) *settings.Settings {
	if s.opts.Settings != nil {
		c := s.opts.Settings.Clone()
		return &c
	}
	mgr := s.opts.SettingsMgr
	if mgr == nil {
		mgr = settings.NewManager(cwd)
	}
	if cfg, err := mgr.Load(); err == nil {
		return cfg
	}
	d := settings.DefaultSettings()
	return &d
}

func (s *Server) resolveModelName(cfg *settings.Settings, opts *RunOptions) string {
	if opts != nil && opts.Model != "" {
		return opts.Model
	}
	if s.opts.ModelName != "" {
		return s.opts.ModelName
	}
	if cfg != nil && cfg.Model != "" {
		return cfg.Model
	}
	return settings.DefaultModel
}

func (s *Server) resolveBaseURL(cfg *settings.Settings) string {
	if s.opts.BaseURL != "" {
		return s.opts.BaseURL
	}
	if v := os.Getenv("OPENAI_BASE_URL"); v != "" {
		return v
	}
	if cfg != nil && cfg.BaseURL != "" {
		return cfg.BaseURL
	}
	return ""
}

func (s *Server) resolveAPIKey(cfg *settings.Settings) string {
	if s.opts.APIKey != "" {
		return s.opts.APIKey
	}
	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		return v
	}
	if cfg != nil && cfg.APIKey != "" {
		return cfg.APIKey
	}
	return ""
}
