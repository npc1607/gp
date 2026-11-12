package modes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"gp/internal/agent"
	"gp/internal/ai"
	"gp/internal/ai/providers/openai"
	"gp/internal/core/storage/sqlite"
	"gp/internal/modes/interactive"
	"gp/internal/modes/print"
	"gp/internal/tools/builtin"
)

// Mode 运行模式类型
type Mode string

const (
	ModeInteractive Mode = "interactive" // TUI 全屏交互模式
	ModePrint       Mode = "print"       // 终端单次流式/非交互直出 (-p)
	ModeJSONL       Mode = "json"        // 结构化 JSONL 事件流 (--mode json)
	ModeRPC         Mode = "rpc"         // 标准输入输出长连接 JSON-RPC (--mode rpc)
	ModeResume      Mode = "resume"      // 断点续跑模式 (gp resume / --resume)
)

// Runner 运行器统一契约
type Runner interface {
	Run(ctx context.Context, initialPrompt string) error
}

// PrintRunner Print 模式运行器
type PrintRunner struct {
	Opts print.RunnerOptions
}

// Run 执行 Print 模式任务
func (p *PrintRunner) Run(ctx context.Context, prompt string) error {
	runner := print.NewRunner(p.Opts)
	return runner.Run(ctx, prompt)
}

// InteractiveRunner Interactive TUI 模式运行器
type InteractiveRunner struct {
	Opts interactive.RunnerOptions
}

// Run 执行 Interactive 模式
func (i *InteractiveRunner) Run(ctx context.Context, initialPrompt string) error {
	runner := interactive.NewRunner(i.Opts)
	return runner.Run(ctx, initialPrompt)
}

// ResumeRunnerOptions 断点恢复运行器参数
type ResumeRunnerOptions struct {
	DBPath        string
	SessionID     string
	ModelName     string
	ToolAllowlist []string
	Writer        io.Writer
	Provider      ai.Provider
	Cwd           string
}

// ResumeRunner 断点恢复运行器
type ResumeRunner struct {
	Opts ResumeRunnerOptions
}

// Run 执行断点恢复并收敛输出
func (r *ResumeRunner) Run(ctx context.Context, _ string) error {
	outWriter := r.Opts.Writer
	if outWriter == nil {
		outWriter = os.Stdout
	}

	dbPath := r.Opts.DBPath
	if dbPath == "" {
		dbPath = ".gp/gp.db"
	}

	if dbPath != ":memory:" && dbPath != "file::memory:?cache=shared" {
		if _, err := os.Stat(dbPath); err != nil {
			fmt.Fprintln(outWriter, "No unfinished tasks found to resume.")
			return nil
		}
	}

	store, err := sqlite.Open(dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database at %s: %w", dbPath, err)
	}
	defer store.Close()

	prov := r.Opts.Provider
	if prov == nil {
		apiKey := os.Getenv("GP_API_KEY")
		if apiKey == "" {
			apiKey = os.Getenv("OPENAI_API_KEY")
		}
		baseURL := os.Getenv("GP_BASE_URL")
		if baseURL == "" {
			baseURL = os.Getenv("OPENAI_BASE_URL")
		}
		prov = openai.NewProvider(apiKey, baseURL)
	}

	modelName := r.Opts.ModelName
	if modelName == "" {
		modelName = os.Getenv("GP_MODEL")
		if modelName == "" {
			modelName = "antigravity/gemini-4-argon"
		}
	}

	reg, err := builtin.NewDefaultRegistry()
	if err != nil {
		return fmt.Errorf("initialize builtin tools: %w", err)
	}
	if len(r.Opts.ToolAllowlist) > 0 {
		reg = reg.ScopedRegistry(r.Opts.ToolAllowlist)
	}

	var wroteAny bool
	streamHandler := func(chunk ai.StreamChunk) {
		if chunk.DeltaText != "" {
			_, _ = io.WriteString(outWriter, chunk.DeltaText)
			wroteAny = true
			if flusher, ok := outWriter.(interface{ Flush() error }); ok {
				_ = flusher.Flush()
			}
		}
	}

	_, resumeErr := agent.Resume(ctx, agent.ResumeOptions{
		Storage:       store,
		Provider:      prov,
		Registry:      reg,
		SessionID:     r.Opts.SessionID,
		Writer:        outWriter,
		StreamHandler: streamHandler,
		Cwd:           r.Opts.Cwd,
		ModelName:     modelName,
	})

	if errors.Is(resumeErr, agent.ErrNoUnfinishedTask) {
		fmt.Fprintln(outWriter, "No unfinished tasks found to resume.")
		return nil
	}
	if resumeErr != nil {
		return resumeErr
	}

	if wroteAny {
		_, _ = io.WriteString(outWriter, "\n")
	}

	return nil
}
