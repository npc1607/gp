package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"gp/internal/modes"
)

var (
	flagMode    = flag.String("mode", "interactive", "运行模式: interactive, print, json, rpc")
	flagPrint   = flag.Bool("p", false, "快捷单次输出模式 (等同于 --mode print)")
	flagModel   = flag.String("model", "", "指定模型标识 (如 anthropic/claude-3-7-sonnet)")
	flagTools   = flag.String("tools", "", "工具白名单列表 (逗号分隔)")
	flagVersion = flag.Bool("v", false, "查看版本号")
)

const Version = "v0.1.0-alpha"

func main() {
	flag.Parse()

	if *flagVersion {
		fmt.Printf("gp (Go Coding Agent Harness) %s\n", Version)
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	activeMode := modes.Mode(*flagMode)
	if *flagPrint {
		activeMode = modes.ModePrint
	}

	remainingArgs := flag.Args()
	prompt := ""
	if len(remainingArgs) > 0 {
		prompt = remainingArgs[0]
	}

	switch activeMode {
	case modes.ModePrint:
		runner := &modes.PrintRunner{}
		if err := runner.Run(ctx, prompt); err != nil {
			fmt.Fprintf(os.Stderr, "运行错误: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Printf("gp %s | 模式: %s | 模型: %s\n", Version, activeMode, *flagModel)
		fmt.Println("提示: 核心骨架已就绪，等待后续模块装配...")
	}
}
