package interactive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
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
	"gp/internal/extensions"
	"gp/internal/extensions/js"
	"gp/internal/mcp"
	"gp/internal/modes/subagent"
	"gp/internal/tools"
	"gp/internal/tools/builtin"
	"gp/internal/tui"

	tea "github.com/charmbracelet/bubbletea"
)

const defaultModel = settings.DefaultModel

// RunnerOptions 配置 Interactive TUI 模式的参数
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
	InitialPrompt string
	Headless      bool // 支持单元测试在无 TTY 终端下非阻塞验证流水线
}

// Runner Interactive 模式运行器
type Runner struct {
	opts RunnerOptions
}

// NewRunner 创建 Interactive 模式运行器
func NewRunner(opts RunnerOptions) *Runner {
	return &Runner{opts: opts}
}

// Run 启动 Interactive 全屏 TUI 模式
func (r *Runner) Run(ctx context.Context, initialPrompt string) error {
	prompt := strings.TrimSpace(initialPrompt)
	if prompt == "" && r.opts.InitialPrompt != "" {
		prompt = strings.TrimSpace(r.opts.InitialPrompt)
	}

	cwd := r.opts.Cwd
	if cwd == "" {
		if dir, err := os.Getwd(); err == nil {
			cwd = dir
		} else {
			cwd = "."
		}
	}

	cfg := r.resolveSettings(cwd)

	homeDir, _ := os.UserHomeDir()
	authStore := auth.NewStore(homeDir)
	go func() {
		for _, spec := range auth.RegisteredProviders() {
			_, _ = authStore.RefreshProviderModels(spec.ID)
		}
	}()

	modelName := r.resolveModelName(cfg)
	baseURL := r.resolveBaseURL(cfg)
	apiKey := r.resolveAPIKey(cfg)

	inferredProv := authStore.ResolveProviderForModel(modelName)
	if inferredProv != "" {
		explicitKey := r.opts.APIKey != "" || (cfg != nil && cfg.APIKey != "")
		if apiKey == "" || !explicitKey {
			if k, u, _, ok := authStore.ResolveCredential(inferredProv); ok {
				apiKey = k
				if u != "" {
					baseURL = u
				}
			}
		}
	}

	prov := r.opts.Provider
	if prov == nil {
		prov = ai.CreateProviderFromPlugins(inferredProv, apiKey, baseURL, modelName)
		if prov == nil {
			prov = openai.NewProvider(apiKey, baseURL)
		}
	}

	reg, activeClients, err := r.resolveRegistryWithClients(cwd, cfg)
	if err != nil {
		return fmt.Errorf("resolve registry: %w", err)
	}
	defer func() {
		for _, c := range activeClients {
			_ = c.Close()
		}
	}()

	var p *tea.Program

	// 接线真实子 Agent 执行器，并将子 Agent 状态、流式输出与数据流实时转发到 TUI
	if err := subagent.WireRunnerWithHooks(reg, prov, modelName, cwd, subagent.Hooks{
		OnLifecycle: func(ev subagent.Event) {
			if p == nil {
				return
			}
			if ev.Type == "start" {
				p.Send(tui.AgentFlowMsg{
					ParentID:  tui.PrimaryAgentID,
					ChildID:   ev.ChildID,
					Role:      ev.Role,
					Direction: "dispatch",
					Payload:   ev.TaskPrompt,
				})
			} else {
				payload := ev.Output
				isErr := ev.Err != nil
				if isErr {
					payload = ev.Err.Error()
				}
				p.Send(tui.AgentFlowMsg{
					ParentID:  tui.PrimaryAgentID,
					ChildID:   ev.ChildID,
					Role:      ev.Role,
					Direction: "return",
					Payload:   payload,
					IsError:   isErr,
					Duration:  ev.Duration,
				})
			}
		},
		OnStream: func(agentID string, chunk ai.StreamChunk) {
			if p == nil {
				return
			}
			if chunk.ThinkingDelta != "" {
				p.Send(tui.AgentThinkingDeltaMsg{
					AgentID:       agentID,
					ThinkingDelta: chunk.ThinkingDelta,
				})
			}
			if chunk.DeltaText != "" {
				p.Send(tui.AgentStreamDeltaMsg{
					AgentID:   agentID,
					DeltaText: chunk.DeltaText,
				})
			}
		},
		OnAgentEvent: func(ev agent.AgentEvent) {
			if p == nil {
				return
			}
			switch ev.Type {
			case agent.EventTurnStart:
				p.Send(tui.TurnStartMsg{AgentID: ev.AgentID, Turn: ev.Turn})
			case agent.EventTurnEnd:
				p.Send(tui.TurnEndMsg{AgentID: ev.AgentID, Turn: ev.Turn, Outcome: ev.Outcome, Duration: ev.Duration})
			case agent.EventToolCallStart:
				p.Send(tui.ToolCallStartMsg{AgentID: ev.AgentID, CallID: ev.CallID, ToolName: ev.ToolName, ArgsRaw: string(ev.Args)})
			case agent.EventToolCallEnd:
				p.Send(tui.ToolCallEndMsg{AgentID: ev.AgentID, CallID: ev.CallID, ToolName: ev.ToolName, ArgsRaw: string(ev.Args), Result: ev.Result, IsError: ev.IsError, Duration: ev.Duration})
			case agent.EventUsage:
				if ev.Usage != nil {
					p.Send(tui.TokenUsageMsg{InputTokens: ev.Usage.InputTokens, OutputTokens: ev.Usage.OutputTokens, TotalTokens: ev.Usage.TotalTokens})
				}
			}
		},
	}); err != nil {
		_ = err
	}

	store := r.opts.Storage
	var shouldCloseStore bool
	if store == nil && r.opts.DBPath != "" {
		if s, err := sqlite.Open(r.opts.DBPath); err == nil {
			store = s
			shouldCloseStore = true
		}
	}
	if store != nil && shouldCloseStore {
		defer store.Close()
	}

	sessID := fmt.Sprintf("sess_%d", time.Now().UnixNano()%1000000)

	ag := agent.NewAgent(tui.PrimaryAgentID, "orchestrator", prov, modelName, reg)
	ag.SetCwd(cwd)
	if cfg != nil && cfg.ThinkingLevel != "" {
		ag.SetThinkingLevel(cfg.ThinkingLevel)
	}

	resLoader := resources.NewResourceLoader(cwd, homeDir)
	if cfg != nil {
		resLoader.SetExtraSkillDirs(cfg.SkillDirs)
		resLoader.SetDisabledSkills(cfg.DisabledSkills)
		resLoader.SetUnregisteredSkills(cfg.UnregisteredSkills)
	}

	// 初始化统一外部能力注册中心（Extension / ToolPack）：统一托管 extension.yaml 与懒加载 MCP 桥接
	buildExtLoadOptions := func() extensions.LoadOptions {
		var legacyMCPCfg *mcp.Config
		if !cfg.IsLegacyExtensionMode() {
			legacyMCPCfg, _ = mcp.DiscoverConfig(cwd, homeDir)
		}
		var extDirs, disabledExts, disabledMCPsCfg []string
		if cfg != nil {
			extDirs = append([]string(nil), cfg.ExtensionDirs...)
			disabledExts = append([]string(nil), cfg.DisabledExtensions...)
			disabledMCPsCfg = append([]string(nil), cfg.DisabledMCPs...)
		}
		return extensions.LoadOptions{
			Cwd:                cwd,
			HomeDir:            homeDir,
			ExtraDirs:          extDirs,
			DisabledExtensions: disabledExts,
			BridgeLegacyMCP:    legacyMCPCfg,
			DisabledMCPs:       disabledMCPsCfg,
		}
	}

	allowDestructive := cfg != nil && cfg.ExtensionAllowDestructive
	extReg := extensions.NewRegistry(extensions.RegistryConfig{
		Load:             buildExtLoadOptions(),
		AuthStore:        authStore,
		AllowDestructive: allowDestructive,
		ConfirmFn: func(confirmCtx context.Context, extName, actionName string, args map[string]any) (bool, error) {
			respCh := make(chan tui.AskUserResult, 1)
			argsHint := ""
			if len(args) > 0 {
				var parts []string
				for k, v := range args {
					parts = append(parts, fmt.Sprintf("%s=%v", k, v))
				}
				slices.Sort(parts)
				joined := strings.Join(parts, ", ")
				if len(joined) > 160 {
					joined = joined[:160] + "…"
				}
				argsHint = " (" + joined + ")"
			}
			sent := tools.SendUIEvent(tui.AskUserQuestionMsg{
				Questions: []tui.AskUserQuestion{
					{
						Header:   "Destructive Ext",
						Question: fmt.Sprintf("Allow destructive extension action %s.%s%s?", extName, actionName, argsHint),
						Options: []tui.AskUserOption{
							{Label: "Deny", Description: "Block this destructive action (safe default)"},
							{Label: "Allow", Description: fmt.Sprintf("Proceed executing %s.%s", extName, actionName)},
						},
					},
				},
				ResponseCh: respCh,
			})
			if !sent {
				return false, nil
			}
			select {
			case res := <-respCh:
				if res.Cancelled {
					return false, nil
				}
				for _, ans := range res.Answers {
					if ans.Answer == "Allow" || slices.Contains(ans.Selected, "Allow") {
						return true, nil
					}
				}
				return false, nil
			case <-confirmCtx.Done():
				return false, confirmCtx.Err()
			}
		},
	})
	defer extReg.CloseAll()

	allowExtDescribe := len(r.opts.ToolAllowlist) == 0 || slices.Contains(r.opts.ToolAllowlist, "ext_describe")
	allowExtCall := len(r.opts.ToolAllowlist) == 0 || slices.Contains(r.opts.ToolAllowlist, "ext_call")
	if len(extReg.ListStatus()) > 0 {
		_ = extensions.RegisterFilteredTools(reg, extReg, allowExtDescribe, allowExtCall)
	}

	rebuildSystemPrompt := func() {
		parts := []string{}
		if base := strings.TrimSpace(resLoader.BuildSystemPrompt(cfg.SystemPrompt)); base != "" {
			parts = append(parts, base)
		}
		if extDir := strings.TrimSpace(extReg.RenderDirectoryPrompt()); extDir != "" {
			parts = append(parts, extDir)
		}
		ag.SetSystemPrompt(strings.Join(parts, "\n\n"))
	}
	rebuildSystemPrompt()
	if store != nil {
		st, err := session.NewSessionTree(sessID, store)
		if err == nil {
			ag.SetSessionTree(st)
			ag.SetStorage(store)
		}
	}

	providerName := ""
	if prov != nil {
		providerName = prov.Name()
	}
	thinkingLevel := ""
	if cfg != nil {
		thinkingLevel = cfg.ThinkingLevel
	}
	header := tui.HeaderInfo{
		AppName:       "gp",
		Version:       "v0.1.0-alpha",
		ModelName:     modelName,
		ProviderName:  providerName,
		ThinkingLevel: thinkingLevel,
		SessionID:     sessID,
		TokenUsage:    "0 tok",
		Cwd:           cwd,
		BaseURL:       baseURL,
		APIKey:        apiKey,
	}

	ag.SetEventHandler(func(ev agent.AgentEvent) {
		if p == nil {
			return
		}
		switch ev.Type {
		case agent.EventTurnStart:
			p.Send(tui.TurnStartMsg{
				AgentID: ev.AgentID,
				Turn:    ev.Turn,
			})
		case agent.EventTurnEnd:
			p.Send(tui.TurnEndMsg{
				AgentID:  ev.AgentID,
				Turn:     ev.Turn,
				Outcome:  ev.Outcome,
				Duration: ev.Duration,
			})
		case agent.EventToolCallStart:
			p.Send(tui.ToolCallStartMsg{
				AgentID:  ev.AgentID,
				CallID:   ev.CallID,
				ToolName: ev.ToolName,
				ArgsRaw:  string(ev.Args),
			})
		case agent.EventToolCallEnd:
			p.Send(tui.ToolCallEndMsg{
				AgentID:  ev.AgentID,
				CallID:   ev.CallID,
				ToolName: ev.ToolName,
				ArgsRaw:  string(ev.Args),
				Result:   ev.Result,
				IsError:  ev.IsError,
				Duration: ev.Duration,
			})
		case agent.EventUsage:
			if ev.Usage != nil {
				p.Send(tui.TokenUsageMsg{
					InputTokens:  ev.Usage.InputTokens,
					OutputTokens: ev.Usage.OutputTokens,
					TotalTokens:  ev.Usage.TotalTokens,
				})
			}
		case agent.EventContextMetrics:
			msg := tui.ContextMetricsMsg{
				HasCompactionCount: true,
				CompactionCount:    ev.CompactionCount,
			}
			if ev.ContextMetric != nil {
				cm := ev.ContextMetric
				msg.TurnIndex = cm.TurnIndex
				msg.StrategyMode = cm.StrategyMode
				msg.EpochID = cm.EpochID
				msg.EpochMaskedCount = cm.EpochMaskedCount
				msg.LedgerVersion = cm.LedgerVersion
				msg.LedgerActiveCnt = cm.LedgerActiveCnt
				msg.LedgerSuperseded = cm.LedgerSuperseded
				msg.WorkingSetTokens = cm.WorkingSetTokens
				msg.WorkingSetEntries = cm.WorkingSetEntries
				msg.GraphMatchedEntities = cm.GraphMatchedEntities
				msg.ContextBuildMs = cm.ContextBuildMs
				msg.RetrievalBuildMs = cm.RetrievalBuildMs
				msg.InputTokens = cm.InputTokens
				msg.CacheReadTokens = cm.CacheReadTokens
				msg.CacheWriteTokens = cm.CacheWriteTokens
				msg.OutputTokens = cm.OutputTokens
				msg.RecallCalls = cm.RecallCalls
				msg.RestoreCalls = cm.RestoreCalls
				msg.SearchCalls = cm.SearchCalls
			}
			if ev.ContextTotals != nil {
				msg.HasTotals = true
				msg.TotalRecallCalls = ev.ContextTotals.TotalRecallCalls
				msg.TotalRestoreCalls = ev.ContextTotals.TotalRestoreCalls
				msg.TotalSearchCalls = ev.ContextTotals.TotalSearchCalls
			}
			p.Send(msg)
		}
	})

	ag.SetStreamHandler(func(chunk ai.StreamChunk) {
		if p == nil {
			return
		}
		if chunk.ThinkingDelta != "" {
			p.Send(tui.AgentThinkingDeltaMsg{
				AgentID:       tui.PrimaryAgentID,
				ThinkingDelta: chunk.ThinkingDelta,
			})
		}
		if chunk.DeltaText != "" {
			p.Send(tui.AgentStreamDeltaMsg{
				AgentID:   tui.PrimaryAgentID,
				DeltaText: chunk.DeltaText,
			})
		}
	})

	var isRunning atomic.Bool
	var cancelMu sync.Mutex
	var activeCancel context.CancelFunc
	uiModel := tui.NewMainUIModel(header, func(userPrompt string) {
		if !isRunning.CompareAndSwap(false, true) {
			if p != nil {
				p.Send(tui.ErrorMsg{Err: fmt.Errorf("当前任务仍在执行中，请稍候再提交")})
			}
			return
		}
		turnCtx, cancel := context.WithCancel(ctx)
		cancelMu.Lock()
		activeCancel = cancel
		cancelMu.Unlock()
		go func() {
			if p != nil {
				p.Send(tui.ActiveCoroutineMsg{
					AgentID:  tui.PrimaryAgentID,
					TaskDesc: "Simmering…",
					Active:   true,
				})
			}
			slog.Debug("turn start", "component", "TURN", "prompt_len", len(userPrompt))
			_, promptErr := ag.Prompt(turnCtx, userPrompt)
			slog.Debug("turn end", "component", "TURN", "err", promptErr)
			cancel()
			cancelMu.Lock()
			activeCancel = nil
			cancelMu.Unlock()
			isRunning.Store(false)
			if p != nil {
				p.Send(tui.ActiveCoroutineMsg{
					AgentID: tui.PrimaryAgentID,
					Active:  false,
				})
				if promptErr != nil {
					if errors.Is(promptErr, context.Canceled) {
						p.Send(tui.TurnEndMsg{AgentID: tui.PrimaryAgentID, Outcome: "interrupted"})
					} else {
						p.Send(tui.TurnEndMsg{AgentID: tui.PrimaryAgentID, Outcome: "error"})
						p.Send(tui.ErrorMsg{Err: promptErr})
					}
				}
			}
		}()
	})
	uiModel.SetCancelHandler(func() {
		cancelMu.Lock()
		cancel := activeCancel
		cancelMu.Unlock()
		if cancel != nil {
			slog.Debug("cancel_requested", "component", "TURN")
			cancel()
		}
	})
	uiModel.SetCompactHandler(func(focus string) (int64, int64, error) {
		res, err := ag.Compact(ctx, focus)
		if err != nil {
			return 0, 0, err
		}
		return res.BeforeTokens, res.AfterTokens, nil
	})
	uiModel.SetResourceHandlers(tui.ResourceHandlers{
		ListSkills: func() ([]tui.SkillStatusItem, error) {
			skills, err := resLoader.LoadSkills()
			if err != nil {
				return nil, err
			}
			out := make([]tui.SkillStatusItem, 0, len(skills))
			for _, s := range skills {
				out = append(out, tui.SkillStatusItem{
					Name:        s.Name,
					Description: s.Description,
					Source:      s.Source,
					Path:        s.Path,
					Enabled:     s.Enabled,
				})
			}
			return out, nil
		},
		ToggleSkill: func(name string, enable bool) error {
			cfg.DisabledSkills = updateDisabledList(cfg.DisabledSkills, name, enable)
			if enable {
				cfg.UnregisteredSkills = updateDisabledList(cfg.UnregisteredSkills, name, true)
				resLoader.SetUnregisteredSkills(cfg.UnregisteredSkills)
			}
			resLoader.SetDisabledSkills(cfg.DisabledSkills)
			rebuildSystemPrompt()
			return settings.NewManager(cwd).SaveGlobal(*cfg)
		},
		AddSkillDir: func(dir string) error {
			if !slices.Contains(cfg.SkillDirs, dir) {
				cfg.SkillDirs = append(cfg.SkillDirs, dir)
			}
			resLoader.SetExtraSkillDirs(cfg.SkillDirs)
			rebuildSystemPrompt()
			return settings.NewManager(cwd).SaveGlobal(*cfg)
		},
		UnregisterSkill: func(name string) error {
			// 若该 Skill 来自自定义 SkillDirs，先移除对应目录；并记录到 UnregisteredSkills
			if skills, err := resLoader.LoadSkills(); err == nil {
				for _, s := range skills {
					if strings.EqualFold(s.Name, strings.TrimSpace(name)) {
						var keptDirs []string
						for _, d := range cfg.SkillDirs {
							if !strings.HasPrefix(s.Path, d) {
								keptDirs = append(keptDirs, d)
							}
						}
						cfg.SkillDirs = keptDirs
						resLoader.SetExtraSkillDirs(cfg.SkillDirs)
						break
					}
				}
			}
			cfg.UnregisteredSkills = updateDisabledList(cfg.UnregisteredSkills, name, false)
			cfg.DisabledSkills = updateDisabledList(cfg.DisabledSkills, name, true)
			resLoader.SetUnregisteredSkills(cfg.UnregisteredSkills)
			resLoader.SetDisabledSkills(cfg.DisabledSkills)
			rebuildSystemPrompt()
			return settings.NewManager(cwd).SaveGlobal(*cfg)
		},
		ListMCPs: func() ([]tui.MCPStatusItem, error) {
			statuses, err := mcp.InspectServers(cwd, homeDir, cfg.DisabledMCPs, activeClients)
			if err != nil {
				return nil, err
			}
			out := make([]tui.MCPStatusItem, 0, len(statuses))
			for _, st := range statuses {
				out = append(out, tui.MCPStatusItem{
					Name:      st.Name,
					Command:   st.Command,
					Args:      st.Args,
					Source:    st.Source,
					Enabled:   st.Enabled,
					Connected: st.Connected,
					ToolCount: st.ToolCount,
				})
			}
			return out, nil
		},
		ToggleMCP: func(name string, enable bool) error {
			cfg.DisabledMCPs = updateDisabledList(cfg.DisabledMCPs, name, enable)
			if !cfg.IsLegacyExtensionMode() {
				extReg.ReloadWithOptions(buildExtLoadOptions())
				rebuildSystemPrompt()
			}
			return saveGlobalSettingsField(r.opts.SettingsMgr, cwd, func(g *settings.Settings) {
				g.DisabledMCPs = append([]string(nil), cfg.DisabledMCPs...)
			})
		},
		UnregisterMCP: func(name string) error {
			if err := mcp.RemoveServerConfig(cwd, homeDir, name); err != nil {
				return err
			}
			cfg.DisabledMCPs = updateDisabledList(cfg.DisabledMCPs, name, true)
			for _, c := range activeClients {
				if c != nil && strings.EqualFold(c.ServerName, strings.TrimSpace(name)) {
					_ = c.Close()
					c.ServerName = ""
				}
			}
			if !cfg.IsLegacyExtensionMode() {
				extReg.ReloadWithOptions(buildExtLoadOptions())
				rebuildSystemPrompt()
			}
			return saveGlobalSettingsField(r.opts.SettingsMgr, cwd, func(g *settings.Settings) {
				g.DisabledMCPs = append([]string(nil), cfg.DisabledMCPs...)
			})
		},
		ListExtensions: func() ([]tui.ExtensionStatusItem, error) {
			return toTUIExtensions(extReg.ListStatus()), nil
		},
		ToggleExtension: func(name string, enable bool) error {
			if !extReg.SetEnabled(name, enable) {
				return fmt.Errorf("extension %q not found or has manifest error", name)
			}
			cfg.DisabledExtensions = updateDisabledList(cfg.DisabledExtensions, name, enable)
			rebuildSystemPrompt()
			return saveGlobalSettingsField(r.opts.SettingsMgr, cwd, func(g *settings.Settings) {
				g.DisabledExtensions = append([]string(nil), cfg.DisabledExtensions...)
			})
		},
		ReloadExtensions: func() ([]tui.ExtensionStatusItem, error) {
			extReg.ReloadWithOptions(buildExtLoadOptions())
			if len(extReg.ListStatus()) > 0 {
				_ = extensions.RegisterFilteredTools(reg, extReg, allowExtDescribe, allowExtCall)
			}
			rebuildSystemPrompt()
			return toTUIExtensions(extReg.ListStatus()), nil
		},
		TestExtension: func(name string) error {
			tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return extReg.HealthCheck(tctx, name)
		},
	})

	uiModel.RegisterStatusSegment(tui.StatusSegment{
		ID:          "ext",
		Description: "已启用的外部工具能力包数量 (ext:N)",
		Side:        tui.StatusSideRight,
		Priority:    42,
		Order:       65,
		Enabled:     true,
		Render: func(_ tui.FooterSnapshot, t tui.Theme, _ tui.UIConfig) string {
			cnt := extReg.EnabledCount()
			if cnt <= 0 {
				return ""
			}
			return t.Fg(t.SyntaxOperator, fmt.Sprintf("ext:%d", cnt))
		},
	})

	// Headless 测试模式直接返回，避免在无终端环境阻塞 Program.Run
	if r.opts.Headless {
		if prompt != "" {
			_, err := ag.Prompt(ctx, prompt)
			return err
		}
		return nil
	}

	uiModel.SetAuthStore(authStore)
	if store != nil {
		uiModel.SetSessionHandlers(
			func() ([]storage.SessionSummary, error) {
				if lister, ok := store.(storage.SessionLister); ok {
					return lister.ListSessions(50)
				}
				return nil, fmt.Errorf("current storage backend does not support listing sessions")
			},
			func(sessionID string) ([]ai.Message, error) {
				st, err := session.NewSessionTree(sessionID, store)
				if err != nil {
					return nil, err
				}
				ag.SetSessionTree(st)
				// 从 SQLite & SessionTree 回填历史会话的上下文指标（累计压缩次数、累计召回/检索次数、最近轮次快照）
				if f := uiModel.Footer(); f != nil {
					syncMsg := tui.ContextMetricsMsg{
						HasCompactionCount: true,
						CompactionCount:    st.CompactionCount(),
					}
					if metricStore, ok := store.(storage.ContextMetricStore); ok {
						if totals, tErr := metricStore.GetContextMetricTotals(sessionID); tErr == nil {
							syncMsg.HasTotals = true
							syncMsg.TotalRecallCalls = totals.TotalRecallCalls
							syncMsg.TotalRestoreCalls = totals.TotalRestoreCalls
							syncMsg.TotalSearchCalls = totals.TotalSearchCalls
						}
						if recent, lErr := metricStore.ListContextMetrics(sessionID, 1); lErr == nil && len(recent) > 0 {
							cm := recent[len(recent)-1]
							syncMsg.TurnIndex = cm.TurnIndex
							syncMsg.StrategyMode = cm.StrategyMode
							syncMsg.EpochID = cm.EpochID
							syncMsg.EpochMaskedCount = cm.EpochMaskedCount
							syncMsg.LedgerVersion = cm.LedgerVersion
							syncMsg.LedgerActiveCnt = cm.LedgerActiveCnt
							syncMsg.LedgerSuperseded = cm.LedgerSuperseded
							syncMsg.WorkingSetTokens = cm.WorkingSetTokens
							syncMsg.WorkingSetEntries = cm.WorkingSetEntries
							syncMsg.GraphMatchedEntities = cm.GraphMatchedEntities
							syncMsg.ContextBuildMs = cm.ContextBuildMs
							syncMsg.RetrievalBuildMs = cm.RetrievalBuildMs
							syncMsg.InputTokens = cm.InputTokens
							syncMsg.CacheReadTokens = cm.CacheReadTokens
							syncMsg.CacheWriteTokens = cm.CacheWriteTokens
							syncMsg.OutputTokens = cm.OutputTokens
						}
					}
					f.ApplyContextMetrics(syncMsg)
				}
				return st.GetLinearContext(), nil
			},
		)
	}
	uiModel.SetThinkingChangeHandler(func(level string) error {
		ag.SetThinkingLevel(level)
		if cfg != nil {
			cfg.ThinkingLevel = level
		}
		return nil
	})
	activeProvID := auth.NormalizeProviderID(inferredProv)
	activeModel := modelName
	activeKey := apiKey
	activeURL := baseURL

	uiModel.SetAuthChangeHandler(func(providerID string) {
		norm := auth.NormalizeProviderID(providerID)
		if norm == "" || (activeProvID != "" && norm != activeProvID) {
			return
		}
		if newKey, newURL, _, ok := authStore.ResolveCredential(norm); ok {
			if newURL == "" {
				newURL = baseURL
			}
			if p := ai.CreateProviderFromPlugins(norm, newKey, newURL, activeModel); p != nil {
				ag.SetProvider(p)
			} else if newKey != "" || newURL != "" {
				ag.SetProvider(openai.NewProvider(newKey, newURL))
			}
			activeProvID = norm
			activeKey = newKey
			activeURL = newURL
		}
	})

	uiModel.SetModelChangeHandler(func(entry auth.ModelEntry, persist bool) error {
		targetProv := auth.NormalizeProviderID(entry.Provider)
		newKey, newURL, _, ok := authStore.ResolveCredential(entry.Provider)
		if !ok {
			if apiKey != "" && (entry.BaseURL == "" || entry.BaseURL == baseURL) {
				newKey = apiKey
				newURL = baseURL
			} else if entry.Provider != "" && entry.Provider != "custom" {
				return fmt.Errorf("未登录 %s，请先执行 /login %s", entry.Provider, entry.Provider)
			}
		}
		if newURL == "" {
			newURL = entry.BaseURL
		}
		if newURL == "" {
			newURL = baseURL
		}
		ag.SetModelName(entry.ID)
		activeModel = entry.ID
		// 同 Provider 且凭证未变时复用已有 Provider 实例，保留已缓存的 jobToken / 连接池，避免反复触发 PAT 兑换
		sameProvider := targetProv != "" && targetProv == activeProvID && newKey == activeKey && newURL == activeURL
		if !sameProvider {
			if p := ai.CreateProviderFromPlugins(entry.Provider, newKey, newURL, entry.ID); p != nil {
				ag.SetProvider(p)
			} else if newKey != "" || newURL != "" {
				ag.SetProvider(openai.NewProvider(newKey, newURL))
			}
			activeProvID = targetProv
			activeKey = newKey
			activeURL = newURL
		}
		if persist {
			mgr := r.opts.SettingsMgr
			if mgr == nil {
				mgr = settings.NewManager(cwd)
			}
			var globalCfg settings.Settings
			if loaded, err := mgr.Load(); err == nil && loaded != nil {
				globalCfg = loaded.Clone()
			} else if cfg != nil {
				globalCfg = cfg.Clone()
			} else {
				globalCfg = settings.DefaultSettings()
			}
			globalCfg.Model = entry.ID
			if cfg != nil && cfg.ThinkingLevel != "" {
				globalCfg.ThinkingLevel = cfg.ThinkingLevel
			}
			if err := mgr.SaveGlobal(globalCfg); err != nil {
				return fmt.Errorf("save default model: %w", err)
			}
		}
		return nil
	})

	var toolNames []string
	for _, t := range reg.List() {
		toolNames = append(toolNames, t.Name())
	}
	var skillItems []tui.SkillStatusItem
	if skills, err := resLoader.LoadSkills(); err == nil {
		for _, s := range skills {
			skillItems = append(skillItems, tui.SkillStatusItem{Name: s.Name, Enabled: s.Enabled})
		}
	}
	var mcpItems []tui.MCPStatusItem
	var disabledMCPs []string
	if cfg != nil {
		disabledMCPs = cfg.DisabledMCPs
	}
	if statuses, err := mcp.InspectServers(cwd, homeDir, disabledMCPs, activeClients); err == nil {
		for _, st := range statuses {
			mcpItems = append(mcpItems, tui.MCPStatusItem{
				Name:      st.Name,
				Enabled:   st.Enabled,
				Connected: st.Connected,
				ToolCount: st.ToolCount,
			})
		}
	}
	uiModel.ShowStartupBanner(tui.StartupSummary{
		Version:   header.Version,
		ModelName: modelName,
		Tools:     toolNames,
		Skills:    skillItems,
		MCPs:      mcpItems,
	})

	p = tea.NewProgram(uiModel, tea.WithAltScreen(), tea.WithMouseCellMotion())
	tools.SetUIEventSender(func(msg any) {
		if p != nil {
			p.Send(msg)
		}
	})
	defer tools.SetUIEventSender(nil)

	if prompt != "" {
		go func() {
			time.Sleep(50 * time.Millisecond)
			p.Send(tui.UserSubmitPromptMsg{Prompt: prompt})
		}()
	}

	_, err = p.Run()
	return err
}

func (r *Runner) resolveRegistry(cwd string, cfg *settings.Settings) (*tools.Registry, error) {
	reg, _, err := r.resolveRegistryWithClients(cwd, cfg)
	return reg, err
}

func (r *Runner) resolveRegistryWithClients(cwd string, cfg *settings.Settings) (*tools.Registry, []*mcp.Client, error) {
	reg, err := builtin.NewDefaultRegistry()
	if err != nil {
		return nil, nil, fmt.Errorf("initialize builtin tools: %w", err)
	}

	if len(r.opts.ToolAllowlist) == 0 && cfg != nil && len(cfg.DefaultTools) > 0 {
		reg = reg.ScopedRegistry(cfg.DefaultTools)
	}

	homeDir, _ := os.UserHomeDir()
	var disabledMCPs []string
	if cfg != nil {
		disabledMCPs = cfg.DisabledMCPs
	}
	if cfg.IsLegacyExtensionMode() {
		clients, _ := mcp.AutoConnectAndRegisterWithDisabled(context.Background(), cwd, homeDir, reg, disabledMCPs)
		_, _ = js.LoadExtensions(cwd, homeDir, reg)
		if len(r.opts.ToolAllowlist) > 0 {
			reg = reg.ScopedRegistry(r.opts.ToolAllowlist)
		}
		return reg, clients, nil
	}
	_, _ = js.LoadExtensions(cwd, homeDir, reg)

	if len(r.opts.ToolAllowlist) > 0 {
		reg = reg.ScopedRegistry(r.opts.ToolAllowlist)
	}
	return reg, nil, nil
}

func toTUIExtensions(list []extensions.ExtensionStatus) []tui.ExtensionStatusItem {
	out := make([]tui.ExtensionStatusItem, 0, len(list))
	for _, st := range list {
		out = append(out, tui.ExtensionStatusItem{
			Name:        st.Name,
			Description: st.Description,
			AdapterType: st.AdapterType,
			Source:      st.Source,
			Enabled:     st.Enabled,
			ActionCount: st.ActionCount,
			State:       st.State,
			LoadError:   st.LoadError,
		})
	}
	return out
}

// saveGlobalSettingsField 仅对全局 ~/.gp/settings.json 的目标字段做增量更新，绝不引入项目级或环境变量覆写值
func saveGlobalSettingsField(mgr *settings.Manager, cwd string, mutate func(g *settings.Settings)) error {
	if mgr == nil {
		mgr = settings.NewManager(cwd)
	}
	return mgr.UpdateGlobal(mutate)
}

func updateDisabledList(list []string, name string, enable bool) []string {
	target := strings.ToLower(strings.TrimSpace(name))
	var out []string
	for _, item := range list {
		if strings.ToLower(strings.TrimSpace(item)) == target {
			continue
		}
		out = append(out, item)
	}
	if !enable && target != "" {
		out = append(out, strings.TrimSpace(name))
	}
	return out
}

func (r *Runner) resolveSettings(cwd string) *settings.Settings {
	if r.opts.Settings != nil {
		s := r.opts.Settings.Clone()
		return &s
	}
	mgr := r.opts.SettingsMgr
	if mgr == nil {
		mgr = settings.NewManager(cwd)
	}
	if cfg, err := mgr.Load(); err == nil {
		return cfg
	}
	d := settings.DefaultSettings()
	return &d
}

func (r *Runner) resolveModelName(cfg ...*settings.Settings) string {
	var c *settings.Settings
	if len(cfg) > 0 {
		c = cfg[0]
	} else {
		c = r.resolveSettings(r.opts.Cwd)
	}
	if r.opts.ModelName != "" {
		return r.opts.ModelName
	}
	if c != nil && c.Model != "" {
		return c.Model
	}
	if val := os.Getenv("GP_MODEL"); val != "" {
		return val
	}
	if val := os.Getenv("PI_MODEL"); val != "" {
		return val
	}
	return defaultModel
}

func (r *Runner) resolveBaseURL(cfg ...*settings.Settings) string {
	var c *settings.Settings
	if len(cfg) > 0 {
		c = cfg[0]
	} else {
		c = r.resolveSettings(r.opts.Cwd)
	}
	if r.opts.BaseURL != "" {
		return r.opts.BaseURL
	}
	if c != nil && c.BaseURL != "" {
		return c.BaseURL
	}
	if val := os.Getenv("GP_BASE_URL"); val != "" {
		return val
	}
	if val := os.Getenv("OPENAI_BASE_URL"); val != "" {
		return val
	}
	return settings.DefaultBaseURL
}

func (r *Runner) resolveAPIKey(cfg ...*settings.Settings) string {
	var c *settings.Settings
	if len(cfg) > 0 {
		c = cfg[0]
	} else {
		c = r.resolveSettings(r.opts.Cwd)
	}
	if r.opts.APIKey != "" {
		return r.opts.APIKey
	}
	if c != nil && c.APIKey != "" {
		return c.APIKey
	}
	if val := os.Getenv("GP_API_KEY"); val != "" {
		return val
	}
	if val := os.Getenv("OPENAI_API_KEY"); val != "" {
		return val
	}
	return ""
}
