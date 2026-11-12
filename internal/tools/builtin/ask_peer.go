package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"gp/internal/tools"
)

// PeerQueryHandler 点对点问答回调处理函数
type PeerQueryHandler func(ctx context.Context, peerRole, question string) (string, error)

// AskPeerInput ask_peer_agent 工具输入
type AskPeerInput struct {
	PeerRole string `json:"peer_role"`
	Question string `json:"question"`
}

// AskPeerTool A2A 点对点即时问答工具
type AskPeerTool struct {
	handler PeerQueryHandler
	mu      sync.RWMutex
}

// NewAskPeerTool 创建 AskPeerTool 实例
func NewAskPeerTool(handler PeerQueryHandler) *AskPeerTool {
	if handler == nil {
		handler = func(ctx context.Context, peerRole, question string) (string, error) {
			return fmt.Sprintf("peer %s acknowledged question: %s", peerRole, question), nil
		}
	}
	return &AskPeerTool{
		handler: handler,
	}
}

func (t *AskPeerTool) Name() string {
	return "ask_peer_agent"
}

func (t *AskPeerTool) Description() string {
	return "Ask a peer specialized agent (e.g. scout, reviewer, planner) a targeted question during task execution."
}

func (t *AskPeerTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"peer_role": {
				"type": "string",
				"description": "Role of the peer agent to ask (e.g. scout, reviewer, planner)"
			},
			"question": {
				"type": "string",
				"description": "The question to ask the peer agent"
			}
		},
		"required": ["peer_role", "question"]
	}`)
}

// ReplayMode 声明重放策略为 safe (只读点对点咨询无磁盘/系统副作用)
func (t *AskPeerTool) ReplayMode() tools.ReplayMode {
	return tools.ReplaySafe
}

// SetHandler 动态注入点对点问答处理函数
func (t *AskPeerTool) SetHandler(handler PeerQueryHandler) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.handler = handler
}

func (t *AskPeerTool) Execute(ctx *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
	var input AskPeerInput
	if err := json.Unmarshal(rawArgs, &input); err != nil {
		return nil, fmt.Errorf("invalid ask_peer_agent args: %w", err)
	}

	peerRole := strings.TrimSpace(input.PeerRole)
	if peerRole == "" {
		return nil, fmt.Errorf("peer_role cannot be empty")
	}

	question := strings.TrimSpace(input.Question)
	if question == "" {
		return nil, fmt.Errorf("question cannot be empty")
	}

	reqCtx := context.Background()
	if ctx != nil && ctx.Context != nil {
		reqCtx = ctx.Context
	}

	t.mu.RLock()
	handler := t.handler
	t.mu.RUnlock()

	reply, err := handler(reqCtx, peerRole, question)
	if err != nil {
		return nil, fmt.Errorf("peer agent query failed: %w", err)
	}

	return &tools.ToolResult{
		Content: fmt.Sprintf("[%s 的回答]: %s", peerRole, reply),
		IsError: false,
	}, nil
}
