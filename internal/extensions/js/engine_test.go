package js

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gp/internal/tools"
)

func TestJSEngine_RegisterAndExecute(t *testing.T) {
	reg := tools.NewRegistry()
	engine := NewEngine(reg)

	script := `
gp.registerTool({
    name: "str_reverse",
    description: "Reverses given string",
    parameters: {
        type: "object",
        properties: {
            text: { type: "string" }
        },
        required: ["text"]
    },
    execute: function(args, ctx) {
        if (!args.text) {
            throw new Error("text parameter is required");
        }
        return args.text.split("").reverse().join("");
    }
});

gp.registerTool({
    name: "calc_stats",
    description: "Computes simple stats",
}, function(args) {
    return {
        count: args.items ? args.items.length : 0,
        status: "ok"
    };
});
`

	err := engine.ExecuteScript(script, "test_ext.js")
	if err != nil {
		t.Fatalf("ExecuteScript failed: %v", err)
	}

	jsTools := engine.Tools()
	if len(jsTools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(jsTools))
	}

	// 1. 验证 str_reverse
	revTool, ok := reg.Get("str_reverse")
	if !ok {
		t.Fatal("tool str_reverse not registered")
	}
	if revTool.Description() != "Reverses given string" {
		t.Errorf("unexpected description: %s", revTool.Description())
	}

	toolCtx := &tools.ToolContext{
		Context: context.Background(),
		AgentID: "tester",
		Cwd:     "/app",
	}

	res, err := revTool.Execute(toolCtx, json.RawMessage(`{"text":"hello world"}`))
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if res.IsError {
		t.Errorf("unexpected error: %s", res.Content)
	}
	if res.Content != "dlrow olleh" {
		t.Errorf("expected dlrow olleh, got %q", res.Content)
	}

	// 2. 验证 JS 内部异常抛出与安全捕获
	resErr, err := revTool.Execute(toolCtx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute unexpected Go error: %v", err)
	}
	if !resErr.IsError {
		t.Errorf("expected IsError=true when exception thrown, got false")
	}

	// 3. 验证返回对象序列化为 JSON
	statsTool, ok := reg.Get("calc_stats")
	if !ok {
		t.Fatal("tool calc_stats not registered")
	}
	resStats, err := statsTool.Execute(toolCtx, json.RawMessage(`{"items":[1,2,3]}`))
	if err != nil {
		t.Fatalf("Execute calc_stats failed: %v", err)
	}
	if resStats.IsError {
		t.Errorf("unexpected error in stats: %s", resStats.Content)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(resStats.Content), &obj); err != nil {
		t.Fatalf("failed to parse returned JSON: %v, content: %s", err, resStats.Content)
	}
	if obj["status"] != "ok" {
		t.Errorf("expected status ok, got %v", obj["status"])
	}
}

func TestJSEngine_Loader(t *testing.T) {
	tempDir := t.TempDir()
	extDir := filepath.Join(tempDir, ".gp", "extensions")
	if err := os.MkdirAll(extDir, 0755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}

	scriptContent := `
gp.registerTool({
    name: "ping",
    description: "Returns pong",
    execute: function(args, ctx) {
        return "pong from " + ctx.agentId;
    }
});
`
	if err := os.WriteFile(filepath.Join(extDir, "ping.js"), []byte(scriptContent), 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	reg := tools.NewRegistry()
	loaded, err := LoadExtensions(tempDir, "", reg)
	if err != nil {
		t.Fatalf("LoadExtensions failed: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 loaded tool, got %d", len(loaded))
	}

	tInstance, ok := reg.Get("ping")
	if !ok {
		t.Fatal("ping tool not found in registry")
	}

	toolCtx := &tools.ToolContext{
		Context: context.Background(),
		AgentID: "agent-007",
	}
	res, err := tInstance.Execute(toolCtx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if res.Content != "pong from agent-007" {
		t.Errorf("expected 'pong from agent-007', got %q", res.Content)
	}
}

func TestJS_TC_JS_001_DualSyntaxToolRegistration(t *testing.T) {
	fn := "Engine.ExecuteScript"
	reg := tools.NewRegistry()
	engine := NewEngine(reg)

	script := `
// Syntax 1: (metaWithExec)
gp.registerTool({
    name: "tool_syntax1",
    description: "Syntax 1 tool description",
    parameters: {
        type: "object",
        properties: {
            fieldA: { type: "string" }
        },
        required: ["fieldA"]
    },
    execute: function(args) {
        return "result1";
    }
});

// Syntax 2: (def, fn)
gp.registerTool({
    name: "tool_syntax2",
    description: "Syntax 2 tool description",
    parameters: {
        type: "object",
        properties: {
            count: { type: "number" }
        }
    }
}, function(args) {
    return "result2";
});
`
	if err := engine.ExecuteScript(script, "dual_syntax.js"); err != nil {
		t.Fatalf("%s(script=dual_syntax.js): got error %v, want nil", fn, err)
	}

	// Check tool 1
	t1, ok := reg.Get("tool_syntax1")
	if !ok {
		t.Fatalf("reg.Get(%q): got false, want true", "tool_syntax1")
	}
	if got, want := t1.Name(), "tool_syntax1"; got != want {
		t.Fatalf("t1.Name(): got %q, want %q", got, want)
	}
	if got, want := t1.Description(), "Syntax 1 tool description"; got != want {
		t.Fatalf("t1.Description(): got %q, want %q", got, want)
	}
	if !strings.Contains(string(t1.InputSchema()), "fieldA") {
		t.Fatalf("t1.InputSchema(): got %s, want schema containing 'fieldA'", string(t1.InputSchema()))
	}

	// Check tool 2
	t2, ok := reg.Get("tool_syntax2")
	if !ok {
		t.Fatalf("reg.Get(%q): got false, want true", "tool_syntax2")
	}
	if got, want := t2.Name(), "tool_syntax2"; got != want {
		t.Fatalf("t2.Name(): got %q, want %q", got, want)
	}
	if got, want := t2.Description(), "Syntax 2 tool description"; got != want {
		t.Fatalf("t2.Description(): got %q, want %q", got, want)
	}
	if !strings.Contains(string(t2.InputSchema()), "count") {
		t.Fatalf("t2.InputSchema(): got %s, want schema containing 'count'", string(t2.InputSchema()))
	}
}

func TestJS_TC_JS_002_ParamParsingAndJSONSerialization(t *testing.T) {
	fn := "JSTool.Execute"
	reg := tools.NewRegistry()
	engine := NewEngine(reg)

	script := `
gp.registerTool({
    name: "complex_calc",
    description: "Returns complex calculation result",
    execute: function(args) {
        return {
            res: args.x,
            doubled: args.x * 2,
            meta: {
                flag: true
            }
        };
    }
});
`
	if err := engine.ExecuteScript(script, "complex.js"); err != nil {
		t.Fatalf("engine.ExecuteScript: got error %v, want nil", err)
	}

	tool, ok := reg.Get("complex_calc")
	if !ok {
		t.Fatalf("reg.Get(%q): got false, want true", "complex_calc")
	}

	toolCtx := &tools.ToolContext{
		Context: context.Background(),
		AgentID: "agent-1",
	}
	inputJSON := `{"x":1}`
	res, err := tool.Execute(toolCtx, json.RawMessage(inputJSON))
	if err != nil {
		t.Fatalf("%s(args=%s): got error %v, want nil", fn, inputJSON, err)
	}
	if got, want := res.IsError, false; got != want {
		t.Fatalf("%s(args=%s): got IsError %v, want %v", fn, inputJSON, got, want)
	}

	var data struct {
		Res     int `json:"res"`
		Doubled int `json:"doubled"`
		Meta    struct {
			Flag bool `json:"flag"`
		} `json:"meta"`
	}
	if err := json.Unmarshal([]byte(res.Content), &data); err != nil {
		t.Fatalf("%s(args=%s): got unmarshal error %v on content %q, want valid JSON", fn, inputJSON, err, res.Content)
	}
	if got, want := data.Res, 1; got != want {
		t.Fatalf("%s(args=%s): got res %d, want %d", fn, inputJSON, got, want)
	}
	if got, want := data.Doubled, 2; got != want {
		t.Fatalf("%s(args=%s): got doubled %d, want %d", fn, inputJSON, got, want)
	}
	if got, want := data.Meta.Flag, true; got != want {
		t.Fatalf("%s(args=%s): got flag %v, want %v", fn, inputJSON, got, want)
	}
}

func TestJS_TC_JS_003_RuntimeExceptionSafety(t *testing.T) {
	fn := "JSTool.Execute"
	reg := tools.NewRegistry()
	engine := NewEngine(reg)

	script := `
gp.registerTool({
    name: "thrower",
    description: "Throws an intentional exception",
    execute: function(args) {
        throw new Error("test failure: invalid operation");
    }
});
`
	if err := engine.ExecuteScript(script, "thrower.js"); err != nil {
		t.Fatalf("engine.ExecuteScript: got error %v, want nil", err)
	}

	tool, ok := reg.Get("thrower")
	if !ok {
		t.Fatalf("reg.Get(%q): got false, want true", "thrower")
	}

	toolCtx := &tools.ToolContext{
		Context: context.Background(),
	}
	inputJSON := `{}`
	res, err := tool.Execute(toolCtx, json.RawMessage(inputJSON))
	if err != nil {
		t.Fatalf("%s(args=%s): got Go error %v, want nil (isolated)", fn, inputJSON, err)
	}
	if got, want := res.IsError, true; got != want {
		t.Fatalf("%s(args=%s): got IsError %v, want %v", fn, inputJSON, got, want)
	}
	wantSubstring := "Error: test failure: invalid operation"
	if !strings.Contains(res.Content, wantSubstring) {
		t.Fatalf("%s(args=%s): got Content %q, want containing %q", fn, inputJSON, res.Content, wantSubstring)
	}
}

func TestJS_TC_JS_004_GojaConcurrencySafety(t *testing.T) {
	fn := "JSTool.Execute"
	reg := tools.NewRegistry()
	engine := NewEngine(reg)

	script := `
gp.registerTool({
    name: "concurrent_math",
    description: "Computes sum in JS",
    execute: function(args) {
        var sum = 0;
        for (var i = 0; i <= args.n; i++) {
            sum += i;
        }
        return sum;
    }
});
`
	if err := engine.ExecuteScript(script, "concur.js"); err != nil {
		t.Fatalf("engine.ExecuteScript: got error %v, want nil", err)
	}

	tool, ok := reg.Get("concurrent_math")
	if !ok {
		t.Fatalf("reg.Get(%q): got false, want true", "concurrent_math")
	}

	const goroutines = 20
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	results := make([]string, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			toolCtx := &tools.ToolContext{
				Context: context.Background(),
				AgentID: fmt.Sprintf("agent-%d", idx),
			}
			inputJSON := fmt.Sprintf(`{"n":%d}`, idx)
			res, err := tool.Execute(toolCtx, json.RawMessage(inputJSON))
			if err != nil {
				errs[idx] = err
				return
			}
			if res.IsError {
				errs[idx] = fmt.Errorf("unexpected tool error: %s", res.Content)
				return
			}
			results[idx] = res.Content
		}(i)
	}

	wg.Wait()

	for idx := 0; idx < goroutines; idx++ {
		if errs[idx] != nil {
			t.Fatalf("%s(goroutine=%d): got error %v, want nil", fn, idx, errs[idx])
		}
		expectedSum := idx * (idx + 1) / 2
		wantStr := fmt.Sprintf("%d", expectedSum)
		if got, want := results[idx], wantStr; got != want {
			t.Fatalf("%s(goroutine=%d, n=%d): got %q, want %q", fn, idx, idx, got, want)
		}
	}
}

func TestJS_TC_JS_005_DirectoryScanAndLoad(t *testing.T) {
	fn := "LoadExtensions"
	homeDir := t.TempDir()
	cwdDir := t.TempDir()

	// 1. homeDir/.gp/extensions
	globalExtDir := filepath.Join(homeDir, ".gp", "extensions")
	if err := os.MkdirAll(globalExtDir, 0755); err != nil {
		t.Fatalf("mkdir global ext: got error %v, want nil", err)
	}
	globalScript := `gp.registerTool({ name: "global_tool", description: "Global tool", execute: function() { return "global_ok"; } });`
	if err := os.WriteFile(filepath.Join(globalExtDir, "tool1.js"), []byte(globalScript), 0644); err != nil {
		t.Fatalf("write global tool1.js: got error %v, want nil", err)
	}
	// Non-js file to ignore
	if err := os.WriteFile(filepath.Join(globalExtDir, "readme.txt"), []byte("this is not js"), 0644); err != nil {
		t.Fatalf("write readme.txt: got error %v, want nil", err)
	}

	// 2. cwdDir/.gp/extensions
	localExtDir := filepath.Join(cwdDir, ".gp", "extensions")
	if err := os.MkdirAll(localExtDir, 0755); err != nil {
		t.Fatalf("mkdir local ext: got error %v, want nil", err)
	}
	localScript := `gp.registerTool({ name: "local_tool", description: "Local tool", execute: function() { return "local_ok"; } });`
	if err := os.WriteFile(filepath.Join(localExtDir, "tool2.js"), []byte(localScript), 0644); err != nil {
		t.Fatalf("write local tool2.js: got error %v, want nil", err)
	}
	// Syntax error script (should be skipped with warning, not crash)
	brokenScript := `function ( { broken syntax !!!`
	if err := os.WriteFile(filepath.Join(localExtDir, "broken.js"), []byte(brokenScript), 0644); err != nil {
		t.Fatalf("write broken.js: got error %v, want nil", err)
	}

	reg := tools.NewRegistry()
	loaded, err := LoadExtensions(cwdDir, homeDir, reg)
	if err != nil {
		t.Fatalf("%s(cwd=%q, home=%q): got error %v, want nil", fn, cwdDir, homeDir, err)
	}

	if got, want := len(loaded), 2; got != want {
		t.Fatalf("%s(cwd=%q, home=%q): got %d loaded tools, want %d", fn, cwdDir, homeDir, got, want)
	}

	if _, ok := reg.Get("global_tool"); !ok {
		t.Fatalf("reg.Get(%q): got false, want true", "global_tool")
	}
	if _, ok := reg.Get("local_tool"); !ok {
		t.Fatalf("reg.Get(%q): got false, want true", "local_tool")
	}
	if _, ok := reg.Get("readme.txt"); ok {
		t.Fatalf("reg.Get(%q): got true, want false (non-js file should not be registered)", "readme.txt")
	}
}
