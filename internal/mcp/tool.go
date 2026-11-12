package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gp/internal/tools"
)

// MCPTool 包装 MCP 远端工具为 gp 标准 tools.Tool 契约接口
type MCPTool struct {
	client     *Client
	serverName string
	toolDef    ToolDefinition
	toolName   string
}

// NewMCPTool 构造 MCP 包装工具实例
func NewMCPTool(serverName string, client *Client, def ToolDefinition) *MCPTool {
	var name string
	if serverName != "" {
		name = fmt.Sprintf("mcp_%s_%s", serverName, def.Name)
	} else {
		name = def.Name
	}

	return &MCPTool{
		client:     client,
		serverName: serverName,
		toolDef:    def,
		toolName:   name,
	}
}

func (m *MCPTool) Name() string {
	return m.toolName
}

func (m *MCPTool) Description() string {
	return m.toolDef.Description
}

func (m *MCPTool) InputSchema() json.RawMessage {
	if len(m.toolDef.InputSchema) > 0 {
		return m.toolDef.InputSchema
	}
	return json.RawMessage(`{"type":"object"}`)
}

func (m *MCPTool) Execute(ctx *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
	if m.client == nil {
		return nil, fmt.Errorf("mcp client not initialized for tool %s", m.toolName)
	}

	reqCtx := context.Background()
	if ctx != nil && ctx.Context != nil {
		reqCtx = ctx.Context
	}

	res, err := m.client.CallTool(reqCtx, m.toolDef.Name, rawArgs)
	if err != nil {
		return &tools.ToolResult{
			Content: fmt.Sprintf("MCP execution error: %v", err),
			IsError: true,
		}, nil
	}

	var textParts []string
	for _, c := range res.Content {
		if c.Text != "" {
			textParts = append(textParts, c.Text)
		}
	}

	output := strings.Join(textParts, "\n")
	return &tools.ToolResult{
		Content: output,
		IsError: res.IsError,
	}, nil
}
