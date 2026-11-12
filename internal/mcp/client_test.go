package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"gp/internal/tools"
)

// mockMCPPeer 模拟简易 MCP 服务端
type mockMCPPeer struct {
	reader *bufio.Reader
	writer io.WriteCloser
}

func newMockPipe() (*Client, *mockMCPPeer) {
	clientRead, serverWrite := io.Pipe()
	serverRead, clientWrite := io.Pipe()

	client := NewClientFromReadWriter(clientRead, clientWrite, clientRead)
	peer := &mockMCPPeer{
		reader: bufio.NewReader(serverRead),
		writer: serverWrite,
	}
	return client, peer
}

func (p *mockMCPPeer) readRequest() (*Request, error) {
	line, err := p.reader.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

func (p *mockMCPPeer) sendResponse(id *uint64, result any, rpcErr *RPCError) error {
	var rawRes json.RawMessage
	if result != nil {
		b, err := json.Marshal(result)
		if err != nil {
			return err
		}
		rawRes = b
	}
	resp := Response{
		JSONRPC: "2.0",
		ID:      id,
		Result:  rawRes,
		Error:   rpcErr,
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = p.writer.Write(data)
	return err
}

func TestMCPClient_InitializeAndCall(t *testing.T) {
	client, peer := newMockPipe()
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 启动 mock peer 消息循环
	go func() {
		for {
			req, err := peer.readRequest()
			if err != nil {
				return
			}
			switch req.Method {
			case "initialize":
				_ = peer.sendResponse(req.ID, InitializeResult{
					ProtocolVersion: "2024-11-05",
					Capabilities:    map[string]any{"tools": map[string]any{}},
					ServerInfo:      map[string]any{"name": "mock-server", "version": "1.0"},
				}, nil)
			case "notifications/initialized":
				// notification, do not send response
			case "tools/list":
				_ = peer.sendResponse(req.ID, ListToolsResult{
					Tools: []ToolDefinition{
						{
							Name:        "calculator",
							Description: "A calculator tool",
							InputSchema: json.RawMessage(`{"type":"object","properties":{"expr":{"type":"string"}}}`),
						},
					},
				}, nil)
			case "tools/call":
				var callParams CallToolParams
				_ = json.Unmarshal(req.Params, &callParams)
				_ = peer.sendResponse(req.ID, CallToolResult{
					Content: []ToolContent{
						{Type: "text", Text: "Result: 42"},
					},
					IsError: false,
				}, nil)
			}
		}
	}()

	// 1. 测试 Initialize
	initRes, err := client.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	if initRes.ProtocolVersion != "2024-11-05" {
		t.Errorf("unexpected protocol version: %s", initRes.ProtocolVersion)
	}

	// 2. 测试 ListTools
	toolDefs, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	if len(toolDefs) != 1 || toolDefs[0].Name != "calculator" {
		t.Fatalf("unexpected tools list: %+v", toolDefs)
	}

	// 3. 测试 CallTool
	callRes, err := client.CallTool(ctx, "calculator", json.RawMessage(`{"expr":"6*7"}`))
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if len(callRes.Content) == 0 || callRes.Content[0].Text != "Result: 42" {
		t.Fatalf("unexpected call result: %+v", callRes)
	}

	// 4. 测试 MCPTool 适配器
	mcpTool := NewMCPTool("demo", client, toolDefs[0])
	if mcpTool.Name() != "mcp_demo_calculator" {
		t.Errorf("expected tool name mcp_demo_calculator, got %s", mcpTool.Name())
	}
	if mcpTool.Description() != "A calculator tool" {
		t.Errorf("unexpected description: %s", mcpTool.Description())
	}

	toolCtx := &tools.ToolContext{
		Context: ctx,
		AgentID: "agent-1",
		Cwd:     "/test",
	}
	tr, err := mcpTool.Execute(toolCtx, json.RawMessage(`{"expr":"6*7"}`))
	if err != nil {
		t.Fatalf("MCPTool.Execute failed: %v", err)
	}
	if tr.IsError {
		t.Errorf("expected IsError=false, got true")
	}
	if tr.Content != "Result: 42" {
		t.Errorf("expected 'Result: 42', got %q", tr.Content)
	}
}

func TestMCPClient_RPCError(t *testing.T) {
	client, peer := newMockPipe()
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		req, err := peer.readRequest()
		if err != nil {
			return
		}
		_ = peer.sendResponse(req.ID, nil, &RPCError{
			Code:    -32601,
			Message: "Method not found",
		})
	}()

	_, err := client.Call(ctx, "unknown_method", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	rpcErr, ok := err.(*RPCError)
	if !ok {
		t.Fatalf("expected *RPCError, got %T: %v", err, err)
	}
	if rpcErr.Code != -32601 {
		t.Errorf("expected code -32601, got %d", rpcErr.Code)
	}
}

func TestMCP_TC_MCP_001_InitializeAndHandshake(t *testing.T) {
	fn := "Client.Initialize"
	client, peer := newMockPipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = peer.writer.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	notifCh := make(chan string, 1)

	go func() {
		for {
			req, err := peer.readRequest()
			if err != nil {
				return
			}
			switch req.Method {
			case "initialize":
				_ = peer.sendResponse(req.ID, InitializeResult{
					ProtocolVersion: "2024-11-05",
					Capabilities:    map[string]any{"tools": map[string]any{}},
					ServerInfo:      map[string]any{"name": "mock-mcp", "version": "1.0"},
				}, nil)
			case "notifications/initialized":
				notifCh <- req.Method
				return
			}
		}
	}()

	initRes, err := client.Initialize(ctx)
	if err != nil {
		t.Fatalf("%s(ctx): got error %v, want nil", fn, err)
	}

	wantVersion := "2024-11-05"
	if got, want := initRes.ProtocolVersion, wantVersion; got != want {
		t.Fatalf("%s(ctx): got protocol version %q, want %q", fn, got, want)
	}

	select {
	case method := <-notifCh:
		if got, want := method, "notifications/initialized"; got != want {
			t.Fatalf("%s(ctx): got notification %q, want %q", fn, got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("%s(ctx): timed out waiting for notifications/initialized", fn)
	}
}

func TestMCP_TC_MCP_002_ListToolsAndNameAdaptation(t *testing.T) {
	fn := "Client.ListTools"
	client, peer := newMockPipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = peer.writer.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	expectedSchema := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)
	go func() {
		for {
			req, err := peer.readRequest()
			if err != nil {
				return
			}
			if req.Method == "tools/list" {
				_ = peer.sendResponse(req.ID, ListToolsResult{
					Tools: []ToolDefinition{
						{
							Name:        "get_weather",
							Description: "Fetches current weather for a city",
							InputSchema: expectedSchema,
						},
					},
				}, nil)
				return
			}
		}
	}()

	toolDefs, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("%s(ctx): got error %v, want nil", fn, err)
	}
	if got, want := len(toolDefs), 1; got != want {
		t.Fatalf("%s(ctx): got %d tool definitions, want %d", fn, got, want)
	}
	if got, want := toolDefs[0].Name, "get_weather"; got != want {
		t.Fatalf("%s(ctx): got tool name %q, want %q", fn, got, want)
	}

	// 1. MCPTool with explicit server name
	serverName := "weather_service"
	mcpTool := NewMCPTool(serverName, client, toolDefs[0])
	wantToolName := "mcp_weather_service_get_weather"
	if got, want := mcpTool.Name(), wantToolName; got != want {
		t.Fatalf("NewMCPTool(serverName=%q).Name(): got %q, want %q", serverName, got, want)
	}
	if got, want := mcpTool.Description(), "Fetches current weather for a city"; got != want {
		t.Fatalf("NewMCPTool(serverName=%q).Description(): got %q, want %q", serverName, got, want)
	}
	if got, want := string(mcpTool.InputSchema()), string(expectedSchema); got != want {
		t.Fatalf("NewMCPTool(serverName=%q).InputSchema(): got %s, want %s", serverName, got, want)
	}

	// 2. MCPTool without server name fallback
	bareTool := NewMCPTool("", client, toolDefs[0])
	if got, want := bareTool.Name(), "get_weather"; got != want {
		t.Fatalf("NewMCPTool(serverName=\"\").Name(): got %q, want %q", got, want)
	}
}

func TestMCP_TC_MCP_003_ExecuteToolAndParamPassthrough(t *testing.T) {
	fn := "MCPTool.Execute"
	client, peer := newMockPipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = peer.writer.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	receivedArgsCh := make(chan string, 1)
	go func() {
		for {
			req, err := peer.readRequest()
			if err != nil {
				return
			}
			if req.Method == "tools/call" {
				var params CallToolParams
				_ = json.Unmarshal(req.Params, &params)
				receivedArgsCh <- string(params.Arguments)
				_ = peer.sendResponse(req.ID, CallToolResult{
					Content: []ToolContent{
						{Type: "text", Text: "res"},
					},
					IsError: false,
				}, nil)
				return
			}
		}
	}()

	def := ToolDefinition{
		Name:        "echo",
		Description: "Echo back input",
	}
	mcpTool := NewMCPTool("srv", client, def)

	reg := tools.NewRegistry()
	if err := reg.Register(mcpTool); err != nil {
		t.Fatalf("reg.Register(): got error %v, want nil", err)
	}

	targetTool, ok := reg.Get("mcp_srv_echo")
	if !ok {
		t.Fatalf("reg.Get(%q): got false, want true", "mcp_srv_echo")
	}

	toolCtx := &tools.ToolContext{
		Context: ctx,
		AgentID: "agent-alpha",
		Cwd:     "/work",
	}
	inputJSON := `{"k":"v"}`
	res, err := targetTool.Execute(toolCtx, json.RawMessage(inputJSON))
	if err != nil {
		t.Fatalf("%s(args=%s): got error %v, want nil", fn, inputJSON, err)
	}
	if got, want := res.IsError, false; got != want {
		t.Fatalf("%s(args=%s): got IsError %v, want %v", fn, inputJSON, got, want)
	}
	if got, want := res.Content, "res"; got != want {
		t.Fatalf("%s(args=%s): got Content %q, want %q", fn, inputJSON, got, want)
	}

	select {
	case received := <-receivedArgsCh:
		var parsedMap map[string]string
		if err := json.Unmarshal([]byte(received), &parsedMap); err != nil {
			t.Fatalf("%s(args=%s): mock peer received invalid JSON %q: %v", fn, inputJSON, received, err)
		}
		if got, want := parsedMap["k"], "v"; got != want {
			t.Fatalf("%s(args=%s): mock peer received k=%q, want %q", fn, inputJSON, got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("%s(args=%s): timed out waiting for mock peer to receive call", fn, inputJSON)
	}
}

func TestMCP_TC_MCP_004_RemoteExecutionErrorIsolation(t *testing.T) {
	fn := "MCPTool.Execute"
	client, peer := newMockPipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = peer.writer.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() {
		for {
			req, err := peer.readRequest()
			if err != nil {
				return
			}
			if req.Method == "tools/call" {
				_ = peer.sendResponse(req.ID, CallToolResult{
					Content: []ToolContent{
						{Type: "text", Text: "err_msg"},
					},
					IsError: true,
				}, nil)
				return
			}
		}
	}()

	def := ToolDefinition{
		Name:        "faulty",
		Description: "Fails intentionally",
	}
	mcpTool := NewMCPTool("srv", client, def)

	toolCtx := &tools.ToolContext{
		Context: ctx,
		AgentID: "agent-1",
	}
	res, err := mcpTool.Execute(toolCtx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("%s(args={}): got Go error %v, want nil (isolated)", fn, err)
	}
	if got, want := res.IsError, true; got != want {
		t.Fatalf("%s(args={}): got IsError %v, want %v", fn, got, want)
	}
	if got, want := res.Content, "err_msg"; got != want {
		t.Fatalf("%s(args={}): got Content %q, want %q", fn, got, want)
	}
}

func TestMCP_TC_MCP_005_PipeBreakAndCleanup(t *testing.T) {
	fn := "Client.Call"
	client, peer := newMockPipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = peer.writer.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	const concurrentCalls = 5
	var wg sync.WaitGroup
	errs := make([]error, concurrentCalls)

	// Don't respond, instead simulate abrupt pipe break after calls are queued
	for i := 0; i < concurrentCalls; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := client.Call(ctx, "tools/call", map[string]any{"call_index": idx})
			errs[idx] = err
		}(i)
	}

	// Give goroutines a brief moment to write and enter pending state
	time.Sleep(50 * time.Millisecond)

	// Abruptly close peer writer (causes EOF on client stdout)
	_ = peer.writer.Close()

	wg.Wait()

	for idx, err := range errs {
		if err == nil {
			t.Fatalf("%s(idx=%d): got nil error, want pipe break/EOF error", fn, idx)
		}
	}

	// Verify pending table is cleared
	client.mu.Lock()
	pendingLen := len(client.pending)
	client.mu.Unlock()

	if got, want := pendingLen, 0; got != want {
		t.Fatalf("client.pending size after pipe break: got %d, want %d", got, want)
	}
}
