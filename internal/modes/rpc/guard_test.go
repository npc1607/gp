package rpc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gp/internal/ai"
	"gp/internal/tools"
)

type stubTool struct {
	name     string
	executed bool
}

func (s *stubTool) Name() string                 { return s.name }
func (s *stubTool) Description() string          { return "stub tool" }
func (s *stubTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (s *stubTool) Execute(_ *tools.ToolContext, _ json.RawMessage) (*tools.ToolResult, error) {
	s.executed = true
	return &tools.ToolResult{Content: "executed"}, nil
}

type verdictProvider struct {
	verdictJSON string
}

func (v *verdictProvider) Name() string { return "verdict-mock" }
func (v *verdictProvider) StreamChat(_ context.Context, _ *ai.ChatRequest) (<-chan ai.StreamChunk, error) {
	ch := make(chan ai.StreamChunk, 2)
	go func() {
		defer close(ch)
		ch <- ai.StreamChunk{DeltaText: v.verdictJSON}
		ch <- ai.StreamChunk{FinishReason: "stop"}
	}()
	return ch, nil
}

func TestRiskGuardBlocksHighRisk(t *testing.T) {
	bashStub := &stubTool{name: "bash"}
	reg := tools.NewRegistry()
	_ = reg.Register(bashStub)

	var events []Event
	guarded := wrapRegistryWithRiskGuard(reg, &riskGuard{
		prov:       &verdictProvider{verdictJSON: `{"risk_level":"high","allow":false,"reason":"包含 rm 删除操作，存在数据丢失风险","required_permission":"需要管理员权限"}`},
		modelName:  "mock/reviewer",
		userPrompt: "删掉临时目录",
		reqID:      "req-1",
		emit:       func(ev Event) { events = append(events, ev) },
	})

	gt, ok := guarded.Get("bash")
	if !ok {
		t.Fatal("bash tool missing from guarded registry")
	}

	_, err := gt.Execute(&tools.ToolContext{Context: context.Background()}, json.RawMessage(`{"command":"rm -rf /tmp/foo"}`))
	if err == nil || !strings.Contains(err.Error(), "SECURITY_BLOCKED") {
		t.Fatalf("expected SECURITY_BLOCKED error, got: %v", err)
	}
	if bashStub.executed {
		t.Fatal("dangerous bash tool should NOT have been executed")
	}
	if len(events) != 2 || events[0].Event != EventSubagentStart || events[1].Event != EventSubagentEnd {
		t.Fatalf("expected subagent_start and subagent_end events, got: %+v", events)
	}
}

func TestRiskGuardAllowsSafeAndReadOnly(t *testing.T) {
	bashStub := &stubTool{name: "bash"}
	reg := tools.NewRegistry()
	_ = reg.Register(bashStub)

	guarded := wrapRegistryWithRiskGuard(reg, &riskGuard{
		prov:       &verdictProvider{verdictJSON: `{"risk_level":"low","allow":true,"reason":"常规只读或测试操作","required_permission":""}`},
		modelName:  "mock/reviewer",
		userPrompt: "跑一下测试",
		reqID:      "req-2",
	})

	gt, _ := guarded.Get("bash")

	// 1. Read-only fast path
	res, err := gt.Execute(&tools.ToolContext{Context: context.Background()}, json.RawMessage(`{"command":"git status"}`))
	if err != nil || res.Content != "executed" {
		t.Fatalf("expected read-only command to pass, err=%v res=%+v", err, res)
	}

	// 2. Subagent-approved command
	bashStub.executed = false
	res, err = gt.Execute(&tools.ToolContext{Context: context.Background()}, json.RawMessage(`{"command":"go test ./..."}`))
	if err != nil || !bashStub.executed || res.Content != "executed" {
		t.Fatalf("expected approved command to execute, err=%v executed=%v", err, bashStub.executed)
	}
}
