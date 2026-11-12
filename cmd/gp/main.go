package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"gp/internal/modes"
	"gp/internal/modes/print"
)

var (
	flagMode    = flag.String("mode", "interactive", "运行模式: interactive, print, json, rpc, resume")
	flagPrint   = flag.Bool("p", false, "快捷单次输出模式 (等同于 --mode print)")
	flagResume  = flag.Bool("resume", false, "断点恢复最近中断的任务 (等同于 resume 子命令)")
	flagDB      = flag.String("db", ".gp/gp.db", "SQLite 数据库路径")
	flagModel   = flag.String("model", "", "指定模型标识 (如 antigravity/gemini-4-argon)")
	flagTools   = flag.String("tools", "", "工具白名单列表 (逗号分隔)")
	flagVersion = flag.Bool("v", false, "查看版本号")
)

const Version = "v0.1.0-alpha"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// 1. 处理 resume 子命令 (如: gp resume [session_id] [-db ...] [-model ...])
	if len(os.Args) > 1 && os.Args[1] == "resume" {
		resumeCmd := flag.NewFlagSet("resume", flag.ExitOnError)
		resumeDB := resumeCmd.String("db", ".gp/gp.db", "SQLite 数据库路径")
		resumeModel := resumeCmd.String("model", "", "指定模型标识")
		resumeTools := resumeCmd.String("tools", "", "工具白名单列表")
		_ = resumeCmd.Parse(os.Args[2:])

		var targetSession string
		if len(resumeCmd.Args()) > 0 {
			targetSession = resumeCmd.Args()[0]
		}

		var allowlist []string
		if *resumeTools != "" {
			for _, t := range strings.Split(*resumeTools, ",") {
				if trimmed := strings.TrimSpace(t); trimmed != "" {
					allowlist = append(allowlist, trimmed)
				}
			}
		}

		runner := &modes.ResumeRunner{
			Opts: modes.ResumeRunnerOptions{
				DBPath:        *resumeDB,
				SessionID:     targetSession,
				ModelName:     *resumeModel,
				ToolAllowlist: allowlist,
			},
		}

		if err := runner.Run(ctx, ""); err != nil {
			fmt.Fprintf(os.Stderr, "恢复执行错误: %v\n", err)
			os.Exit(1)
		}
		return
	}

	flag.Parse()

	if *flagVersion {
		fmt.Printf("gp (Go Coding Agent Harness) %s\n", Version)
		return
	}

	activeMode := modes.Mode(*flagMode)
	if *flagPrint {
		activeMode = modes.ModePrint
	}
	if *flagResume {
		activeMode = modes.ModeResume
	}

	remainingArgs := flag.Args()
	prompt := strings.Join(remainingArgs, " ")

	var allowlist []string
	if *flagTools != "" {
		for _, t := range strings.Split(*flagTools, ",") {
			if trimmed := strings.TrimSpace(t); trimmed != "" {
				allowlist = append(allowlist, trimmed)
			}
		}
	}

	switch activeMode {
	case modes.ModeResume:
		var targetSession string
		if len(remainingArgs) > 0 {
			targetSession = remainingArgs[0]
		}

		runner := &modes.ResumeRunner{
			Opts: modes.ResumeRunnerOptions{
				DBPath:        *flagDB,
				SessionID:     targetSession,
				ModelName:     *flagModel,
				ToolAllowlist: allowlist,
			},
		}

		if err := runner.Run(ctx, ""); err != nil {
			fmt.Fprintf(os.Stderr, "恢复执行错误: %v\n", err)
			os.Exit(1)
		}

	case modes.ModePrint:
		if strings.TrimSpace(prompt) == "" {
			fmt.Fprintln(os.Stderr, "错误: 请提供提示词，如: gp -p \"读取 main.go\"")
			os.Exit(1)
		}

		runner := &modes.PrintRunner{
			Opts: print.RunnerOptions{
				ModelName:     *flagModel,
				ToolAllowlist: allowlist,
				DBPath:        *flagDB,
			},
		}

		if err := runner.Run(ctx, prompt); err != nil {
			fmt.Fprintf(os.Stderr, "运行错误: %v\n", err)
			os.Exit(1)
		}

	default:
		fmt.Printf("gp %s | 模式: %s | 模型: %s\n", Version, activeMode, *flagModel)
		fmt.Println("提示: 核心骨架已就绪，等待后续模块装配...")
	}
}
