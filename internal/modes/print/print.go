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
	"gp/internal/tools/builtin"
)

const defaultModel = "antigravity/gemini-4-argon"

// RunnerOptions 配置 Print 模式的参数
type RunnerOptions struct {
	ModelName     string
	BaseURL       string
	APIKey        string
	Cwd           string
	ToolAllowlist []string
	Writer        io.Writer
	Provider      ai.Provider
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

	modelName := r.resolveModelName()
	baseURL := r.resolveBaseURL()
	apiKey := r.resolveAPIKey()

	cwd := r.opts.Cwd
	if cwd == "" {
		if dir, err := os.Getwd(); err == nil {
			cwd = dir
		} else {
			cwd = "."
		}
	}

	outWriter := r.opts.Writer
	if outWriter == nil {
		outWriter = os.Stdout
	}

	prov := r.opts.Provider
	if prov == nil {
		prov = openai.NewProvider(apiKey, baseURL)
	}

	reg, err := builtin.NewDefaultRegistry()
	if err != nil {
		return fmt.Errorf("initialize builtin tools: %w", err)
	}
	if len(r.opts.ToolAllowlist) > 0 {
		reg = reg.ScopedRegistry(r.opts.ToolAllowlist)
	}

	ag := agent.NewAgent("cli-print", "assistant", prov, modelName, reg)
	ag.SetCwd(cwd)

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

func (r *Runner) resolveModelName() string {
	if r.opts.ModelName != "" {
		return r.opts.ModelName
	}
	if val := os.Getenv("GP_MODEL"); val != "" {
		return val
	}
	if val := os.Getenv("PI_MODEL"); val != "" {
		return val
	}
	return defaultModel
}

func (r *Runner) resolveBaseURL() string {
	if r.opts.BaseURL != "" {
		return r.opts.BaseURL
	}
	if val := os.Getenv("GP_BASE_URL"); val != "" {
		return val
	}
	if val := os.Getenv("OPENAI_BASE_URL"); val != "" {
		return val
	}
	return "https://api.openai.com/v1"
}

func (r *Runner) resolveAPIKey() string {
	if r.opts.APIKey != "" {
		return r.opts.APIKey
	}
	if val := os.Getenv("GP_API_KEY"); val != "" {
		return val
	}
	if val := os.Getenv("OPENAI_API_KEY"); val != "" {
		return val
	}
	return ""
}
