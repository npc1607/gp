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
	_ tools.Tool           = (*ReadTool)(nil)
	_ tools.ReplayableTool = (*ReadTool)(nil)
)

const (
	maxReadLines = 2000
	maxReadBytes = 50 * 1024 // 50KB
)

// ReadTool 读取文件内容，支持分页、行号格式化与防爆截断
type ReadTool struct{}

func (r *ReadTool) ReplayMode() tools.ReplayMode {
	return tools.ReplaySafe
}

type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

func (r *ReadTool) Name() string {
	return "read"
}

func (r *ReadTool) Description() string {
	return "Read file contents with line numbers and pagination support."
}

func (r *ReadTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "Path to the file to read (relative or absolute)"
			},
			"offset": {
				"type": "integer",
				"description": "Line number to start reading from (1-indexed, default 1)"
			},
			"limit": {
				"type": "integer",
				"description": "Maximum number of lines to read"
			}
		},
		"required": ["path"]
	}`)
}

func (r *ReadTool) Execute(ctx *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
	var args readArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return &tools.ToolResult{
			Content: fmt.Sprintf("invalid arguments for read tool: %v", err),
			IsError: true,
		}, nil
	}

	if strings.TrimSpace(args.Path) == "" {
		return &tools.ToolResult{
			Content: "path is required",
			IsError: true,
		}, nil
	}

	targetPath := args.Path
	if !filepath.IsAbs(targetPath) {
		cwd := "."
		if ctx != nil && ctx.Cwd != "" {
			cwd = ctx.Cwd
		}
		targetPath = filepath.Join(cwd, targetPath)
	}

	data, err := os.ReadFile(targetPath)
	if err != nil {
		return &tools.ToolResult{
			Content: fmt.Sprintf("failed to read file: %v", err),
			IsError: true,
		}, nil
	}

	if len(data) == 0 {
		return &tools.ToolResult{
			Content: "(file is empty)",
		}, nil
	}

	raw := string(data)
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")

	lines := strings.Split(raw, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" && strings.HasSuffix(raw, "\n") {
		lines = lines[:len(lines)-1]
	}
	totalLines := len(lines)
	if totalLines == 0 {
		return &tools.ToolResult{
			Content: "(file is empty)",
		}, nil
	}

	startLine := args.Offset
	if startLine <= 0 {
		startLine = 1
	}
	if startLine > totalLines {
		return &tools.ToolResult{
			Content: fmt.Sprintf("offset %d out of range (file has %d lines)", startLine, totalLines),
			IsError: true,
		}, nil
	}

	endLine := totalLines
	if args.Limit > 0 {
		reqEnd := startLine + args.Limit - 1
		if reqEnd < endLine {
			endLine = reqEnd
		}
	}

	var sb strings.Builder
	bytesWritten := 0
	linesWritten := 0
	lastLineRendered := startLine - 1
	truncated := false

	for lineNum := startLine; lineNum <= endLine; lineNum++ {
		if linesWritten >= maxReadLines {
			truncated = true
			break
		}

		lineStr := fmt.Sprintf("%6d | %s\n", lineNum, lines[lineNum-1])
		if bytesWritten+len(lineStr) > maxReadBytes && linesWritten > 0 {
			truncated = true
			break
		}

		sb.WriteString(lineStr)
		bytesWritten += len(lineStr)
		linesWritten++
		lastLineRendered = lineNum
	}

	// Add notice if truncated or partial
	if truncated || lastLineRendered < totalLines {
		sb.WriteString(fmt.Sprintf("\n[Showing lines %d-%d of %d. Use offset=%d to continue.]\n",
			startLine, lastLineRendered, totalLines, lastLineRendered+1))
	}

	return &tools.ToolResult{
		Content: sb.String(),
	}, nil
}
