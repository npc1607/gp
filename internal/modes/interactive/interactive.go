package interactive

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"gp/internal/agent"
	"gp/internal/ai"
	"gp/internal/ai/providers/openai"
	"gp/internal/core/resources"
	"gp/internal/core/session"
	"gp/internal/core/settings"
	"gp/internal/core/storage"
	"gp/internal/core/storage/sqlite"
	"gp/internal/extensions/js"
	"gp/internal/mcp"
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

	modelName := r.resolveModelName(cfg)
	baseURL := r.resolveBaseURL(cfg)
	apiKey := r.resolveAPIKey(cfg)

	prov := r.opts.Provider
	if prov == nil {
		prov = openai.NewProvider(apiKey, baseURL)
	}

	reg, err := r.resolveRegistry(cwd, cfg)
	if err != nil {
		return fmt.Errorf("resolve registry: %w", err)
	}

	var store storage.Storage = r.opts.Storage
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

	ag := agent.NewAgent("main", "orchestrator", prov, modelName, reg)
	ag.SetCwd(cwd)

	homeDir, _ := os.UserHomeDir()
	resLoader := resources.NewResourceLoader(cwd, homeDir)
	sysPrompt := resLoader.BuildSystemPrompt(cfg.SystemPrompt)
	if sysPrompt != "" {
		ag.SetSystemPrompt(sysPrompt)
	}
	if store != nil {
		st, err := session.NewSessionTree(sessID, store)
		if err == nil {
			ag.SetSessionTree(st)
			ag.SetStorage(store)
		}
	}

	header := tui.HeaderInfo{
		AppName:    "gp",
		Version:    "v0.1.0-alpha",
		ModelName:  modelName,
		SessionID:  sessID,
		TokenUsage: "0 tok",
	}

	var p *tea.Program

	ag.SetStreamHandler(func(chunk ai.StreamChunk) {
		if p != nil && chunk.DeltaText != "" {
			p.Send(tui.AgentStreamDeltaMsg{
				AgentID:   "main",
				DeltaText: chunk.DeltaText,
			})
		}
	})

	var isRunning atomic.Bool
	uiModel := tui.NewMainUIModel(header, func(userPrompt string) {
		if !isRunning.CompareAndSwap(false, true) {
			if p != nil {
				p.Send(tui.AgentStreamDeltaMsg{
					AgentID:   "main",
					DeltaText: "\n[System]: 当前任务仍在执行中，请稍候再提交...\n",
				})
			}
			return
		}
		go func() {
			defer isRunning.Store(false)
			if p != nil {
				p.Send(tui.ActiveCoroutineMsg{
					AgentID:  "main",
					Role:     "Orchestrator",
					TaskDesc: "Thinking...",
					Elapsed:  0,
					Active:   true,
				})
			}
			start := time.Now()
			_, promptErr := ag.Prompt(ctx, userPrompt)
			elapsed := time.Since(start)
			if p != nil {
				p.Send(tui.ActiveCoroutineMsg{
					AgentID: "main",
					Active:  false,
				})
				if promptErr != nil {
					p.Send(tui.ErrorMsg{Err: promptErr})
				} else {
					p.Send(tui.AgentStreamDeltaMsg{
						AgentID:   "main",
						DeltaText: fmt.Sprintf("\n[Completed in %.1fs]\n", elapsed.Seconds()),
					})
				}
			}
		}()
	})

	// Headless 测试模式直接返回，避免在无终端环境阻塞 Program.Run
	if r.opts.Headless {
		if prompt != "" {
			_, err := ag.Prompt(ctx, prompt)
			return err
		}
		return nil
	}

	p = tea.NewProgram(uiModel, tea.WithAltScreen())

	if prompt != "" {
		go func() {
			time.Sleep(50 * time.Millisecond)
			p.Send(tui.AgentStreamDeltaMsg{
				AgentID:   "main",
				DeltaText: fmt.Sprintf("[User]: %s\n\n", prompt),
			})
			p.Send(tui.ActiveCoroutineMsg{
				AgentID:  "main",
				Role:     "Orchestrator",
				TaskDesc: "Processing prompt...",
				Elapsed:  0,
				Active:   true,
			})
			start := time.Now()
			_, err := ag.Prompt(ctx, prompt)
			elapsed := time.Since(start)
			p.Send(tui.ActiveCoroutineMsg{
				AgentID: "main",
				Active:  false,
			})
			if err != nil {
				p.Send(tui.ErrorMsg{Err: err})
			} else {
				p.Send(tui.AgentStreamDeltaMsg{
					AgentID:   "main",
					DeltaText: fmt.Sprintf("\n[Completed in %.1fs]\n", elapsed.Seconds()),
				})
			}
		}()
	}

	_, err = p.Run()
	return err
}

func (r *Runner) resolveRegistry(cwd string, cfg *settings.Settings) (*tools.Registry, error) {
	reg, err := builtin.NewDefaultRegistry()
	if err != nil {
		return nil, fmt.Errorf("initialize builtin tools: %w", err)
	}

	homeDir, _ := os.UserHomeDir()
	_, _ = mcp.AutoConnectAndRegister(context.Background(), cwd, homeDir, reg)
	_, _ = js.LoadExtensions(cwd, homeDir, reg)

	allowlist := r.opts.ToolAllowlist
	if len(allowlist) == 0 && cfg != nil && len(cfg.DefaultTools) > 0 {
		allowlist = cfg.DefaultTools
	}
	if len(allowlist) > 0 {
		reg = reg.ScopedRegistry(allowlist)
	}
	return reg, nil
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
