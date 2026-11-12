package print

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

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
	"time"
)

const defaultModel = settings.DefaultModel

// RunnerOptions 配置 Print 模式的参数
type RunnerOptions struct {
	ModelName     string
	BaseURL       string
	APIKey        string
	Cwd           string
	DBPath        string
	Storage       storage.Storage
	ToolAllowlist []string
	Writer        io.Writer
	Provider      ai.Provider
	SettingsMgr   *settings.Manager
	Settings      *settings.Settings
}

// Runner Print 模式运行器
type Runner struct {
	opts RunnerOptions
}

// NewRunner 创建 Print 模式运行器
func NewRunner(opts RunnerOptions) *Runner {
	return &Runner{opts: opts}
}

// Run 执行一次性命令行推理交互，流式直出模型文本
func (r *Runner) Run(ctx context.Context, prompt string) error {
	trimmedPrompt := strings.TrimSpace(prompt)
	if trimmedPrompt == "" {
		return fmt.Errorf("prompt is required in print mode")
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

	outWriter := r.opts.Writer
	if outWriter == nil {
		outWriter = os.Stdout
	}

	prov := r.opts.Provider
	if prov == nil {
		prov = openai.NewProvider(apiKey, baseURL)
	}

	reg, err := r.resolveRegistry(cwd, cfg)
	if err != nil {
		return fmt.Errorf("resolve registry: %w", err)
	}

	ag := agent.NewAgent("cli-print", "assistant", prov, modelName, reg)
	ag.SetCwd(cwd)

	homeDir, _ := os.UserHomeDir()
	resLoader := resources.NewResourceLoader(cwd, homeDir)
	sysPrompt := resLoader.BuildSystemPrompt(cfg.SystemPrompt)
	if sysPrompt != "" {
		ag.SetSystemPrompt(sysPrompt)
	}

	var store storage.Storage = r.opts.Storage
	var shouldCloseStore bool
	if store == nil && r.opts.DBPath != "" {
		if s, err := sqlite.Open(r.opts.DBPath); err == nil {
			store = s
			shouldCloseStore = true
		}
	}
	if store != nil {
		if shouldCloseStore {
			defer store.Close()
		}
		sessID := fmt.Sprintf("session_%d", time.Now().UnixNano())
		st, err := session.NewSessionTree(sessID, store)
		if err == nil {
			ag.SetSessionTree(st)
			ag.SetStorage(store)
		}
	}

	var wroteAny bool
	ag.SetStreamHandler(func(chunk ai.StreamChunk) {
		if chunk.DeltaText != "" {
			_, _ = io.WriteString(outWriter, chunk.DeltaText)
			wroteAny = true
			if flusher, ok := outWriter.(interface{ Flush() error }); ok {
				_ = flusher.Flush()
			}
		}
	})

	_, promptErr := ag.Prompt(ctx, trimmedPrompt)
	if promptErr != nil {
		return promptErr
	}

	if wroteAny {
		_, _ = io.WriteString(outWriter, "\n")
	}

	return nil
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
