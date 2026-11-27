package extensions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"gp/internal/tools"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestExtensions_DiscoveryOverrideAndFaultIsolation(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()

	// 全局扩展
	globalDir := filepath.Join(home, ".gp", "extensions", "demo-guide")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(globalDir, "extension.yaml"), []byte(`
api_version: gp.ext/v1
name: demo-guide
description: global version
guide: GUIDE.md
adapter:
  type: guide
`), 0o644)
	_ = os.WriteFile(filepath.Join(globalDir, "GUIDE.md"), []byte("Global Guide Body"), 0o644)

	// 项目级同名覆盖
	projDir := filepath.Join(cwd, ".gp", "extensions", "demo-guide")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(projDir, "extension.yaml"), []byte(`
api_version: gp.ext/v1
name: demo-guide
description: project override version
guide: GUIDE.md
adapter:
  type: guide
`), 0o644)
	_ = os.WriteFile(filepath.Join(projDir, "GUIDE.md"), []byte("Project Guide Body"), 0o644)

	// 损坏的扩展（应被隔离为 error 状态，不影响正常扩展）
	brokenDir := filepath.Join(cwd, ".gp", "extensions", "broken-ext")
	_ = os.MkdirAll(brokenDir, 0o755)
	_ = os.WriteFile(filepath.Join(brokenDir, "extension.yaml"), []byte(`
api_version: gp.ext/v1
name: broken-ext
description: missing adapter command
adapter:
  type: cli
`), 0o644)

	reg := NewRegistry(RegistryConfig{
		Load: LoadOptions{Cwd: cwd, HomeDir: home},
	})
	defer reg.CloseAll()

	statuses := reg.ListStatus()
	if len(statuses) != 2 {
		t.Fatalf("expected 2 extensions, got %+v", statuses)
	}
	if statuses[0].Name != "broken-ext" || statuses[0].State != "error" {
		t.Fatalf("expected broken-ext in error state, got %+v", statuses[0])
	}
	if statuses[1].Name != "demo-guide" || statuses[1].Source != "project" || statuses[1].Description != "project override version" {
		t.Fatalf("expected project override for demo-guide, got %+v", statuses[1])
	}

	dirPrompt := reg.RenderDirectoryPrompt()
	if !strings.Contains(dirPrompt, "demo-guide [guide]: project override version") || strings.Contains(dirPrompt, "broken-ext") {
		t.Fatalf("unexpected Stage 0 directory prompt:\n%s", dirPrompt)
	}

	desc, isErr := reg.Describe(context.Background(), "demo-guide", "", true, 5)
	if isErr || !strings.Contains(desc, "Project Guide Body") {
		t.Fatalf("expected project GUIDE.md content, got isErr=%v:\n%s", isErr, desc)
	}
}

func TestExtensions_CLI_InjectionImmunityAndGovernance(t *testing.T) {
	cwd := t.TempDir()
	extDir := filepath.Join(cwd, ".gp", "extensions", "safe-cli")
	_ = os.MkdirAll(extDir, 0o755)
	_ = os.WriteFile(filepath.Join(extDir, "extension.yaml"), []byte(`
api_version: gp.ext/v1
name: safe-cli
description: safe command runner
governance:
  subagent_policy: read_only
adapter:
  type: cli
  command: printf
actions:
  - name: echo_arg
    description: safely print argument
    risk: read
    args: ["%s", "{{.msg}}"]
    params:
      msg: { type: string, required: true }
  - name: dangerous_op
    description: destructive operation
    risk: destructive
    args: ["%s", "boom"]
`), 0o644)

	reg := NewRegistry(RegistryConfig{
		Load: LoadOptions{Cwd: cwd},
	})
	defer reg.CloseAll()

	// 1. 注入载荷应原样作为字面量输出，绝不被 shell 解释
	payload := "hello; echo INJECTED"
	res := reg.Call(context.Background(), "safe-cli", "echo_arg", map[string]any{"msg": payload}, CallContext{Cwd: cwd})
	if res.IsError || res.Content != payload {
		t.Fatalf("expected literal output %q without shell execution, got isErr=%v content=%q", payload, res.IsError, res.Content)
	}

	// 2. 缺失必填参数应返回 ERR_INVALID_ARGS 并附带 Schema 提示
	missing := reg.Call(context.Background(), "safe-cli", "echo_arg", map[string]any{}, CallContext{Cwd: cwd})
	if !missing.IsError || missing.ErrorCode != ErrInvalidArgs || !strings.Contains(missing.Content, "Expected schema:") {
		t.Fatalf("expected ERR_INVALID_ARGS with schema hint, got %+v", missing)
	}

	// 3. 高危动作在无确认回调时默认 Fail-Closed (ERR_DENIED)
	denied := reg.Call(context.Background(), "safe-cli", "dangerous_op", nil, CallContext{Cwd: cwd})
	if !denied.IsError || denied.ErrorCode != ErrDenied {
		t.Fatalf("expected destructive action Fail-Closed ERR_DENIED, got %+v", denied)
	}

	// 4. 子 Agent 调用非只读动作被 subagent_policy=read_only 拦截
	subDenied := reg.Call(context.Background(), "safe-cli", "dangerous_op", nil, CallContext{Cwd: cwd, AgentID: "worker_1", IsSubagent: true})
	if !subDenied.IsError || subDenied.ErrorCode != ErrDenied || !strings.Contains(subDenied.Content, "subagent") {
		t.Fatalf("expected subagent policy denial, got %+v", subDenied)
	}
}

func TestExtensions_ScriptAdapter_AndSecretScrubbing(t *testing.T) {
	cwd := t.TempDir()
	extDir := filepath.Join(cwd, ".gp", "extensions", "diag-script")
	scriptsDir := filepath.Join(extDir, "scripts")
	_ = os.MkdirAll(scriptsDir, 0o755)

	scriptBody := `#!/bin/sh
cat > /dev/null
printf '{"ok":true,"content":"Leaked token check: %s","data":{"status":"ok"}}' "$GP_EXT_AUTH_TOKEN"
`
	scriptPath := filepath.Join(scriptsDir, "run.sh")
	_ = os.WriteFile(scriptPath, []byte(scriptBody), 0o755)
	_ = os.WriteFile(filepath.Join(extDir, "extension.yaml"), []byte(`
api_version: gp.ext/v1
name: diag-script
description: script protocol test
auth:
  type: bearer
  token_ref: env://TEST_EXT_SECRET_TOKEN
  required: true
adapter:
  type: script
  exec: ./scripts/run.sh
  interpreter: /bin/sh
actions:
  - name: check
    description: run diagnostic
    risk: read
`), 0o644)

	t.Setenv("TEST_EXT_SECRET_TOKEN", "super-secret-token-998877")

	reg := NewRegistry(RegistryConfig{
		Load: LoadOptions{Cwd: cwd},
	})
	defer reg.CloseAll()

	res := reg.Call(context.Background(), "diag-script", "check", nil, CallContext{Cwd: cwd})
	if res.IsError {
		t.Fatalf("expected script success, got %+v", res)
	}
	if strings.Contains(res.Content, "super-secret-token-998877") {
		t.Fatalf("expected secret token to be scrubbed, got: %s", res.Content)
	}
	if !strings.Contains(res.Content, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] marker in scrubbed output, got: %s", res.Content)
	}
}

func TestExtensions_HTTPAdapter_ExtractAndSpillBudget(t *testing.T) {
	cwd := t.TempDir()
	spillDir := t.TempDir()
	extDir := filepath.Join(cwd, ".gp", "extensions", "data-test-platform")
	_ = os.MkdirAll(extDir, 0o755)
	_ = os.WriteFile(filepath.Join(extDir, "extension.yaml"), []byte(`
api_version: gp.ext/v1
name: data-test-platform
description: data-service test platform
governance:
  default_risk: read
  output_max_bytes: 120
  output_max_lines: 6
adapter:
  type: http
  base_url: "http://mock.local:9099"
actions:
  - name: recent_logs
    description: fetch recent logs
    risk: read
    method: GET
    path: /api/logs/recent
    extract: "data.logs[:2]"
    params:
      limit: { type: integer, default: 50 }
`), 0o644)

	reg := NewRegistry(RegistryConfig{
		Load:     LoadOptions{Cwd: cwd},
		SpillDir: spillDir,
	})
	defer reg.CloseAll()

	// 注入内存态 RoundTripper，避免沙箱回环端口限制
	reg.mu.RLock()
	ha := reg.exts["data-test-platform"].adapter.(*httpAdapter)
	reg.mu.RUnlock()
	ha.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/api/logs/recent" || r.URL.Query().Get("limit") != "50" {
				t.Fatalf("unexpected http request: %s", r.URL.String())
			}
			body := `{"code":0,"data":{"logs":["line-1-very-long-sql-select-from-suppliers-where-qc-tag-id-is-not-null","line-2-another-very-long-sql-query-record-for-spill-test","line-3-should-be-sliced-out"]}}`
			return &http.Response{
				StatusCode: 200,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	}

	// 通过标准 tools.Registry 调用 ext_describe 与 ext_call
	tReg := tools.NewRegistry()
	if err := RegisterTools(tReg, reg); err != nil {
		t.Fatalf("RegisterTools failed: %v", err)
	}
	descTool, _ := tReg.Get("ext_describe")
	callTool, _ := tReg.Get("ext_call")

	descOut, err := descTool.Execute(&tools.ToolContext{Context: context.Background(), AgentID: "root"}, json.RawMessage(`{"extension":"data-test-platform"}`))
	if err != nil || descOut.IsError || !strings.Contains(descOut.Content, "recent_logs") {
		t.Fatalf("ext_describe failed: err=%v out=%+v", err, descOut)
	}

	callOut, err := callTool.Execute(&tools.ToolContext{Context: context.Background(), AgentID: "root"}, json.RawMessage(`{"extension":"data-test-platform","action":"recent_logs","args":{}}`))
	if err != nil || callOut.IsError {
		t.Fatalf("ext_call failed: err=%v out=%+v", err, callOut)
	}
	if strings.Contains(callOut.Content, "line-3-should-be-sliced-out") {
		t.Fatalf("expected extract data.logs[:2] to slice out line-3, got:\n%s", callOut.Content)
	}
	if !strings.Contains(callOut.Content, "Full output saved to:") {
		t.Fatalf("expected output over budget to spill to file, got:\n%s", callOut.Content)
	}
}

func TestExtensions_OpenAPIAdapter_BM25ActionSearch(t *testing.T) {
	cwd := t.TempDir()
	extDir := filepath.Join(cwd, ".gp", "extensions", "data-service-api")
	_ = os.MkdirAll(extDir, 0o755)
	_ = os.WriteFile(filepath.Join(extDir, "openapi.yaml"), []byte(`
openapi: "3.0.0"
paths:
  /api/v1/qc/supplier-tags:
    get:
      operationId: GetSupplierQcTagIds
      summary: 查询供应商质检 QC 标签 ID 列表
      tags: [QCService]
      parameters:
        - name: supplier_id
          in: query
          required: true
          schema: { type: string }
  /api/v1/jobs/{job_id}:
    delete:
      operationId: DeleteJob
      summary: 删除任务记录
      tags: [JobService]
      parameters:
        - name: job_id
          in: path
          required: true
          schema: { type: string }
`), 0o644)
	_ = os.WriteFile(filepath.Join(extDir, "extension.yaml"), []byte(`
api_version: gp.ext/v1
name: data-service-api
description: data-service OpenAPI catalog
adapter:
  type: openapi
  spec_file: openapi.yaml
  base_url: "http://127.0.0.1:8005"
`), 0o644)

	reg := NewRegistry(RegistryConfig{
		Load: LoadOptions{Cwd: cwd},
	})
	defer reg.CloseAll()

	desc, isErr := reg.Describe(context.Background(), "data-service-api", "GetSupplierQcTagIds", false, 1)
	if isErr || !strings.Contains(desc, "GetSupplierQcTagIds") || strings.Contains(desc, "DeleteJob") {
		t.Fatalf("expected BM25 search to return only GetSupplierQcTagIds, got isErr=%v:\n%s", isErr, desc)
	}
}

func TestExtensions_SecuritySymlinkAndOpenAPISpecBoundary(t *testing.T) {
	outside := t.TempDir()
	secretFile := filepath.Join(outside, "host-secret.txt")
	_ = os.WriteFile(secretFile, []byte("HOST_SECRET_DATA"), 0o600)

	cwd := t.TempDir()
	// 1. 软链逃逸 GUIDE.md 应被 safeJoinUnderDir 拦截
	evilDir := filepath.Join(cwd, ".gp", "extensions", "evil-symlink")
	_ = os.MkdirAll(evilDir, 0o755)
	_ = os.Symlink(secretFile, filepath.Join(evilDir, "GUIDE.md"))
	_ = os.WriteFile(filepath.Join(evilDir, "extension.yaml"), []byte(`
api_version: gp.ext/v1
name: evil-symlink
description: tries symlink escape
guide: GUIDE.md
adapter:
  type: guide
`), 0o644)

	// 2. OpenAPI spec_file 绝对路径与 ../ 越界应被拦截
	evilOADir := filepath.Join(cwd, ".gp", "extensions", "evil-oa")
	_ = os.MkdirAll(evilOADir, 0o755)
	_ = os.WriteFile(filepath.Join(evilOADir, "extension.yaml"), []byte(`
api_version: gp.ext/v1
name: evil-oa
description: absolute spec path
adapter:
  type: openapi
  spec_file: `+secretFile+`
  base_url: "http://127.0.0.1:8005"
`), 0o644)

	reg := NewRegistry(RegistryConfig{Load: LoadOptions{Cwd: cwd}})
	defer reg.CloseAll()

	statuses := reg.ListStatus()
	for _, st := range statuses {
		if st.Name == "evil-symlink" && st.State != "error" {
			t.Fatalf("expected symlink escape extension to fail validation, got %+v", st)
		}
	}
	oaDesc, isErr := reg.Describe(context.Background(), "evil-oa", "", false, 5)
	if !isErr || !strings.Contains(oaDesc, "forbidden") {
		t.Fatalf("expected absolute openapi spec_file to be rejected, got isErr=%v: %s", isErr, oaDesc)
	}
}

func TestExtensions_DestructiveGateBeforeCmdAuthAndReloadPreservesDisable(t *testing.T) {
	cwd := t.TempDir()
	marker := filepath.Join(cwd, "cmd_auth_ran.marker")
	extDir := filepath.Join(cwd, ".gp", "extensions", "gated-ext")
	_ = os.MkdirAll(extDir, 0o755)
	_ = os.WriteFile(filepath.Join(extDir, "extension.yaml"), []byte(`
api_version: gp.ext/v1
name: gated-ext
description: tests destructive gate order
governance:
  subagent_policy: deny
auth:
  type: bearer
  token_ref: cmd://touch `+marker+`
adapter:
  type: cli
  command: printf
actions:
  - name: wipe
    description: destructive wipe
    risk: destructive
    args: ["%s", "done"]
`), 0o644)

	reg := NewRegistry(RegistryConfig{Load: LoadOptions{Cwd: cwd}})
	defer reg.CloseAll()

	// 1. 高危动作被拒时，绝不能先执行 cmd:// auth
	res := reg.Call(context.Background(), "gated-ext", "wipe", nil, CallContext{Cwd: cwd})
	if !res.IsError || res.ErrorCode != ErrDenied {
		t.Fatalf("expected ERR_DENIED, got %+v", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("cmd:// auth executed BEFORE destructive gate!")
	}

	// 2. subagent_policy: deny 同步拦截 ext_describe
	desc, isErr := reg.DescribeForCaller(context.Background(), "gated-ext", "", true, 5, true)
	if !isErr || !strings.Contains(desc, ErrDenied) {
		t.Fatalf("expected subagent describe denial, got isErr=%v: %s", isErr, desc)
	}

	// 3. SetEnabled(false) 后 Reload() 必须保持禁用，不能静默回滚
	if !reg.SetEnabled("gated-ext", false) {
		t.Fatalf("SetEnabled(false) returned false")
	}
	reg.Reload()
	for _, st := range reg.ListStatus() {
		if st.Name == "gated-ext" && st.Enabled {
			t.Fatalf("expected Reload() to preserve disabled state, got enabled=true")
		}
	}
}

func TestExtensions_OpenAPIRefBodyLargeIntAndScrubbing(t *testing.T) {
	cwd := t.TempDir()
	extDir := filepath.Join(cwd, ".gp", "extensions", "oa-ref")
	_ = os.MkdirAll(extDir, 0o755)
	_ = os.WriteFile(filepath.Join(extDir, "openapi.yaml"), []byte(`
openapi: "3.0.0"
components:
  schemas:
    CreateReq:
      type: object
      required: [supplier_id]
      properties:
        supplier_id:
          type: integer
        note:
          type: string
paths:
  /api/v1/items/{item_id}:
    post:
      operationId: CreateItem
      summary: create item
      parameters:
        - name: item_id
          in: path
          required: true
          schema: { type: integer }
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: "#/components/schemas/CreateReq"
  /api/v1/raw:
    post:
      operationId: RawPayload
      summary: opaque body
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
`), 0o644)
	_ = os.WriteFile(filepath.Join(extDir, "extension.yaml"), []byte(`
api_version: gp.ext/v1
name: oa-ref
description: openapi $ref & scrubbing test
auth:
  type: basic
  token_ref: env://TEST_BASIC_CRED
adapter:
  type: openapi
  spec_file: openapi.yaml
  base_url: "http://mock.local:8005"
`), 0o644)

	t.Setenv("TEST_BASIC_CRED", "alice:my+secret/pass")
	reg := NewRegistry(RegistryConfig{Load: LoadOptions{Cwd: cwd}})
	defer reg.CloseAll()

	reg.mu.RLock()
	oa := reg.exts["oa-ref"].adapter.(*openapiAdapter)
	reg.mu.RUnlock()

	var capturedPath, capturedBody string
	oa.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			capturedPath = r.URL.Path
			b, _ := io.ReadAll(r.Body)
			capturedBody = string(b)
			if r.URL.Path == "/api/v1/raw" && strings.Contains(capturedBody, `"fail_net"`) {
				return nil, &url.Error{Op: "Post", URL: "http://mock.local:8005/api/v1/raw?api_key=alice%3Amy%2Bsecret%2Fpass", Err: errors.New("connection refused")}
			}
			// 回显 Base64 Authorization 头以验证脱敏
			return &http.Response{
				StatusCode: 200,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"echo_auth":"` + r.Header.Get("Authorization") + `"}`)),
			}, nil
		}),
	}

	tReg := tools.NewRegistry()
	_ = RegisterTools(tReg, reg)
	callTool, _ := tReg.Get("ext_call")

	// 1. 大整数 ID 1700000000 不能变成 1.7e+09，且 $ref 字段扁平化 + Base64 密钥脱敏
	out, err := callTool.Execute(&tools.ToolContext{Context: context.Background()}, json.RawMessage(`{
		"extension": "oa-ref",
		"action": "CreateItem",
		"args": {"item_id": 1700000000, "supplier_id": 1234567890, "note": "ok"}
	}`))
	if err != nil || out.IsError {
		t.Fatalf("CreateItem failed: err=%v out=%+v", err, out)
	}
	if capturedPath != "/api/v1/items/1700000000" {
		t.Fatalf("expected non-scientific integer path /api/v1/items/1700000000, got %q", capturedPath)
	}
	if !strings.Contains(capturedBody, `1234567890`) || strings.Contains(capturedBody, `"body"`) {
		t.Fatalf("expected unwrapped $ref body with exact int, got %s", capturedBody)
	}
	b64 := base64.StdEncoding.EncodeToString([]byte("alice:my+secret/pass"))
	if strings.Contains(out.Content, b64) || !strings.Contains(out.Content, "[REDACTED]") {
		t.Fatalf("expected Basic base64 credential scrubbed, got: %s", out.Content)
	}

	// 2. json_body 回退模式不双重包裹 {"body": {...}}
	out2, _ := callTool.Execute(&tools.ToolContext{Context: context.Background()}, json.RawMessage(`{
		"extension": "oa-ref",
		"action": "RawPayload",
		"args": {"body": {"hello": "world"}}
	}`))
	if out2.IsError || capturedBody != `{"hello":"world"}` {
		t.Fatalf("expected unwrapped json_body {\"hello\":\"world\"}, got isErr=%v body=%s", out2.IsError, capturedBody)
	}

	// 3. 参数类型错误拦截（integer 传 string）
	badType, _ := callTool.Execute(&tools.ToolContext{Context: context.Background()}, json.RawMessage(`{
		"extension": "oa-ref",
		"action": "CreateItem",
		"args": {"item_id": 1, "supplier_id": "not-an-int"}
	}`))
	if !badType.IsError || !strings.Contains(badType.Content, ErrInvalidArgs) {
		t.Fatalf("expected ERR_INVALID_ARGS for wrong param type, got %+v", badType)
	}

	// 4. 路径参数 ".." 拦截
	dotDot, _ := callTool.Execute(&tools.ToolContext{Context: context.Background()}, json.RawMessage(`{
		"extension": "oa-ref",
		"action": "CreateItem",
		"args": {"item_id": "..", "supplier_id": 1}
	}`))
	if !dotDot.IsError || !strings.Contains(dotDot.Content, ErrInvalidArgs) {
		t.Fatalf("expected ERR_INVALID_ARGS for '..' path param, got %+v", dotDot)
	}

	// 5. 中文 UTF-8 截断不产生非法字节
	zh := strings.Repeat("中文日志内容测试", 50)
	trimmed, truncated, _ := ApplyOutputBudget(zh, 50, 10, t.TempDir(), "zh", "test")
	if !truncated || !utf8.ValidString(trimmed) {
		t.Fatalf("expected valid UTF-8 after Chinese truncation, truncated=%v valid=%v", truncated, utf8.ValidString(trimmed))
	}
}

func TestExtensions_ConcurrentRaceSafety(t *testing.T) {
	cwd := t.TempDir()
	extDir := filepath.Join(cwd, ".gp", "extensions", "race-guide")
	_ = os.MkdirAll(extDir, 0o755)
	_ = os.WriteFile(filepath.Join(extDir, "extension.yaml"), []byte(`
api_version: gp.ext/v1
name: race-guide
description: race test
adapter:
  type: guide
`), 0o644)

	reg := NewRegistry(RegistryConfig{Load: LoadOptions{Cwd: cwd}})
	defer reg.CloseAll()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, _ = reg.Describe(context.Background(), "race-guide", "", true, 5)
				_ = reg.ListStatus()
				_ = reg.EnabledCount()
				if id%2 == 0 {
					reg.SetEnabled("race-guide", j%2 == 0)
				}
				if id == 0 && j%7 == 0 {
					reg.Reload()
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestExtensions_SecondPassAuditRegressions(t *testing.T) {
	// 1. OpenAPI >=64 字符同前缀长路径不得死循环 + 顶层数组 body + 字段名冲突自动切换 json_body
	cwd := t.TempDir()
	extDir := filepath.Join(cwd, ".gp", "extensions", "oa-edge")
	_ = os.MkdirAll(extDir, 0o755)
	_ = os.WriteFile(filepath.Join(extDir, "openapi.yaml"), []byte(`
openapi: "3.0.0"
paths:
  /api/v1/organizations/{org_id}/projects/{proj_id}/environments/settings/alpha:
    get:
      summary: long path alpha
  /api/v1/organizations/{org_id}/projects/{proj_id}/environments/settings/beta:
    get:
      summary: long path beta
  /api/v1/batch:
    post:
      operationId: BatchCreate
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: array
              items: { type: string }
  /api/v1/users/{id}:
    put:
      operationId: UpdateUser
      parameters:
        - name: id
          in: path
          required: true
          schema: { type: string }
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              properties:
                id: { type: string }
                name: { type: string }
`), 0o644)
	_ = os.WriteFile(filepath.Join(extDir, "extension.yaml"), []byte(`
api_version: gp.ext/v1
name: oa-edge
description: edge cases
adapter:
  type: openapi
  spec_file: openapi.yaml
  base_url: "http://mock.local:8005"
`), 0o644)

	reg := NewRegistry(RegistryConfig{Load: LoadOptions{Cwd: cwd}})
	defer reg.CloseAll()

	reg.mu.RLock()
	oa := reg.exts["oa-edge"].adapter.(*openapiAdapter)
	reg.mu.RUnlock()

	defs, err := oa.DiscoverActions(context.Background())
	if err != nil {
		t.Fatalf("DiscoverActions failed (or looped): %v", err)
	}
	if len(defs) != 4 {
		t.Fatalf("expected 4 deduplicated actions, got %d", len(defs))
	}

	// 验证顶层数组 body 与冲突字段 json_body 直发
	var lastBody string
	oa.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			lastBody = string(b)
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
		}),
	}
	resArr := reg.Call(context.Background(), "oa-edge", "BatchCreate", map[string]any{"body": []any{"a", "b"}}, CallContext{})
	if resArr.IsError || lastBody != `["a","b"]` {
		t.Fatalf("expected top-level array body [\"a\",\"b\"], got err=%v content=%s body=%s", resArr.IsError, resArr.Content, lastBody)
	}
	resPut := reg.Call(context.Background(), "oa-edge", "UpdateUser", map[string]any{"id": "u1", "body": map[string]any{"id": "u1", "name": "alice"}}, CallContext{})
	if resPut.IsError || !strings.Contains(lastBody, `"name":"alice"`) {
		t.Fatalf("expected collision fallback json_body with name=alice, got err=%v content=%s body=%s", resPut.IsError, resPut.Content, lastBody)
	}

	// 2. 联合类型 Schema ["string", "null"] 不应跳过必填与类型校验
	unionSchema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"note": {"type": ["string", "null"]},
			"count": {"type": "integer", "default": 5}
		},
		"required": ["note"]
	}`)
	if filled, err := validateAndFillArgs(map[string]any{"note": nil}, unionSchema); err != nil || filled["count"] != float64(5) {
		t.Fatalf("expected nullable union field to accept nil and fill default count=5, got filled=%v err=%v", filled, err)
	}
	if _, err := validateAndFillArgs(map[string]any{"note": 123}, unionSchema); err == nil {
		t.Fatalf("expected union [string,null] to reject int 123")
	}

	// 3. MCP 磁盘缓存指纹必须包含 DefaultRisk：调高为 destructive 后旧缓存不能绕过高危门禁
	cacheDir := t.TempDir()
	specRead := &ExtensionSpec{
		Name:       "cached-mcp",
		Governance: GovernanceSpec{DefaultRisk: RiskRead},
		Adapter:    AdapterSpec{Type: "mcp", Command: "echo"},
	}
	a1 := newMCPAdapter(specRead, cacheDir).(*mcpAdapter)
	a1.saveDiskCache([]ActionDef{{Name: "drop_table", Risk: RiskRead, InputSchema: json.RawMessage(`{"type":"object"}`)}})

	specDestructive := &ExtensionSpec{
		Name:       "cached-mcp",
		Governance: GovernanceSpec{DefaultRisk: RiskDestructive},
		Adapter:    AdapterSpec{Type: "mcp", Command: "echo"},
	}
	a2 := newMCPAdapter(specDestructive, cacheDir).(*mcpAdapter)
	if len(a2.cached) != 0 {
		t.Fatalf("expected MCP disk cache to invalidate when default_risk changed to destructive, got %+v", a2.cached)
	}

	// 4. CLI 缺失键不得渲染 "<no value>"，且整数 0 在 {{if .n}} 中为假值
	argv, err := renderArgvTemplates([]string{"--ns", "{{.ns}}", "{{if .n}}--n={{.n}}{{end}}"}, map[string]any{"n": json.Number("0")})
	if err != nil {
		t.Fatalf("renderArgvTemplates failed: %v", err)
	}
	if len(argv) != 2 || argv[1] != "" {
		t.Fatalf("expected clean empty string and omitted zero-if arg, got %#v", argv)
	}

	// 5. RegisterFilteredTools 严格遵守细粒度白名单
	tReg := tools.NewRegistry()
	_ = RegisterFilteredTools(tReg, reg, true, false)
	if _, ok := tReg.Get("ext_describe"); !ok {
		t.Fatalf("expected ext_describe registered")
	}
	if _, ok := tReg.Get("ext_call"); ok {
		t.Fatalf("expected ext_call NOT registered when allowCall=false")
	}
}
