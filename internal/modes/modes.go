package modes

import (
	"context"

	"gp/internal/modes/print"
)

// Mode 运行模式类型
type Mode string

const (
	ModeInteractive Mode = "interactive" // TUI 全屏交互模式
	ModePrint       Mode = "print"       // 终端单次流式/非交互直出 (-p)
	ModeJSONL       Mode = "json"        // 结构化 JSONL 事件流 (--mode json)
	ModeRPC         Mode = "rpc"         // 标准输入输出长连接 JSON-RPC (--mode rpc)
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
