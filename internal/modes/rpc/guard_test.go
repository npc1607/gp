package rpc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
	calls       int
}

func (v *verdictProvider) Name() string { return "verdict-mock" }
func (v *verdictProvider) StreamChat(_ context.Context, _ *ai.ChatRequest) (<-chan ai.StreamChunk, error) {
	v.calls++
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
		userPrompt: "删掉业务目录",
		reqID:      "req-1",
		emit:       func(ev Event) { events = append(events, ev) },
		cache:      newAuditCache(5 * time.Minute),
	})

	gt, ok := guarded.Get("bash")
	if !ok {
		t.Fatal("bash tool missing from guarded registry")
	}

	_, err := gt.Execute(&tools.ToolContext{Context: context.Background()}, json.RawMessage(`{"command":"rm -rf /Users/project/prod-data"}`))
	if err == nil || !strings.Contains(err.Error(), "SECURITY_BLOCKED") {
		t.Fatalf("expected SECURITY_BLOCKED error, got: %v", err)
	}
	if bashStub.executed {
		t.Fatal("dangerous bash tool should NOT have been executed")
	}

	// Ambiguous command goes through LLM reviewer
	_, err = gt.Execute(&tools.ToolContext{Context: context.Background()}, json.RawMessage(`{"command":"python3 dangerous.py"}`))
	if err == nil || !strings.Contains(err.Error(), "SECURITY_BLOCKED") {
		t.Fatalf("expected SECURITY_BLOCKED for python3, got: %v", err)
	}
	if len(events) != 2 || events[0].Event != EventSubagentStart || events[1].Event != EventSubagentEnd {
		t.Fatalf("expected subagent_start and subagent_end events for LLM-reviewed command, got: %+v", events)
	}
}

func TestRiskGuardAllowsSafeTempAndCaches(t *testing.T) {
	bashStub := &stubTool{name: "bash"}
	writeStub := &stubTool{name: "write"}
	reg := tools.NewRegistry()
	_ = reg.Register(bashStub)
	_ = reg.Register(writeStub)

	vp := &verdictProvider{verdictJSON: `{"risk_level":"low","allow":true,"reason":"常规测试操作","required_permission":""}`}
	guarded := wrapRegistryWithRiskGuard(reg, &riskGuard{
		prov:       vp,
		modelName:  "mock/reviewer",
		userPrompt: "跑一下测试",
		reqID:      "req-2",
		cache:      newAuditCache(5 * time.Minute),
	})

	bashTool, _ := guarded.Get("bash")
	writeTool, _ := guarded.Get("write")

	// 1. Read-only fast path (0 LLM calls)
	res, err := bashTool.Execute(&tools.ToolContext{Context: context.Background()}, json.RawMessage(`{"command":"git status"}`))
	if err != nil || res.Content != "executed" {
		t.Fatalf("expected read-only command to pass, err=%v res=%+v", err, res)
	}

	// 2. Temp directory fast path for rm and write (0 LLM calls)
	bashStub.executed = false
	if _, err := bashTool.Execute(&tools.ToolContext{Context: context.Background()}, json.RawMessage(`{"command":"rm -rf /tmp/fsrobot-test"}`)); err != nil || !bashStub.executed {
		t.Fatalf("expected /tmp rm to pass fast-path, err=%v executed=%v", err, bashStub.executed)
	}
	if _, err := writeTool.Execute(&tools.ToolContext{Context: context.Background()}, json.RawMessage(`{"file_path":"/tmp/note.txt","content":"hi"}`)); err != nil || !writeStub.executed {
		t.Fatalf("expected /tmp write to pass fast-path, err=%v executed=%v", err, writeStub.executed)
	}
	if vp.calls != 0 {
		t.Fatalf("expected 0 audit LLM calls for fast-path, got %d", vp.calls)
	}

	// 3. Ambiguous command hits LLM once, then hits cache on second call
	bashStub.executed = false
	if _, err := bashTool.Execute(&tools.ToolContext{Context: context.Background()}, json.RawMessage(`{"command":"python3 safe_check.py"}`)); err != nil || !bashStub.executed {
		t.Fatalf("expected approved command to execute, err=%v executed=%v", err, bashStub.executed)
	}
	bashStub.executed = false
	if _, err := bashTool.Execute(&tools.ToolContext{Context: context.Background()}, json.RawMessage(`{"command":"python3 safe_check.py"}`)); err != nil || !bashStub.executed {
		t.Fatalf("expected cached command to execute, err=%v executed=%v", err, bashStub.executed)
	}
	if vp.calls != 1 {
		t.Fatalf("expected exactly 1 audit LLM call due to cache, got %d", vp.calls)
	}
}

func TestRiskGuardWorkspaceBoundary(t *testing.T) {
	workDir := t.TempDir()
	readStub := &stubTool{name: "read"}
	writeStub := &stubTool{name: "write"}
	bashStub := &stubTool{name: "bash"}
	reg := tools.NewRegistry()
	_ = reg.Register(readStub)
	_ = reg.Register(writeStub)
	_ = reg.Register(bashStub)

	var approvalCalls int
	guarded := wrapRegistryWithRiskGuard(reg, &riskGuard{
		prov:          &verdictProvider{verdictJSON: `{"risk_level":"low","allow":true,"can_approve":false,"reason":"ok"}`},
		modelName:     "mock/reviewer",
		workDir:       workDir,
		allowApproval: true,
		requestApproval: func(_ context.Context, _ string, _ string, _ json.RawMessage) (bool, error) {
			approvalCalls++
			return false, nil
		},
		cache: newAuditCache(5 * time.Minute),
	})

	rt, _ := guarded.Get("read")
	wt, _ := guarded.Get("write")
	bt, _ := guarded.Get("bash")

	// 1. Inside workDir & /tmp -> 0ms pass
	if _, err := rt.Execute(&tools.ToolContext{Context: context.Background(), Cwd: workDir}, json.RawMessage(`{"path":"main.go"}`)); err != nil || !readStub.executed {
		t.Fatalf("expected read inside workDir to pass, err=%v", err)
	}
	readStub.executed = false
	if _, err := rt.Execute(&tools.ToolContext{Context: context.Background(), Cwd: workDir}, json.RawMessage(`{"path":"/tmp/img.png"}`)); err != nil || !readStub.executed {
		t.Fatalf("expected read inside /tmp to pass, err=%v", err)
	}

	// 2. Outside workDir -> must trigger admin approval
	readStub.executed = false
	if _, err := rt.Execute(&tools.ToolContext{Context: context.Background(), Cwd: workDir}, json.RawMessage(`{"path":"/etc/passwd"}`)); err == nil || !strings.Contains(err.Error(), "SECURITY_BLOCKED") {
		t.Fatalf("expected read /etc/passwd to require approval and block when rejected, got: %v", err)
	}
	if readStub.executed {
		t.Fatal("read /etc/passwd should NOT execute when rejected")
	}

	if _, err := wt.Execute(&tools.ToolContext{Context: context.Background(), Cwd: workDir}, json.RawMessage(`{"path":"~/.ssh/authorized_keys","content":"x"}`)); err == nil || !strings.Contains(err.Error(), "SECURITY_BLOCKED") {
		t.Fatalf("expected write outside workDir to require approval, got: %v", err)
	}

	if _, err := bt.Execute(&tools.ToolContext{Context: context.Background(), Cwd: workDir}, json.RawMessage(`{"command":"cat /etc/hosts"}`)); err == nil || !strings.Contains(err.Error(), "SECURITY_BLOCKED") {
		t.Fatalf("expected bash reading /etc/hosts to require approval, got: %v", err)
	}

	if approvalCalls != 3 {
		t.Fatalf("expected 3 admin approval requests for 3 out-of-workspace ops, got %d", approvalCalls)
	}
}
