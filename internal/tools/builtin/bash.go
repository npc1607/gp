package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"gp/internal/tools"
)

var (
	_ tools.Tool           = (*BashTool)(nil)
	_ tools.ReplayableTool = (*BashTool)(nil)
)

const (
	defaultBashTimeout = 30 * time.Second
	maxBashBytes       = 50 * 1024 // 50KB
	maxBashLines       = 2000
)

// BashTool 执行 Shell 命令，支持进程组隔离、超时强杀与输出防爆截断
type BashTool struct{}

func (b *BashTool) ReplayMode() tools.ReplayMode {
	return tools.ReplayInterrupt
}

type bashArgs struct {
	Command string  `json:"command"`
	Timeout float64 `json:"timeout,omitempty"`
}

func (b *BashTool) Name() string {
	return "bash"
}

func (b *BashTool) Description() string {
	return "Execute a shell command with timeout and output capture."
}

func (b *BashTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {
				"type": "string",
				"description": "Shell command to execute"
			},
			"timeout": {
				"type": "number",
				"description": "Optional timeout in seconds (default 30)"
			}
		},
		"required": ["command"]
	}`)
}

func (b *BashTool) Execute(ctx *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
	var args bashArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return &tools.ToolResult{
			Content: fmt.Sprintf("invalid arguments for bash tool: %v", err),
			IsError: true,
		}, nil
	}

	cmdStr := strings.TrimSpace(args.Command)
	if cmdStr == "" {
		return &tools.ToolResult{
			Content: "command is required",
			IsError: true,
		}, nil
	}

	timeout := defaultBashTimeout
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout * float64(time.Second))
	}

	parentCtx := context.Background()
	if ctx != nil && ctx.Context != nil {
		parentCtx = ctx.Context
	}
	cmdCtx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	shell := "bash"
	if _, err := exec.LookPath("bash"); err != nil {
		shell = "sh"
	}

	cmd := exec.CommandContext(cmdCtx, shell, "-c", cmdStr)

	// Set working directory
	cwd := "."
	if ctx != nil && ctx.Cwd != "" {
		cwd = ctx.Cwd
	}
	cmd.Dir = cwd

	// Merge environment variables
	cmd.Env = os.Environ()
	if ctx != nil && ctx.Env != nil {
		for k, v := range ctx.Env {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	// Process group isolation & SIGKILL on cancel
	setProcessGroup(cmd)
	cmd.Cancel = func() error {
		return killProcessGroup(cmd)
	}

	var outputBuf syncOutputBuffer
	cmd.Stdout = &outputBuf
	cmd.Stderr = &outputBuf

	stopProgress := startBashProgressSampler(cmdCtx, ctx, &outputBuf)
	runErr := cmd.Run()
	stopProgress()

	rawOut := outputBuf.String()
	truncatedOut, wasTruncated := truncateOutput(rawOut, maxBashBytes, maxBashLines)
	if wasTruncated {
		truncatedOut += "\n[Output truncated at 50KB / 2000 lines]"
	}

	if cmdCtx.Err() == context.DeadlineExceeded {
		msg := truncatedOut
		if msg == "" {
			msg = fmt.Sprintf("Command timed out after %v", timeout)
		} else {
			msg = fmt.Sprintf("%s\nCommand timed out after %v", msg, timeout)
		}
		return &tools.ToolResult{
			Content: msg,
			IsError: true,
		}, nil
	}

	if runErr != nil {
		msg := truncatedOut
		if msg == "" {
			msg = fmt.Sprintf("Process exited with error: %v", runErr)
		} else {
			msg = fmt.Sprintf("%s\nProcess exited with error: %v", msg, runErr)
		}
		return &tools.ToolResult{
			Content: msg,
			IsError: true,
		}, nil
	}

	if strings.TrimSpace(truncatedOut) == "" {
		truncatedOut = "(no output)"
	}

	return &tools.ToolResult{
		Content: truncatedOut,
		IsError: false,
	}, nil
}

func truncateOutput(s string, maxBytes, maxLines int) (string, bool) {
	if len(s) == 0 {
		return "", false
	}

	truncated := false
	raw := s
	if len(raw) > maxBytes {
		raw = raw[:maxBytes]
		truncated = true
	}

	lines := strings.Split(raw, "\n")
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		truncated = true
	}

	return strings.Join(lines, "\n"), truncated
}

var (
	bashProgressInitialDelay = 10 * time.Second
	bashProgressInterval     = 15 * time.Second
	bashStallThreshold       = 120 * time.Second
)

type syncOutputBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncOutputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncOutputBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *syncOutputBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncOutputBuffer) Snapshot() (int, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.buf.Len()
	if n == 0 {
		return 0, ""
	}
	raw := b.buf.Bytes()
	if len(raw) > 2048 {
		raw = raw[len(raw)-2048:]
	}
	return n, extractLastNonEmptyLine(string(raw))
}

func extractLastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			runes := []rune(line)
			if len(runes) > 100 {
				return string(runes[:100]) + "..."
			}
			return line
		}
	}
	return ""
}

func startBashProgressSampler(cmdCtx context.Context, toolCtx *tools.ToolContext, buf *syncOutputBuffer) func() {
	if toolCtx == nil || toolCtx.EventChan == nil {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		startedAt := time.Now()
		lastChangeAt := startedAt
		lastLen := 0

		timer := time.NewTimer(bashProgressInitialDelay)
		defer timer.Stop()

		select {
		case <-done:
			return
		case <-cmdCtx.Done():
			return
		case <-timer.C:
		}

		ticker := time.NewTicker(bashProgressInterval)
		defer ticker.Stop()

		emitSample := func() {
			curLen, tailLine := buf.Snapshot()
			stalled := false
			var action string
			if curLen > lastLen {
				lastLen = curLen
				lastChangeAt = time.Now()
				if tailLine != "" {
					action = fmt.Sprintf("输出更新 (%d B): %s", curLen, tailLine)
				} else {
					action = fmt.Sprintf("输出更新 (%d B)", curLen)
				}
			} else {
				silentFor := time.Since(lastChangeAt)
				if silentFor >= bashStallThreshold {
					stalled = true
					action = fmt.Sprintf("已连续 %s 无新输出（累计 %d B），若非编译/下载请确认是否卡在等待输入", silentFor.Round(time.Second), curLen)
				} else {
					action = fmt.Sprintf("运行中（已耗时 %s，输出 %d B）", time.Since(startedAt).Round(time.Second), curLen)
				}
			}
			select {
			case toolCtx.EventChan <- tools.ToolEvent{
				ToolName: "bash",
				Action:   action,
				Stalled:  stalled,
			}:
			default:
			}
		}

		emitSample()
		for {
			select {
			case <-done:
				return
			case <-cmdCtx.Done():
				return
			case <-ticker.C:
				emitSample()
			}
		}
	}()
	return func() {
		once.Do(func() { close(done) })
	}
}
