package js

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/dop251/goja"
	"gp/internal/tools"
)

// Engine 包装 Goja JavaScript 运行环境，并向环境注入 gp 宿主扩展能力
type Engine struct {
	mu       sync.Mutex
	vm       *goja.Runtime
	registry *tools.Registry
	tools    []*JSTool
}

// NewEngine 构造初始化 JS 引擎
func NewEngine(reg *tools.Registry) *Engine {
	e := &Engine{
		vm:       goja.New(),
		registry: reg,
	}

	e.initGlobals()
	return e
}

func (e *Engine) initGlobals() {
	// 1. 注入 console 对象
	consoleObj := e.vm.NewObject()
	_ = consoleObj.Set("log", func(call goja.FunctionCall) goja.Value {
		var parts []any
		for _, arg := range call.Arguments {
			parts = append(parts, arg.Export())
		}
		fmt.Println(parts...)
		return goja.Undefined()
	})
	_ = consoleObj.Set("warn", func(call goja.FunctionCall) goja.Value {
		var parts []any
		for _, arg := range call.Arguments {
			parts = append(parts, arg.Export())
		}
		fmt.Fprintln(os.Stderr, parts...)
		return goja.Undefined()
	})
	_ = consoleObj.Set("error", func(call goja.FunctionCall) goja.Value {
		var parts []any
		for _, arg := range call.Arguments {
			parts = append(parts, arg.Export())
		}
		fmt.Fprintln(os.Stderr, parts...)
		return goja.Undefined()
	})
	_ = e.vm.Set("console", consoleObj)

	// 2. 注入 gp 全局扩展对象
	gpObj := e.vm.NewObject()
	_ = gpObj.Set("registerTool", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			panic(e.vm.ToValue("registerTool expects at least 1 argument"))
		}

		firstArg := call.Arguments[0].ToObject(e.vm)
		var name string
		var desc string
		var schemaRaw json.RawMessage
		var execCallable goja.Callable

		if firstArg != nil {
			if v := firstArg.Get("name"); v != nil && !goja.IsUndefined(v) {
				name = v.String()
			}
			if v := firstArg.Get("description"); v != nil && !goja.IsUndefined(v) {
				desc = v.String()
			}

			// 获取 schema (支持 parameters, inputSchema, schema)
			var schemaVal goja.Value
			if v := firstArg.Get("parameters"); v != nil && !goja.IsUndefined(v) {
				schemaVal = v
			} else if v := firstArg.Get("inputSchema"); v != nil && !goja.IsUndefined(v) {
				schemaVal = v
			} else if v := firstArg.Get("schema"); v != nil && !goja.IsUndefined(v) {
				schemaVal = v
			}
			if schemaVal != nil {
				if b, err := json.Marshal(schemaVal.Export()); err == nil {
					schemaRaw = b
				}
			}

			if v := firstArg.Get("execute"); v != nil && !goja.IsUndefined(v) {
				if fn, ok := goja.AssertFunction(v); ok {
					execCallable = fn
				}
			}
		}

		// 支持双参数模式: gp.registerTool(meta, fn)
		if len(call.Arguments) > 1 {
			if fn, ok := goja.AssertFunction(call.Arguments[1]); ok {
				execCallable = fn
			}
		}

		if name == "" {
			panic(e.vm.ToValue("registerTool: tool 'name' is required"))
		}
		if execCallable == nil {
			panic(e.vm.ToValue(fmt.Sprintf("registerTool(%s): 'execute' function is required", name)))
		}

		tool := NewJSTool(&e.mu, e.vm, name, desc, schemaRaw, execCallable)
		e.tools = append(e.tools, tool)

		if e.registry != nil {
			_ = e.registry.Register(tool)
		}

		return goja.Undefined()
	})
	_ = e.vm.Set("gp", gpObj)
}

// ExecuteScript 执行 JS 脚本源代码
func (e *Engine) ExecuteScript(source, scriptName string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if scriptName == "" {
		scriptName = "<script>"
	}

	_, err := e.vm.RunScript(scriptName, source)
	if err != nil {
		return fmt.Errorf("execute js script (%s): %w", scriptName, err)
	}
	return nil
}

// Tools 返回当前引擎已成功注册的所有 JS 工具
func (e *Engine) Tools() []*JSTool {
	e.mu.Lock()
	defer e.mu.Unlock()

	out := make([]*JSTool, len(e.tools))
	copy(out, e.tools)
	return out
}
