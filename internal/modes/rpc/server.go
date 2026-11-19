package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
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

// Server serves AGP (Agent Gateway Protocol) requests over stdio or unix/tcp sockets.
type Server struct {
	opts       RunnerOptions
	store      storage.Storage
	ownStore   bool
	mu         sync.Mutex
	sessionGen map[string]int
	cancels    map[string]context.CancelFunc
}

// NewServer creates a new AGP RPC Server.
func NewServer(opts RunnerOptions) *Server {
	return &Server{
		opts:       opts,
		sessionGen: make(map[string]int),
		cancels:    make(map[string]context.CancelFunc),
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
			s.sessionGen[req.SessionID]++
		}
		s.mu.Unlock()
		emit(Event{ID: req.ID, Event: EventResetAck, SessionID: req.SessionID})
	case OpAbort:
		s.mu.Lock()
		cancel, ok := s.cancels[req.TargetID]
		s.mu.Unlock()
		if ok && cancel != nil {
			cancel()
		}
		emit(Event{ID: req.ID, Event: EventAbortAck})
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

func (s *Server) effectiveSessionID(sessionID string) string {
	if sessionID == "" {
		return fmt.Sprintf("rpc_%d", time.Now().UnixNano())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	gen := s.sessionGen[sessionID]
	if gen == 0 {
		return sessionID
	}
	return fmt.Sprintf("%s_g%d", sessionID, gen)
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
	if req.Options != nil && len(req.Options.Tools) > 0 {
		allowlist = req.Options.Tools
	} else {
		allowlist = s.opts.ToolAllowlist
	}

	reg, err := s.resolveRegistry(cwd, homeDir, cfg, allowlist)
	if err != nil {
		emit(Event{ID: req.ID, Event: EventError, Code: "REGISTRY_ERROR", Message: err.Error()})
		return
	}

	reg = wrapRegistryWithRiskGuard(reg, &riskGuard{
		prov:       prov,
		modelName:  modelName,
		userPrompt: prompt,
		reqID:      req.ID,
		emit:       emit,
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
		if ev.Type != agent.EventUsage || ev.Usage == nil {
			return
		}
		usageMu.Lock()
		turnUsage.InputTokens += ev.Usage.InputTokens
		turnUsage.OutputTokens += ev.Usage.OutputTokens
		turnUsage.CacheReadTokens += ev.Usage.CacheReadTokens
		turnUsage.TotalTokens += ev.Usage.TotalTokens
		usageMu.Unlock()
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
