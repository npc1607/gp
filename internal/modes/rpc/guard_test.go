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
		prov:      &verdictProvider{verdictJSON: `{"risk_level":"low","allow":true,"can_approve":false,"reason":"ok"}`},
		modelName: "mock/reviewer",
		workDir:   workDir,
		caller:    CallerContext{AllowApproval: true},
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

func TestRiskGuardQuoteAwareFastPassAndNormalizedCache(t *testing.T) {
	workDir := "/workspace/demo-project"
	bashStub := &stubTool{name: "bash"}
	editStub := &stubTool{name: "edit"}
	reg := tools.NewRegistry()
	_ = reg.Register(bashStub)
	_ = reg.Register(editStub)

	vp := &verdictProvider{verdictJSON: `{"risk_level":"low","allow":true,"reason":"ok","required_permission":""}`}
	guarded := wrapRegistryWithRiskGuard(reg, &riskGuard{
		prov:      vp,
		modelName: "mock/reviewer",
		workDir:   workDir,
		cache:     newAuditCache(5 * time.Minute),
	})

	bt, _ := guarded.Get("bash")
	et, _ := guarded.Get("edit")
	ctx := &tools.ToolContext{Context: context.Background(), Cwd: workDir}

	// 1. Quoted alternation pipe in grep, read-only sed -n, and cd inside workDir + grep -> 0 LLM calls
	fastCmds := []string{
		`grep -rnE "days|DayLimit" .`,
		`sed -n '1,50p' main.go`,
		`cd sub && grep -rn "foo" . | head -n 20`,
		`grep -rn "a > b; c" . | sed -n '1,10p'`,
	}
	for _, c := range fastCmds {
		bashStub.executed = false
		raw, _ := json.Marshal(map[string]string{"command": c})
		if _, err := bt.Execute(ctx, raw); err != nil || !bashStub.executed {
			t.Fatalf("expected fast-pass for %q, err=%v executed=%v", c, err, bashStub.executed)
		}
	}
	if vp.calls != 0 {
		t.Fatalf("expected 0 LLM calls for quote-aware read-only commands, got %d", vp.calls)
	}

	// 2. sed -i must NOT fast-pass
	bashStub.executed = false
	if _, err := bt.Execute(ctx, json.RawMessage(`{"command":"sed -i 's/a/b/g' main.go"}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vp.calls != 1 {
		t.Fatalf("expected sed -i to trigger LLM audit (calls=1), got %d", vp.calls)
	}

	// 3. Normalized command signature cache: make test-a and make test-b share 1 LLM call
	if _, err := bt.Execute(ctx, json.RawMessage(`{"command":"make test-a"}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := bt.Execute(ctx, json.RawMessage(`{"command":"make test-b"}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vp.calls != 2 {
		t.Fatalf("expected normalized signature cache hit for make test-b (calls=2), got %d", vp.calls)
	}

	// 4. Interpreters (python3) must NOT share normalized cache across different scripts
	if _, err := bt.Execute(ctx, json.RawMessage(`{"command":"python3 script_a.py"}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := bt.Execute(ctx, json.RawMessage(`{"command":"python3 script_b.py"}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vp.calls != 4 {
		t.Fatalf("expected python3 scripts NOT to share normalized cache (calls=4), got %d", vp.calls)
	}

	// 5. Repeated edit on the same file in workDir shares path-level cache
	if _, err := et.Execute(ctx, json.RawMessage(`{"path":"main.go","oldText":"a","newText":"b"}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := et.Execute(ctx, json.RawMessage(`{"path":"main.go","oldText":"c","newText":"d"}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vp.calls != 5 {
		t.Fatalf("expected edit on same file to hit path cache (calls=5), got %d", vp.calls)
	}
}

func TestRiskGuardOutOfWorkspaceNeverHitsCache(t *testing.T) {
	workDir := "/workspace/demo-project"
	bashStub := &stubTool{name: "bash"}
	editStub := &stubTool{name: "edit"}
	reg := tools.NewRegistry()
	_ = reg.Register(bashStub)
	_ = reg.Register(editStub)

	var approvalCalls int
	vp := &verdictProvider{verdictJSON: `{"risk_level":"low","allow":true,"reason":"ok","required_permission":""}`}
	guarded := wrapRegistryWithRiskGuard(reg, &riskGuard{
		prov:      vp,
		modelName: "mock/reviewer",
		workDir:   workDir,
		caller:    CallerContext{AllowApproval: true},
		requestApproval: func(_ context.Context, _ string, _ string, _ json.RawMessage) (bool, error) {
			approvalCalls++
			return false, nil
		},
		cache: newAuditCache(5 * time.Minute),
	})

	bt, _ := guarded.Get("bash")
	et, _ := guarded.Get("edit")
	ctx := &tools.ToolContext{Context: context.Background(), Cwd: workDir}

	// Warm up normalized cache with in-workspace cd + make and in-workspace edit
	if _, err := bt.Execute(ctx, json.RawMessage(`{"command":"cd sub && make test"}`)); err != nil {
		t.Fatalf("unexpected error for in-workspace cd+make: %v", err)
	}
	if _, err := et.Execute(ctx, json.RawMessage(`{"path":"main.go","oldText":"a","newText":"b"}`)); err != nil {
		t.Fatalf("unexpected error for in-workspace edit: %v", err)
	}

	// Out-of-workspace cd (relative .. and absolute) and out-of-workspace edit must trigger admin approval
	for _, cmd := range []string{`cd ../other-repo && make test`, `cd /etc && make test`} {
		raw, _ := json.Marshal(map[string]string{"command": cmd})
		if _, err := bt.Execute(ctx, raw); err == nil || !strings.Contains(err.Error(), "SECURITY_BLOCKED") {
			t.Fatalf("expected %q to trigger approval and block, got: %v", cmd, err)
		}
	}
	for _, p := range []string{`../other-repo/main.go`, `/etc/hosts`} {
		raw, _ := json.Marshal(map[string]string{"path": p, "oldText": "a", "newText": "b"})
		if _, err := et.Execute(ctx, raw); err == nil || !strings.Contains(err.Error(), "SECURITY_BLOCKED") {
			t.Fatalf("expected edit %q to trigger approval and block, got: %v", p, err)
		}
	}
	if approvalCalls != 4 {
		t.Fatalf("expected 4 admin approval calls, got %d", approvalCalls)
	}
}

func TestRiskGuardWhoResourceAndApprovalCache(t *testing.T) {
	workDir := "/workspace/demo-project"
	sharedAudit := newAuditCache(30 * time.Minute)
	sharedApproved := newAuditCache(5 * time.Minute)

	// 1. Different "who" (senderID) must NOT share audit cache
	vpA := &verdictProvider{verdictJSON: `{"risk_level":"low","allow":true,"reason":"ok"}`}
	vpB := &verdictProvider{verdictJSON: `{"risk_level":"low","allow":true,"reason":"ok"}`}
	regA := tools.NewRegistry()
	_ = regA.Register(&stubTool{name: "bash"})
	_ = regA.Register(&stubTool{name: "feishu_update_doc"})
	_ = regA.Register(&stubTool{name: "feishu_delete_doc"})
	_ = regA.Register(&stubTool{name: "read"})

	var apprA, apprB int
	gA := wrapRegistryWithRiskGuard(regA, &riskGuard{
		prov:          vpA,
		modelName:     "mock/reviewer",
		workDir:       workDir,
		sessionID:     "chat-1",
		caller:        CallerContext{SenderID: "ou_boss", IsBoss: true, AllowApproval: true},
		cache:         sharedAudit,
		approvedCache: sharedApproved,
		requestApproval: func(_ context.Context, _ string, _ string, _ json.RawMessage) (bool, error) {
			apprA++
			return true, nil
		},
	})

	regB := tools.NewRegistry()
	_ = regB.Register(&stubTool{name: "bash"})
	_ = regB.Register(&stubTool{name: "read"})
	gB := wrapRegistryWithRiskGuard(regB, &riskGuard{
		prov:          vpB,
		modelName:     "mock/reviewer",
		workDir:       workDir,
		sessionID:     "chat-1",
		caller:        CallerContext{SenderID: "ou_member", IsBoss: false, AllowApproval: true},
		cache:         sharedAudit,
		approvedCache: sharedApproved,
		requestApproval: func(_ context.Context, _ string, _ string, _ json.RawMessage) (bool, error) {
			apprB++
			return false, nil
		},
	})

	ctx := &tools.ToolContext{Context: context.Background(), Cwd: workDir}

	btA, _ := gA.Get("bash")
	btB, _ := gB.Get("bash")
	if _, err := btA.Execute(ctx, json.RawMessage(`{"command":"make test"}`)); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if _, err := btB.Execute(ctx, json.RawMessage(`{"command":"make test"}`)); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if vpA.calls != 1 || vpB.calls != 1 {
		t.Fatalf("expected separate audit cache per senderID (vpA=1, vpB=1), got vpA=%d vpB=%d", vpA.calls, vpB.calls)
	}

	// 2. MCP resource normalization: update_doc caches by document_id, delete_doc does NOT
	upTool, _ := gA.Get("feishu_update_doc")
	delTool, _ := gA.Get("feishu_delete_doc")
	if _, err := upTool.Execute(ctx, json.RawMessage(`{"document_id":"doc_123","content":"v1"}`)); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if _, err := upTool.Execute(ctx, json.RawMessage(`{"document_id":"doc_123","content":"v2"}`)); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if vpA.calls != 2 {
		t.Fatalf("expected feishu_update_doc to share resource cache (vpA=2), got %d", vpA.calls)
	}
	if _, err := delTool.Execute(ctx, json.RawMessage(`{"document_id":"doc_123","block":"b1"}`)); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if _, err := delTool.Execute(ctx, json.RawMessage(`{"document_id":"doc_123","block":"b2"}`)); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if vpA.calls != 4 {
		t.Fatalf("expected feishu_delete_doc NOT to share resource cache (vpA=4), got %d", vpA.calls)
	}

	// 3. Approval cache: ou_boss approved /opt/ext/a.txt -> /opt/ext/b.txt passes without re-prompt;
	// ou_member must still be prompted!
	rtA, _ := gA.Get("read")
	rtB, _ := gB.Get("read")
	if _, err := rtA.Execute(ctx, json.RawMessage(`{"path":"/opt/ext/a.txt"}`)); err != nil {
		t.Fatalf("expected approved read to succeed: %v", err)
	}
	if _, err := rtA.Execute(ctx, json.RawMessage(`{"path":"/opt/ext/b.txt"}`)); err != nil {
		t.Fatalf("expected second read in approved dir to pass without re-prompt: %v", err)
	}
	if apprA != 1 {
		t.Fatalf("expected exactly 1 approval prompt for ou_boss, got %d", apprA)
	}
	if _, err := rtB.Execute(ctx, json.RawMessage(`{"path":"/opt/ext/b.txt"}`)); err == nil || !strings.Contains(err.Error(), "SECURITY_BLOCKED") {
		t.Fatalf("expected ou_member NOT to inherit ou_boss approval cache, got: %v", err)
	}
	if apprB != 1 {
		t.Fatalf("expected 1 approval prompt for ou_member, got %d", apprB)
	}
}

func TestRiskGuardRolePolicyEnforcement(t *testing.T) {
	baseDir := "/workspace/services"
	dataSvc := baseDir + "/data-service"
	collectDao := baseDir + "/collect-dao"
	otherRepo := baseDir + "/fsrobot"

	readStub := &stubTool{name: "read"}
	writeStub := &stubTool{name: "write"}
	bashStub := &stubTool{name: "bash"}
	reg := tools.NewRegistry()
	_ = reg.Register(readStub)
	_ = reg.Register(writeStub)
	_ = reg.Register(bashStub)

	falseVal := false
	guarded := wrapRegistryWithRiskGuard(reg, &riskGuard{
		prov:      &verdictProvider{verdictJSON: `{"risk_level":"low","allow":true,"reason":"ok"}`},
		modelName: "mock/reviewer",
		workDir:   baseDir,
		policy: ExecutionPolicy{
			WorkDir:     baseDir,
			AllowedDirs: []string{dataSvc, collectDao},
			DeniedPaths: []string{".env*", "*.key", "config.toml"},
			AllowBash:   &falseVal,
			AllowWrite:  &falseVal,
		},
		cache: newAuditCache(5 * time.Minute),
	})

	rt, _ := guarded.Get("read")
	wt, _ := guarded.Get("write")
	bt, _ := guarded.Get("bash")
	ctx := &tools.ToolContext{Context: context.Background(), Cwd: baseDir}

	// 1. Allowed dirs (data-service & collect-dao) pass at 0ms
	if _, err := rt.Execute(ctx, json.RawMessage(`{"path":"data-service/main.go"}`)); err != nil || !readStub.executed {
		t.Fatalf("expected read in data-service to pass, err=%v", err)
	}
	readStub.executed = false
	if _, err := rt.Execute(ctx, json.RawMessage(`{"path":"collect-dao/repo.go"}`)); err != nil || !readStub.executed {
		t.Fatalf("expected read in collect-dao to pass, err=%v", err)
	}

	// 2. Sibling repo outside allowed_dirs (fsrobot) must be blocked
	readStub.executed = false
	if _, err := rt.Execute(ctx, json.RawMessage(`{"path":"fsrobot/main.go"}`)); err == nil || !strings.Contains(err.Error(), "SECURITY_BLOCKED") {
		t.Fatalf("expected read in sibling %s to be blocked, got: %v", otherRepo, err)
	}

	// 3. Denied path (.env.prod, secret.key) inside allowed_dirs must be permanently blocked
	if _, err := rt.Execute(ctx, json.RawMessage(`{"path":"data-service/.env.prod"}`)); err == nil || !strings.Contains(err.Error(), "SECURITY_BLOCKED") {
		t.Fatalf("expected denied_paths .env.prod to be blocked, got: %v", err)
	}
	if _, err := rt.Execute(ctx, json.RawMessage(`{"path":"collect-dao/tls.key"}`)); err == nil || !strings.Contains(err.Error(), "SECURITY_BLOCKED") {
		t.Fatalf("expected denied_paths tls.key to be blocked, got: %v", err)
	}

	// 4. allow_bash=false & allow_write=false permanently block bash/write
	if _, err := bt.Execute(ctx, json.RawMessage(`{"command":"ls"}`)); err == nil || !strings.Contains(err.Error(), "SECURITY_BLOCKED") {
		t.Fatalf("expected allow_bash=false to block bash, got: %v", err)
	}
	if _, err := wt.Execute(ctx, json.RawMessage(`{"path":"data-service/a.go","content":"x"}`)); err == nil || !strings.Contains(err.Error(), "SECURITY_BLOCKED") {
		t.Fatalf("expected allow_write=false to block write, got: %v", err)
	}
}

func TestRiskGuardScopedBashAndCapabilities(t *testing.T) {
	workDir := "/workspace/services"
	skillDir := "/opt/skills/diagram"
	bashStub := &stubTool{name: "bash"}
	mcpRead := &stubTool{name: "mcp__feishu__read_doc"}
	mcpDel := &stubTool{name: "mcp__feishu__delete_doc"}

	rawReg := tools.NewRegistry()
	_ = rawReg.Register(bashStub)
	_ = rawReg.Register(mcpRead)
	_ = rawReg.Register(mcpDel)

	policy := ExecutionPolicy{
		WorkDir:     workDir,
		AllowedDirs: []string{workDir + "/data-service"},
		DeniedPaths: []string{".env*"},
		BashMode:    "scoped",
		Capabilities: []CapabilityRule{
			{
				Kind:  "skill",
				Name:  "diagram-skill",
				Allow: []string{"mmdc", "scripts/draw.sh"},
				Paths: []string{skillDir},
			},
			{
				Kind:  "mcp",
				Name:  "feishu-mcp",
				Allow: []string{"mcp__feishu__read_doc"},
			},
		},
	}

	// 1. applyToolPolicy keeps allowed MCP tool and strips non-allowlisted MCP tool
	scopedReg := applyToolPolicy(rawReg, policy)
	if _, ok := scopedReg.Get("mcp__feishu__read_doc"); !ok {
		t.Fatal("expected allowlisted mcp__feishu__read_doc to remain")
	}
	if _, ok := scopedReg.Get("mcp__feishu__delete_doc"); ok {
		t.Fatal("expected non-allowlisted mcp__feishu__delete_doc to be stripped")
	}

	guarded := wrapRegistryWithRiskGuard(scopedReg, &riskGuard{
		prov:      &verdictProvider{verdictJSON: `{"risk_level":"low","allow":true,"reason":"ok"}`},
		modelName: "mock/reviewer",
		workDir:   workDir,
		policy:    policy,
		cache:     newAuditCache(5 * time.Minute),
	})
	bt, _ := guarded.Get("bash")
	ctx := &tools.ToolContext{Context: context.Background(), Cwd: workDir}

	// 2. Authorized skill commands within /tmp & skillDir pass at 0ms
	for _, cmd := range []string{
		`mmdc -i /opt/skills/diagram/tpl.mmd -o /tmp/out.png`,
		`scripts/draw.sh /tmp/diagram.svg`,
	} {
		bashStub.executed = false
		raw, _ := json.Marshal(map[string]string{"command": cmd})
		if _, err := bt.Execute(ctx, raw); err != nil || !bashStub.executed {
			t.Fatalf("expected scoped skill command %q to pass, err=%v executed=%v", cmd, err, bashStub.executed)
		}
	}

	// 3. Disallowed bash commands, chaining, and out-of-scope paths are permanently blocked
	for _, badCmd := range []string{
		`ls -la`,
		`cat /etc/passwd`,
		`mmdc -i /opt/skills/diagram/tpl.mmd && rm -rf /tmp/x`,
		`mmdc -i /etc/hosts -o /tmp/out.png`,
		`python3 -c "print(1)"`,
	} {
		bashStub.executed = false
		raw, _ := json.Marshal(map[string]string{"command": badCmd})
		if _, err := bt.Execute(ctx, raw); err == nil || !strings.Contains(err.Error(), "SECURITY_BLOCKED") {
			t.Fatalf("expected %q to be blocked in scoped bash mode, got: %v", badCmd, err)
		}
		if bashStub.executed {
			t.Fatalf("blocked command %q should NOT have executed", badCmd)
		}
	}
}

func TestRiskGuardAgentDirAndMCPCapabilities(t *testing.T) {
	fakeHome := "/opt/test-agent-home"
	t.Setenv("HOME", fakeHome)
	gpDir := fakeHome + "/.gp"
	skillFile := gpDir + "/skills/anidoodle/SKILL.md"
	skillScript := gpDir + "/skills/anidoodle/render.py"
	authFile := gpDir + "/auth.json"

	ctx := &tools.ToolContext{Context: context.Background(), Cwd: "/workspace/services"}

	// --- 1. Full mode: agentDirs read/exec allowed, sensitive files & mutations require approval ---
	vpFull := &verdictProvider{verdictJSON: `{"risk_level":"low","allow":true,"reason":"ok"}`}
	readStub := &stubTool{name: "read"}
	writeStub := &stubTool{name: "write"}
	bashStub := &stubTool{name: "bash"}
	mcpAllowedStub := &stubTool{name: "mcp__feishu__read_doc"}
	mcpBlockedStub := &stubTool{name: "mcp__feishu__delete_doc"}

	rawReg := tools.NewRegistry()
	_ = rawReg.Register(readStub)
	_ = rawReg.Register(writeStub)
	_ = rawReg.Register(bashStub)
	_ = rawReg.Register(mcpAllowedStub)
	_ = rawReg.Register(mcpBlockedStub)

	apprPrompts := 0
	fullGuarded := wrapRegistryWithRiskGuard(rawReg, &riskGuard{
		prov:      vpFull,
		modelName: "mock/reviewer",
		workDir:   "/workspace/services",
		agentDirs: []string{gpDir},
		policy: ExecutionPolicy{
			WorkDir:     "/workspace/services",
			AllowedDirs: []string{"/workspace/services/data-service"},
		},
		caller: CallerContext{SenderID: "ou_boss", IsBoss: true, AllowApproval: true},
		cache:  newAuditCache(time.Minute),
		requestApproval: func(_ context.Context, _ string, _ string, _ json.RawMessage) (bool, error) {
			apprPrompts++
			return false, nil // reject so we can assert approval was triggered
		},
	})

	rt, _ := fullGuarded.Get("read")
	wt, _ := fullGuarded.Get("write")
	bt, _ := fullGuarded.Get("bash")

	// Read skill file -> 0ms pass (no audit, no approval)
	rawReadSkill, _ := json.Marshal(map[string]string{"path": skillFile})
	if _, err := rt.Execute(ctx, rawReadSkill); err != nil {
		t.Fatalf("expected read on agent skill file to pass, got: %v", err)
	}
	// Read-only bash on skill dir -> 0ms pass
	rawLsSkill, _ := json.Marshal(map[string]string{"command": "ls " + gpDir + "/skills/anidoodle"})
	if _, err := bt.Execute(ctx, rawLsSkill); err != nil {
		t.Fatalf("expected ls on agent skill dir to pass, got: %v", err)
	}
	if vpFull.calls != 0 || apprPrompts != 0 {
		t.Fatalf("expected 0ms fast-pass for agent dir reads, got vp=%d appr=%d", vpFull.calls, apprPrompts)
	}

	// Non-mutating script execution in skill dir -> NOT statically blocked (goes through audit and passes)
	rawRunSkill, _ := json.Marshal(map[string]string{"command": "python3 " + skillScript + " /tmp/out.png"})
	if _, err := bt.Execute(ctx, rawRunSkill); err != nil {
		t.Fatalf("expected running agent skill script to pass without static approval, got: %v", err)
	}
	if apprPrompts != 0 {
		t.Fatalf("expected no approval prompt for running skill script, got %d", apprPrompts)
	}

	// Sensitive auth.json in ~/.gp -> MUST require approval
	rawReadAuth, _ := json.Marshal(map[string]string{"path": authFile})
	if _, err := rt.Execute(ctx, rawReadAuth); err == nil || !strings.Contains(err.Error(), "SECURITY_BLOCKED") {
		t.Fatalf("expected reading auth.json to require approval, got: %v", err)
	}
	// Writing to skill dir -> MUST require approval
	rawWriteSkill, _ := json.Marshal(map[string]string{"path": skillFile, "content": "hacked"})
	if _, err := wt.Execute(ctx, rawWriteSkill); err == nil || !strings.Contains(err.Error(), "SECURITY_BLOCKED") {
		t.Fatalf("expected writing to agent skill dir to require approval, got: %v", err)
	}
	// Mutating bash on skill dir -> MUST require approval
	rawTouchSkill, _ := json.Marshal(map[string]string{"command": "touch " + gpDir + "/skills/anidoodle/new.txt"})
	if _, err := bt.Execute(ctx, rawTouchSkill); err == nil || !strings.Contains(err.Error(), "SECURITY_BLOCKED") {
		t.Fatalf("expected mutating bash on agent skill dir to require approval, got: %v", err)
	}
	if apprPrompts != 3 {
		t.Fatalf("expected 3 approval prompts (auth.json, write, touch), got %d", apprPrompts)
	}

	// --- 2. Scoped mode: agentDirs NOT auto-allowed; MCP capability whitelist enforced ---
	scopedAppr := 0
	scopedPolicy := ExecutionPolicy{
		WorkDir:     "/workspace/services",
		AllowedDirs: []string{"/workspace/services/data-service"},
		BashMode:    "scoped",
		Capabilities: []CapabilityRule{
			{Kind: "mcp", Name: "feishu-read", Allow: []string{"mcp__feishu__read_*"}},
		},
	}
	scopedGuarded := wrapRegistryWithRiskGuard(rawReg, &riskGuard{
		prov:      &verdictProvider{verdictJSON: `{"risk_level":"low","allow":true,"reason":"ok"}`},
		modelName: "mock/reviewer",
		workDir:   "/workspace/services",
		agentDirs: []string{gpDir},
		policy:    scopedPolicy,
		caller:    CallerContext{SenderID: "ou_boss", IsBoss: true, AllowApproval: true},
		cache:     newAuditCache(time.Minute),
		requestApproval: func(_ context.Context, _ string, _ string, _ json.RawMessage) (bool, error) {
			scopedAppr++
			return false, nil
		},
	})

	// Unconfigured bash in ~/.gp/skills -> hard rejected in scoped mode (CanApprove=false, scopedAppr stays 0)
	sBt, _ := scopedGuarded.Get("bash")
	if _, err := sBt.Execute(ctx, rawLsSkill); err == nil || !strings.Contains(err.Error(), "SECURITY_BLOCKED") {
		t.Fatalf("expected scoped mode to block unconfigured agent skill bash, got: %v", err)
	}
	if scopedAppr != 0 {
		t.Fatalf("expected scoped bash rejection to be non-approvable (0 prompts), got %d", scopedAppr)
	}

	// Allowlisted MCP wildcard -> 0ms pass
	sMcpAllow, _ := scopedGuarded.Get("mcp__feishu__read_doc")
	if _, err := sMcpAllow.Execute(ctx, json.RawMessage(`{"ref":"doc1"}`)); err != nil {
		t.Fatalf("expected allowlisted MCP wildcard tool to pass, got: %v", err)
	}
	// Non-allowlisted MCP -> hard rejected (CanApprove=false)
	sMcpBlock, _ := scopedGuarded.Get("mcp__feishu__delete_doc")
	if _, err := sMcpBlock.Execute(ctx, json.RawMessage(`{"ref":"doc1"}`)); err == nil || !strings.Contains(err.Error(), "SECURITY_BLOCKED") {
		t.Fatalf("expected non-allowlisted MCP tool to be hard rejected, got: %v", err)
	}
	if scopedAppr != 0 {
		t.Fatalf("expected non-allowlisted MCP rejection to be non-approvable (0 prompts), got %d", scopedAppr)
	}
}
