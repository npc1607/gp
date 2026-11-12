package mcp

import (
	"encoding/json"
	"fmt"
)

// Request JSON-RPC 2.0 请求/通知结构体
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *uint64         `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response JSON-RPC 2.0 响应结构体
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *uint64         `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError JSON-RPC 2.0 错误结构体
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("jsonrpc error (code %d): %s", e.Code, e.Message)
}

// ClientInfo 客户端标识信息
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeParams initialize 握手请求参数
type InitializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      ClientInfo     `json:"clientInfo"`
}

// InitializeResult initialize 握手返回结果
type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      map[string]any `json:"serverInfo,omitempty"`
}

// ToolDefinition MCP 工具元数据定义
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ListToolsResult tools/list 返回结果
type ListToolsResult struct {
	Tools      []ToolDefinition `json:"tools"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

// CallToolParams tools/call 请求参数
type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ToolContent MCP 工具返回内容块
type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// CallToolResult tools/call 响应结果
type CallToolResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ServerConfig 单个 MCP Server 配置
type ServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// Config mcp.json 配置文件定义
type Config struct {
	MCPServers map[string]ServerConfig `json:"mcpServers,omitempty"`
	Servers    map[string]ServerConfig `json:"servers,omitempty"`
}

// GetServers 整合并获取所有服务配置
func (c *Config) GetServers() map[string]ServerConfig {
	res := make(map[string]ServerConfig)
	if c == nil {
		return res
	}
	for k, v := range c.MCPServers {
		res[k] = v
	}
	for k, v := range c.Servers {
		res[k] = v
	}
	return res
}
