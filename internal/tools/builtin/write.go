package builtin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gp/internal/tools"
)

var (
	_ tools.Tool           = (*WriteTool)(nil)
	_ tools.ReplayableTool = (*WriteTool)(nil)
)

// WriteTool 原子写入文件内容，自动创建父级目录
type WriteTool struct{}

func (w *WriteTool) ReplayMode() tools.ReplayMode {
	return tools.ReplayInterrupt
}

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (w *WriteTool) Name() string {
	return "write"
}

func (w *WriteTool) Description() string {
	return "Write content to a file atomically, creating parent directories if needed."
}

func (w *WriteTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "Path to the file to write (relative or absolute)"
			},
			"content": {
				"type": "string",
				"description": "Content to write to the file"
			}
		},
		"required": ["path", "content"]
	}`)
}

func (w *WriteTool) Execute(ctx *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
	var args writeArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return &tools.ToolResult{
			Content: fmt.Sprintf("invalid arguments for write tool: %v", err),
			IsError: true,
		}, nil
	}

	if strings.TrimSpace(args.Path) == "" {
		return &tools.ToolResult{
			Content: "path is required",
			IsError: true,
		}, nil
	}

	absPath := args.Path
	if !filepath.IsAbs(absPath) {
		cwd := "."
		if ctx != nil && ctx.Cwd != "" {
			cwd = ctx.Cwd
		}
		absPath = filepath.Join(cwd, absPath)
	}

	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return &tools.ToolResult{
			Content: fmt.Sprintf("failed to create directory: %v", err),
			IsError: true,
		}, nil
	}

	// Atomic write using a temp file in the same directory
	tmpFile, err := os.CreateTemp(dir, ".tmp-write-*")
	if err != nil {
		return &tools.ToolResult{
			Content: fmt.Sprintf("failed to create temp file: %v", err),
			IsError: true,
		}, nil
	}
	tmpName := tmpFile.Name()
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmpFile.WriteString(args.Content); err != nil {
		_ = tmpFile.Close()
		return &tools.ToolResult{
			Content: fmt.Sprintf("failed to write to temp file: %v", err),
			IsError: true,
		}, nil
	}

	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return &tools.ToolResult{
			Content: fmt.Sprintf("failed to sync temp file: %v", err),
			IsError: true,
		}, nil
	}

	if err := tmpFile.Close(); err != nil {
		return &tools.ToolResult{
			Content: fmt.Sprintf("failed to close temp file: %v", err),
			IsError: true,
		}, nil
	}

	if err := os.Rename(tmpName, absPath); err != nil {
		return &tools.ToolResult{
			Content: fmt.Sprintf("failed to replace destination file: %v", err),
			IsError: true,
		}, nil
	}

	success = true
	return &tools.ToolResult{
		Content: fmt.Sprintf("Successfully wrote %d bytes to %s", len(args.Content), args.Path),
	}, nil
}
