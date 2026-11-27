package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gp/internal/tools"
)

// RegisterTools 向 gp 标准 tools.Registry 注册静态不变的双元工具 ext_describe 与 ext_call
func RegisterTools(reg *tools.Registry, extReg *Registry) error {
	return RegisterFilteredTools(reg, extReg, true, true)
}

// RegisterFilteredTools 按白名单细粒度注册 ext_describe 与 ext_call
func RegisterFilteredTools(reg *tools.Registry, extReg *Registry, allowDescribe, allowCall bool) error {
	if reg == nil || extReg == nil {
		return nil
	}
	if allowDescribe {
		if _, exists := reg.Get("ext_describe"); !exists {
			if err := reg.Register(&ExtDescribeTool{registry: extReg}); err != nil {
				return err
			}
		}
	}
	if allowCall {
		if _, exists := reg.Get("ext_call"); !exists {
			if err := reg.Register(&ExtCallTool{registry: extReg}); err != nil {
				return err
			}
		}
	}
	return nil
}

// ExtDescribeTool Stage 1 元工具：按需加载扩展使用指南与 BM25 检索后的动作 Schema
type ExtDescribeTool struct {
	registry *Registry
}

var _ tools.ReplayableTool = (*ExtDescribeTool)(nil)

func (t *ExtDescribeTool) Name() string { return "ext_describe" }

func (t *ExtDescribeTool) Description() string {
	return "Inspect an external extension's guide and action schemas (supports keyword search via query for large extensions)."
}

func (t *ExtDescribeTool) ReplayMode() tools.ReplayMode { return tools.ReplaySafe }

func (t *ExtDescribeTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"extension": {
				"type": "string",
				"description": "Extension name from [Available Extensions]"
			},
			"query": {
				"type": "string",
				"description": "Optional search keywords to filter actions when an extension exposes many actions"
			},
			"include_guide": {
				"type": "boolean",
				"description": "Whether to include GUIDE.md instructions (default: true when query is empty)"
			},
			"limit": {
				"type": "integer",
				"description": "Max number of matching actions to return (default: 8, max: 20)"
			}
		},
		"required": ["extension"]
	}`)
}

func (t *ExtDescribeTool) Execute(ctx *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
	var args struct {
		Extension    string `json:"extension"`
		Query        string `json:"query"`
		IncludeGuide *bool  `json:"include_guide"`
		Limit        int    `json:"limit"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return &tools.ToolResult{
			Content: formatCodedError(ErrInvalidArgs, fmt.Sprintf("invalid ext_describe arguments: %v", err)),
			IsError: true,
		}, nil
	}
	if strings.TrimSpace(args.Extension) == "" {
		return &tools.ToolResult{
			Content: formatCodedError(ErrInvalidArgs, "missing required field: extension"),
			IsError: true,
		}, nil
	}
	includeGuide := strings.TrimSpace(args.Query) == ""
	if args.IncludeGuide != nil {
		includeGuide = *args.IncludeGuide
	}

	reqCtx := context.Background()
	isSubagent := false
	if ctx != nil {
		if ctx.Context != nil {
			reqCtx = ctx.Context
		}
		isSubagent = ctx.IsSubagent
	}
	out, isErr := t.registry.DescribeForCaller(reqCtx, args.Extension, args.Query, includeGuide, args.Limit, isSubagent)
	return &tools.ToolResult{
		Content: out,
		IsError: isErr,
	}, nil
}

// ExtCallTool Stage 1 统一执行网关：带参数校验、鉴权脱敏、风险门禁与输出防污染裁剪
type ExtCallTool struct {
	registry *Registry
}

var _ tools.ReplayableTool = (*ExtCallTool)(nil)

func (t *ExtCallTool) Name() string { return "ext_call" }

func (t *ExtCallTool) Description() string {
	return "Invoke an action on a registered external extension (call ext_describe first if unsure about arguments)."
}

func (t *ExtCallTool) ReplayMode() tools.ReplayMode { return tools.ReplayInterrupt }

func (t *ExtCallTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"extension": {
				"type": "string",
				"description": "Target extension name"
			},
			"action": {
				"type": "string",
				"description": "Action name within the extension"
			},
			"args": {
				"type": "object",
				"description": "Arguments matching the action schema"
			}
		},
		"required": ["extension", "action"]
	}`)
}

func (t *ExtCallTool) Execute(ctx *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
	var payload struct {
		Extension string         `json:"extension"`
		Action    string         `json:"action"`
		Args      map[string]any `json:"args"`
	}
	// 使用 UseNumber 保留大整数原始字面量，防止 >= 1e6 的 ID/时间戳变成科学计数法
	dec := json.NewDecoder(bytes.NewReader(rawArgs))
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil {
		return &tools.ToolResult{
			Content: formatCodedError(ErrInvalidArgs, fmt.Sprintf("invalid ext_call arguments: %v", err)),
			IsError: true,
		}, nil
	}
	if strings.TrimSpace(payload.Extension) == "" || strings.TrimSpace(payload.Action) == "" {
		return &tools.ToolResult{
			Content: formatCodedError(ErrInvalidArgs, "both extension and action are required"),
			IsError: true,
		}, nil
	}

	reqCtx := context.Background()
	caller := CallContext{}
	if ctx != nil {
		if ctx.Context != nil {
			reqCtx = ctx.Context
		}
		caller.Cwd = ctx.Cwd
		caller.AgentID = ctx.AgentID
		caller.IsSubagent = ctx.IsSubagent
		caller.SessionID = ctx.SessionID
	}

	res := t.registry.Call(reqCtx, payload.Extension, payload.Action, payload.Args, caller)
	return &tools.ToolResult{
		Content: res.Content,
		IsError: res.IsError,
		Details: map[string]any{
			"extension":    payload.Extension,
			"action":       payload.Action,
			"adapter_type": res.AdapterType,
			"risk":         string(res.Risk),
			"duration_ms":  res.Duration.Milliseconds(),
			"truncated":    res.Truncated,
			"spill_path":   res.SpillPath,
			"error_code":   res.ErrorCode,
		},
	}, nil
}
