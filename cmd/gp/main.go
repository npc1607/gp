package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
	"gp/internal/core/settings"
	"gp/internal/modes"
	"gp/internal/modes/interactive"
	"gp/internal/modes/print"
	"gp/internal/modes/rpc"
	_ "gp/plugins/askuser" // 挂载 rpiv-ask-user-question 插件
	_ "gp/plugins/qoder"   // 挂载 Qoder Provider 插件
)

func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".gp", "gp.db")
	}
	return filepath.Join(home, ".gp", "gp.db")
}

var (
	flagMode    = flag.String("mode", "interactive", "运行模式: interactive, print, json, rpc, resume")
	flagPrint   = flag.Bool("p", false, "快捷单次输出模式 (等同于 --mode print)")
	flagResume  = flag.Bool("resume", false, "断点恢复最近中断的任务 (等同于 resume 子命令)")
	flagDB      = flag.String("db", defaultDBPath(), "SQLite 数据库路径")
	flagModel   = flag.String("model", "", "指定模型标识 (如 antigravity/gemini-4-argon)")
	flagTools   = flag.String("tools", "", "工具白名单列表 (逗号分隔)")
	flagListen  = flag.String("listen", "", "RPC/AGP 模式监听地址 (如 unix:///tmp/gp-agent.sock 或 tcp://127.0.0.1:9527)")
	flagDebug   = flag.Bool("debug", false, "启用调试日志输出到 ./logs 目录")
	flagVersion = flag.Bool("v", false, "查看版本号")
)

var Version = "v0.1.0-alpha"

// debugEnvEnabled 判断环境变量 GP_DEBUG 是否开启调试日志
func debugEnvEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("GP_DEBUG")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// setupDebugLog 开启 slog 调试日志：Debug 级别写入 logs/gp-debug-<时间戳>.log，
// 并将其设为 slog 默认 logger；未开启时在非 RPC 模式下静默丢弃以免污染 TUI 终端界面。
func setupDebugLog(dir string) (string, func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", func() {}, fmt.Errorf("create log dir %q: %w", dir, err)
	}
	path := filepath.Join(dir, fmt.Sprintf("gp-debug-%s.log", time.Now().Format("20060102-150405")))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", func() {}, fmt.Errorf("open debug log %q: %w", path, err)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})))
	slog.Debug("debug log initialized", "component", "CLI", "file", path)
	return path, func() { _ = f.Close() }, nil
}

// configureDefaultLogger 根据运行模式与调试开关设置 slog 默认输出目标：
// 1. debug=true 时统一写入调试日志文件；
// 2. debug=false 且处于 interactive/print/resume 等前景终端模式时静默丢弃 slog，防止日志覆盖 TUI 画面；
// 3. RPC/serve 守护进程模式保留标准错误日志输出。
func configureDefaultLogger(debug bool, activeMode modes.Mode, logDir string) (func(), error) {
	if debug {
		_, closeFn, err := setupDebugLog(logDir)
		return closeFn, err
	}
	if activeMode != modes.ModeRPC {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1})))
	}
	return func() {}, nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	settingsMgr := settings.NewManager(cwd)
	cfg, err := settingsMgr.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "警告: 加载配置文件失败 (%v)，使用默认配置\n", err)
		defaultCfg := settings.DefaultSettings()
		cfg = &defaultCfg
	}

	// 0. 处理 serve 子命令 (如: gp serve --listen unix:///tmp/gp-agent.sock)
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		serveCmd := flag.NewFlagSet("serve", flag.ExitOnError)
		serveListen := serveCmd.String("listen", "unix:///tmp/gp-agent.sock", "AGP 协议监听地址")
		serveDB := serveCmd.String("db", defaultDBPath(), "SQLite 数据库路径")
		serveModel := serveCmd.String("model", "", "指定模型标识")
		serveTools := serveCmd.String("tools", "", "工具白名单列表")
		_ = serveCmd.Parse(os.Args[2:])

		var allowlist []string
		if *serveTools != "" {
			for t := range strings.SplitSeq(*serveTools, ",") {
				if trimmed := strings.TrimSpace(t); trimmed != "" {
					allowlist = append(allowlist, trimmed)
				}
			}
		}

		modelName := *serveModel
		if modelName == "" {
			modelName = cfg.Model
		}

		runner := &modes.RPCRunner{
			Endpoint: *serveListen,
			Opts: rpc.RunnerOptions{
				ModelName:     modelName,
				ToolAllowlist: allowlist,
				DBPath:        *serveDB,
				Settings:      cfg,
				SettingsMgr:   settingsMgr,
				AgentVersion:  Version,
			},
		}

		if err := runner.Run(ctx, ""); err != nil {
			fmt.Fprintf(os.Stderr, "AGP 服务运行错误: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// 1. 处理 resume 子命令 (如: gp resume [session_id] [-db ...] [-model ...] [-tools ...])
	if len(os.Args) > 1 && os.Args[1] == "resume" {
		if closeLog, logErr := configureDefaultLogger(debugEnvEnabled(), modes.ModeResume, filepath.Join(cwd, "logs")); logErr != nil {
			fmt.Fprintf(os.Stderr, "警告: 初始化调试日志失败: %v\n", logErr)
		} else {
			defer closeLog()
		}
		resumeCmd := flag.NewFlagSet("resume", flag.ExitOnError)
		resumeDB := resumeCmd.String("db", defaultDBPath(), "SQLite 数据库路径")
		resumeModel := resumeCmd.String("model", "", "指定模型标识")
		resumeTools := resumeCmd.String("tools", "", "工具白名单列表")
		_ = resumeCmd.Parse(os.Args[2:])

		var targetSession string
		if len(resumeCmd.Args()) > 0 {
			targetSession = resumeCmd.Args()[0]
		}

		var allowlist []string
		if *resumeTools != "" {
			for t := range strings.SplitSeq(*resumeTools, ",") {
				if trimmed := strings.TrimSpace(t); trimmed != "" {
					allowlist = append(allowlist, trimmed)
				}
			}
		}

		modelName := *resumeModel
		if modelName == "" {
			modelName = cfg.Model
		}

		runner := &modes.ResumeRunner{
			Opts: modes.ResumeRunnerOptions{
				DBPath:        *resumeDB,
				SessionID:     targetSession,
				ModelName:     modelName,
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

	modelName := *flagModel
	if modelName == "" {
		modelName = cfg.Model
	}

	activeMode := modes.Mode(*flagMode)
	if *flagPrint {
		activeMode = modes.ModePrint
	}
	if *flagResume {
		activeMode = modes.ModeResume
	}

	if closeLog, logErr := configureDefaultLogger(*flagDebug || debugEnvEnabled(), activeMode, filepath.Join(cwd, "logs")); logErr != nil {
		fmt.Fprintf(os.Stderr, "警告: 初始化调试日志失败: %v\n", logErr)
	} else {
		defer closeLog()
	}

	remainingArgs := flag.Args()
	prompt := strings.Join(remainingArgs, " ")
	slog.Debug("startup", "component", "CLI", "mode", activeMode, "model", modelName, "cwd", cwd)

	var allowlist []string
	if *flagTools != "" {
		for t := range strings.SplitSeq(*flagTools, ",") {
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
				ModelName:     modelName,
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
				ModelName:     modelName,
				ToolAllowlist: allowlist,
				DBPath:        *flagDB,
				Settings:      cfg,
				SettingsMgr:   settingsMgr,
			},
		}

		if err := runner.Run(ctx, prompt); err != nil {
			fmt.Fprintf(os.Stderr, "运行错误: %v\n", err)
			os.Exit(1)
		}

	case modes.ModeRPC:
		runner := &modes.RPCRunner{
			Endpoint: *flagListen,
			Opts: rpc.RunnerOptions{
				ModelName:     modelName,
				ToolAllowlist: allowlist,
				DBPath:        *flagDB,
				Settings:      cfg,
				SettingsMgr:   settingsMgr,
				AgentVersion:  Version,
			},
		}

		if err := runner.Run(ctx, ""); err != nil {
			fmt.Fprintf(os.Stderr, "RPC 服务运行错误: %v\n", err)
			os.Exit(1)
		}

	case modes.ModeInteractive:
		if !isatty.IsTerminal(os.Stdin.Fd()) && !isatty.IsCygwinTerminal(os.Stdin.Fd()) {
			fmt.Printf("gp %s | 模式: %s | 模型: %s\n", Version, activeMode, modelName)
			fmt.Println("提示: 核心骨架已就绪，等待后续模块装配...")
			return
		}

		runner := &modes.InteractiveRunner{
			Opts: interactive.RunnerOptions{
				ModelName:     modelName,
				ToolAllowlist: allowlist,
				DBPath:        *flagDB,
				Settings:      cfg,
				SettingsMgr:   settingsMgr,
				InitialPrompt: prompt,
			},
		}

		if err := runner.Run(ctx, prompt); err != nil {
			fmt.Fprintf(os.Stderr, "交互运行错误: %v\n", err)
			os.Exit(1)
		}

	default:
		fmt.Printf("gp %s | 模式: %s | 模型: %s\n", Version, activeMode, modelName)
		fmt.Println("提示: 核心骨架已就绪，等待后续模块装配...")
	}
}
