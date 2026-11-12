package js

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/dop251/goja"
	"gp/internal/tools"
)

// JSTool 将 JavaScript 函数适配为 gp 内部的标准 tools.Tool 接口
type JSTool struct {
	mu          *sync.Mutex
	vm          *goja.Runtime
	name        string
	description string
	schema      json.RawMessage
	fn          goja.Callable
}

// NewJSTool 构造 JavaScript 工具
func NewJSTool(mu *sync.Mutex, vm *goja.Runtime, name, desc string, schema json.RawMessage, fn goja.Callable) *JSTool {
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	return &JSTool{
		mu:          mu,
		vm:          vm,
		name:        name,
		description: desc,
		schema:      schema,
		fn:          fn,
	}
}

func (t *JSTool) Name() string {
	return t.name
}

func (t *JSTool) Description() string {
	return t.description
}

func (t *JSTool) InputSchema() json.RawMessage {
	return t.schema
}

func (t *JSTool) Execute(ctx *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.fn == nil {
		return &tools.ToolResult{
			Content: "JavaScript tool handler is undefined",
			IsError: true,
		}, nil
	}

	var parsedArgs any
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &parsedArgs); err != nil {
			return &tools.ToolResult{
				Content: fmt.Sprintf("invalid arguments JSON: %v", err),
				IsError: true,
			}, nil
		}
	} else {
		parsedArgs = map[string]any{}
	}

	jsArgs := t.vm.ToValue(parsedArgs)

	jsCtx := t.vm.NewObject()
	if ctx != nil {
		_ = jsCtx.Set("cwd", ctx.Cwd)
		_ = jsCtx.Set("agentId", ctx.AgentID)
	}

	resVal, err := t.fn(goja.Undefined(), jsArgs, jsCtx)
	if err != nil {
		return &tools.ToolResult{
			Content: fmt.Sprintf("JS execution exception: %v", err),
			IsError: true,
		}, nil
	}

	if resVal == nil || goja.IsUndefined(resVal) || goja.IsNull(resVal) {
		return &tools.ToolResult{
			Content: "",
			IsError: false,
		}, nil
	}

	exported := resVal.Export()
	switch v := exported.(type) {
	case string:
		return &tools.ToolResult{
			Content: v,
			IsError: false,
		}, nil
	case map[string]any, []any:
		b, err := json.Marshal(v)
		if err != nil {
			return &tools.ToolResult{
				Content: fmt.Sprint(v),
				IsError: false,
			}, nil
		}
		return &tools.ToolResult{
			Content: string(b),
			IsError: false,
		}, nil
	default:
		return &tools.ToolResult{
			Content: fmt.Sprint(v),
			IsError: false,
		}, nil
	}
}
