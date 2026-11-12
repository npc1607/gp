package modes

import (
	"context"
	"fmt"
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

// PrintRunner Print 模式运行器实现骨架
type PrintRunner struct{}

func (p *PrintRunner) Run(ctx context.Context, prompt string) error {
	fmt.Printf("[gp print mode] 处理任务: %s\n", prompt)
	return nil
}
