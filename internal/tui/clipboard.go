package tui

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// ClipboardWriter 可在测试中替换的剪贴板写入钩子
var ClipboardWriter = copyToSystemClipboard

// CopyTextToClipboard 将文本写入剪贴板（优先系统原生命令，同时回退/补充 OSC 52 兼容 SSH 与现代终端）
func CopyTextToClipboard(text string) error {
	if text == "" {
		return nil
	}
	return ClipboardWriter(text)
}

func copyToSystemClipboard(text string) error {
	nativeErr := writeNativeClipboard(text)
	// 同时发射 OSC 52（支持 iTerm2 / Ghostty / WezTerm / Kitty / Alacritty / SSH / tmux）
	oscErr := writeOSC52(os.Stdout, text)
	if nativeErr == nil || oscErr == nil {
		return nil
	}
	return fmt.Errorf("clipboard copy failed (native: %v; osc52: %v)", nativeErr, oscErr)
}

func writeNativeClipboard(text string) error {
	var candidates [][]string
	switch runtime.GOOS {
	case "darwin":
		candidates = [][]string{{"pbcopy"}}
	case "windows":
		candidates = [][]string{{"clip"}}
	default:
		candidates = [][]string{
			{"wl-copy"},
			{"xclip", "-selection", "clipboard"},
			{"xsel", "--clipboard", "--input"},
		}
	}
	var lastErr error
	for _, args := range candidates {
		path, err := exec.LookPath(args[0])
		if err != nil {
			lastErr = err
			continue
		}
		cmd := exec.Command(path, args[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	if lastErr == nil {
		return fmt.Errorf("no clipboard command found")
	}
	return lastErr
}

// writeOSC52 通过终端 OSC 52 转义序列写入系统剪贴板
func writeOSC52(w io.Writer, text string) error {
	if w == nil {
		return fmt.Errorf("nil writer")
	}
	// 仅在真实 TTY 环境下输出 OSC 52，避免污染非交互管道或测试 stdout
	if f, ok := w.(*os.File); ok {
		fi, err := f.Stat()
		if err != nil || (fi.Mode()&os.ModeCharDevice) == 0 {
			return fmt.Errorf("not a tty")
		}
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(text))
	seq := "\x1b]52;c;" + encoded + "\x07"
	if os.Getenv("TMUX") != "" {
		// tmux passthrough 包装
		seq = "\x1bPtmux;\x1b" + seq + "\x1b\\"
	}
	_, err := io.WriteString(w, seq)
	return err
}
