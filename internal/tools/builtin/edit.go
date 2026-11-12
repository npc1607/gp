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
	_ tools.Tool           = (*EditTool)(nil)
	_ tools.ReplayableTool = (*EditTool)(nil)
)

// EditTool 精确文本替换，带严格唯一性校验
type EditTool struct{}

func (e *EditTool) ReplayMode() tools.ReplayMode {
	return tools.ReplayInterrupt
}

type editArgs struct {
	Path    string `json:"path"`
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

func (e *EditTool) Name() string {
	return "edit"
}

func (e *EditTool) Description() string {
	return "Replace exact text in a file. oldText must match exactly once in the file."
}

func (e *EditTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "Path to the file to edit (relative or absolute)"
			},
			"oldText": {
				"type": "string",
				"description": "Exact text to be replaced (must appear exactly once in the file)"
			},
			"newText": {
				"type": "string",
				"description": "Replacement text"
			}
		},
		"required": ["path", "oldText", "newText"]
	}`)
}

func (e *EditTool) Execute(ctx *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
	var args editArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return &tools.ToolResult{
			Content: fmt.Sprintf("invalid arguments for edit tool: %v", err),
			IsError: true,
		}, nil
	}

	if strings.TrimSpace(args.Path) == "" {
		return &tools.ToolResult{
			Content: "path is required",
			IsError: true,
		}, nil
	}

	if args.OldText == "" {
		return &tools.ToolResult{
			Content: "oldText must not be empty",
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

	data, err := os.ReadFile(absPath)
	if err != nil {
		return &tools.ToolResult{
			Content: fmt.Sprintf("failed to read file: %v", err),
			IsError: true,
		}, nil
	}

	content := string(data)
	count := strings.Count(content, args.OldText)
	if count == 0 {
		return &tools.ToolResult{
			Content: fmt.Sprintf("oldText not found in file: %s", args.Path),
			IsError: true,
		}, nil
	}
	if count > 1 {
		return &tools.ToolResult{
			Content: fmt.Sprintf("oldText appears %d times in %s, must be unique", count, args.Path),
			IsError: true,
		}, nil
	}

	newContent := strings.Replace(content, args.OldText, args.NewText, 1)

	// Atomic write
	dir := filepath.Dir(absPath)
	tmpFile, err := os.CreateTemp(dir, ".tmp-edit-*")
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

	if _, err := tmpFile.WriteString(newContent); err != nil {
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
		Content: fmt.Sprintf("Successfully replaced text in %s", args.Path),
	}, nil
}
