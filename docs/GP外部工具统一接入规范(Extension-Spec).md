# GP 外部工具统一接入规范（Extension Spec v1.1）

> 本文档定义 `gp` 新一代外部能力统一接入架构（**Extension / ToolPack**），用于取代当前割裂、高 Token 消耗、重常驻进程的 `Skill + MCP` 双轨接入方式，并与 `gp` 既有核心能力（Prompt Cache 前缀稳定、`internal/core/retrieval.BM25`、`Epoch Observation Masking`、`tools.ReplayMode`、`auth.Store`、`ask_user_question` 审批交互）深度融合。

---

## 1. 背景与现状痛点

当前 `gp` 通过两套割裂机制接入外部能力：
- **Skill**（`internal/core/resources/`）：仅提供纯文本 `SKILL.md` 指南，无结构化执行契约，模型需手工拼 `bash` 命令并脆弱地解析终端输出；
- **MCP**（`internal/mcp/`）：启动时全量拉起所有 stdio 子进程并将全部工具 Schema 塞入每一轮 `req.Tools`。

在实际工程使用中暴露出 6 个核心痛点：
1. **首轮 Token 爆炸**：几十上百个 MCP 工具 Schema 常驻每轮请求，未执行任务即消耗 10k~20k input tokens。
2. **Prompt Cache 稳定性脆弱**：动态增删外部工具会直接改变 `req.Tools` 前缀，导致整段 KV Cache 失效。
3. **协议过重、接入成本高**：像 `data-test-platform` 这类本身已有 HTTP API 或 CLI 的本地工具，为了给 Agent 使用还需额外开发维护常驻 MCP Server。
4. **知识与执行割裂**：同一个外部能力被迫拆成“一份 Skill 文档 + 一套 MCP 配置”，难以分发、启停和版本管理。
5. **第三方 Schema 方言冲突**：外部 MCP 返回的非标准 JSON Schema（如 `items: true` / tuple array）会直接触发 Moonshot/Kimi 等严格上游的 HTTP 400。
6. **治理边界粗糙**：缺少统一的风险分级（只读/写/高危）、子 Agent 权限收敛、密钥脱敏、输出体积预算与懒加载/空闲回收机制。

---

## 2. 核心设计原则

1. **统一抽象（One Pack = Docs + Actions + Governance）**
   - 外部能力以 **Extension 能力包** 为唯一管理单元，在一个目录内同时声明元数据、使用指引（`GUIDE.md`）、可执行动作、鉴权引用与安全策略。
2. **从轻到重的多协议光谱（Zero-Boilerplate Adapters）**
   - 不强绑 MCP，按接入复杂度提供 6 种原生适配器：
   ```text
   guide (纯知识) → cli (命令行) → script (stdin/stdout JSON) → http / openapi (REST 服务) → mcp (长连接生态) → go plugin (内核扩展)
   ```
3. **渐进式披露 + Prompt Cache 零破坏（Cache-Safe Progressive Disclosure）**
   - 主模型请求中的 `req.Tools` 永远只增加 **2 个静态不变的内置元工具**（`ext_describe`、`ext_call`），绝不因外部扩展增减或激活而改动 `req.Tools` 列表；
   - System Prompt 中的能力目录严格按 `name` 字典序确定性拼接，最大程度保护 KV Cache 前缀命中率。
4. **Harness 原生治理（Fail-Closed & Anti-Pollution）**
   - 强制声明 `risk` 风险等级、禁止明文密钥、强制脱敏、参数数组化防命令注入、输出限流落盘衔接 `Epoch Observation Masking`。

---

## 3. 目录结构、发现优先级与配置持久化

### 3.1 能力包目录规范
每个 Extension 位于独立目录，最小仅需一个 `extension.yaml`：
```text
<ext-name>/
├── extension.yaml      # 必填：能力包清单（元数据、适配器、动作、权限）
├── GUIDE.md            # 可选：深度使用指南、排错 SOP、示例（按需注入）
└── scripts/            # 可选：script / cli 适配器依赖的本地脚本或二进制
```

### 3.2 发现路径与覆盖优先级（由低到高）
1. 全局共享目录：`~/.agents/extensions/<name>/extension.yaml`
2. 全局用户目录：`~/.gp/extensions/<name>/extension.yaml`
3. 自定义扩展目录：`settings.json` 中 `extension_dirs` 声明的额外路径
4. 工程共享目录：`<repo>/.agents/extensions/<name>/extension.yaml`
5. 工程专用目录：`<repo>/.gp/extensions/<name>/extension.yaml`

同名 Extension 按“就近覆盖”原则生效（记录来源 `source: global | custom | project`）。

### 3.3 `settings.json` 新增配置项
在 `internal/core/settings/settings.go` 中与现有 `skill_dirs` / `disabled_skills` / `disabled_mcps` 对齐扩展：
```json
{
  "extension_mode": "unified",
  "extension_dirs": [],
  "disabled_extensions": ["some-unused-ext"],
  "extension_allow_destructive": false
}
```
- `extension_mode`：`"unified"`（默认，启用统一 Extension + 桥接旧 Skill/MCP）或 `"legacy"`（回退旧全量注入模式，便于灰度验证）。

---

## 4. `extension.yaml` 完整清单规范

### 4.1 顶层 Schema 与字段约束
```yaml
api_version: gp.ext/v1              # 必填，固定版本标识
name: data-test-platform            # 必填，正则 ^[a-z0-9][a-z0-9_-]{0,63}$
version: "1.0.0"                    # 可选，语义化版本号
description: "一句话说明能力用途"      # 必填，建议 <= 120 字符（用于 Stage 0 目录）
triggers:                           # 可选，轻量意图关键词（用于 BM25 检索加权）
  - data-service
  - 慢SQL
guide: GUIDE.md                     # 可选，相对 Extension 目录的路径（禁止 .. 越界）
enabled: true                       # 可选，默认 true（可被 settings.disabled_extensions 覆盖）

governance:                         # 可选，能力包级安全与治理策略
  default_risk: read                # read | write | destructive（默认 write，保守原则）
  subagent_policy: read_only        # inherit | read_only | deny（默认 read_only）
  output_max_bytes: 16384           # 单次最大输出字节数（默认 16KB）
  output_max_lines: 200             # 单次最大输出行数（默认 200 行）
  timeout: 15s                      # 单次动作默认超时（默认 30s，最大 5m）

auth:                               # 可选，统一鉴权声明（禁止明文密钥）
  type: bearer                      # none | bearer | header | basic | query
  header: Authorization             # type=header 时必填；type=bearer 默认 Authorization
  query_param: api_key              # type=query 时必填
  prefix: "Bearer "                 # type=bearer 默认 "Bearer "
  token_ref: env://DATA_TEST_TOKEN  # env://<VAR> | auth://<provider> | cmd://<command>
  required: false                   # 为 true 且解析为空时，调用前直接返回 ERR_AUTH_MISSING

healthcheck:                        # 可选，供 `/ext test <name>` 探测连通性
  http_get: /api/process/status     # http/openapi 适配器：探测路径
  cli_args: ["--version"]           # cli 适配器：探测参数
  timeout: 3s

adapter:                            # 必填，传输适配器配置（见 4.2）
  type: http
  ...

actions:                            # 动作声明列表（guide 可空；openapi/mcp 可自动发现或白名单筛选）
  - name: recent_logs               # 正则 ^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$
    description: "..."
    risk: read                      # 覆盖 governance.default_risk
    timeout: 10s                    # 覆盖 governance.timeout
    params:                         # 参数定义（标准 JSON Schema 子集）
      limit:
        type: integer               # string | integer | number | boolean | array | object
        required: false
        default: 50
        enum: []
        description: "返回条数"
```

### 4.2 变量展开与安全转义规则
为防止模板注入与命令注入，变量展开严格分为两层：
1. **配置加载期环境变量展开**：仅支持 `${VAR}` 与 `${VAR:-default}`（用于 `base_url`、`cwd`、`env` 等配置项），不执行任何 shell 命令。
2. **调用期参数插值（按适配器隔离）**：
   - **`cli`**：**严禁拼接 shell 字符串**，必须采用参数数组模板（如 `args: ["get", "pod", "-n", "{{.namespace}}"]`），直接传给 `exec.CommandContext(ctx, command, argv...)`；可选参数支持 `{{if .selector}}-l={{.selector}}{{end}}` 省略空元素；
   - **`http` / `openapi`**：路径变量（`/api/items/{id}`）强制 `url.PathEscape`，Query 参数强制 `url.Values.Encode`，JSON Body 强制 `json.Marshal`；
   - **`script`**：参数一律通过 `stdin` 传递标准 JSON，不拼接命令行。

### 4.3 六种适配器详细规范

#### ① `guide`（纯知识/工作流指南，替代原 Skill）
```yaml
adapter:
  type: guide
guide: GUIDE.md
```
- 无进程、无 `actions`，模型通过 `ext_describe(name)` 按需加载完整 `GUIDE.md`。

#### ② `cli`（无状态命令行封装）
```yaml
adapter:
  type: cli
  command: kubectl
  cwd: "${GP_CWD}"                  # 可选，默认继承当前 Agent 的 ToolContext.Cwd
  env:
    KUBECONFIG: "${KUBECONFIG}"
actions:
  - name: pod_logs
    description: 查看指定 Pod 最近日志
    risk: read
    args: ["logs", "{{.pod}}", "-n", "{{.namespace}}", "--tail={{.tail}}"]
    output: text                    # text | json（json 会自动校验与美化裁剪）
    params:
      pod:       { type: string, required: true }
      namespace: { type: string, default: "default" }
      tail:      { type: integer, default: 100 }
```

#### ③ `script`（极简 stdin/stdout JSON 协议，替代 80% 轻量自研 MCP）
适用于需要复杂签名、多步聚合、私有 SDK 封装的场景，任何语言 15 行即可接入，无需 JSON-RPC 握手与常驻服务：
```yaml
adapter:
  type: script
  exec: ./scripts/tool.py           # 相对 Extension 目录，禁止跳出目录边界
  interpreter: python3              # 可选；省略时直接执行带 +x 权限的二进制/脚本
```
- **输入（stdin，单次完整 JSON）**：
  ```json
  {
    "api_version": "gp.ext/v1",
    "action": "query_qc_tags",
    "args": { "supplier_id": "sup_123" },
    "auth": { "token": "<resolved-if-configured>" },
    "context": { "cwd": "/path/to/repo", "agent_id": "root", "timeout_ms": 15000 }
  }
  ```
- **输出（stdout，标准 JSON）**：
  ```json
  {
    "ok": true,
    "content": "Found 3 QC tags for sup_123",
    "data": { "tags": ["t1", "t2", "t3"] },
    "error": "",
    "error_code": ""
  }
  ```
- **容错规则**：若进程退出码非 0 或 stdout 非合法 JSON，框架自动捕获 `stderr`（经密钥脱敏后）包装为 `ERR_UPSTREAM` 结构化错误。

#### ④ `http`（REST / 内部 HTTP 服务）
```yaml
adapter:
  type: http
  base_url: "${DATA_TEST_PLATFORM_URL:-http://127.0.0.1:9099}"
  default_headers:
    Accept: application/json
actions:
  - name: remote_log_search
    method: POST
    path: /api/logs/remote/search   # 支持路径占位符 /api/jobs/{job_id}
    query_params: [env]             # 指定哪些 params 放 URL query（GET 默认全放 query）
    body_mode: json                 # json（默认，POST/PUT/PATCH 将非 path/query 参数序列化为 JSON）| raw | none
    extract: "data.logs[:50]"       # 可选：轻量响应裁剪路径（点号访问 + 数组切片），减少上下文噪音
```

#### ⑤ `openapi`（大型 OpenAPI 3.0 自动发现）
```yaml
adapter:
  type: openapi
  spec_file: ../data-service/openapi.yaml   # 或 spec_url: http://127.0.0.1:9099/api/openapi
  base_url: "http://127.0.0.1:8005"
  include_tags: [QCService, JobService]     # 可选白名单
  exclude_paths: ["/internal/*"]            # 可选黑名单
  method_risk_map:                          # 按 HTTP 方法自动推断默认风险等级
    GET: read
    HEAD: read
    POST: write
    PUT: write
    PATCH: write
    DELETE: destructive
```
- 启动时仅做轻量解析并建立 `retrieval.BM25` 内存索引，**不把 140 个接口塞进主请求**；模型通过 `ext_describe(ext, query)` 按需召回 Top-5 接口 Schema。

#### ⑥ `mcp`（兼容既有 MCP 生态，升级为懒启动 + 空闲回收）
```yaml
adapter:
  type: mcp
  transport: stdio                  # stdio | streamable_http
  command: npx
  args: ["-y", "@modelcontextprotocol/server-postgres"]
  url: ""                           # transport=streamable_http 时必填
  idle_ttl: 10m                     # 空闲超时自动 Close 进程，默认 10m
  include_tools: []                 # 可选白名单
  exclude_tools: []                 # 可选黑名单
```
- **懒启动策略**：`gp` 启动时**不拉起** MCP 子进程（优先读取上次缓存在 `~/.gp/cache/ext-mcp-<name>.json` 的工具目录建索引；首次 `ext_describe` 无缓存或首次 `ext_call` 时才启动并刷新缓存）；
- **自动 Schema 清洗**：发现到的所有工具 Schema 自动经过 `ai.SanitizeToolSchema`（消除 Moonshot/Kimi 等方言 400）；
- **崩溃自愈**：调用时发现子进程已退出，自动重启重连一次后再返回错误。

---

## 5. 模型交互契约（渐进式披露 + 静态双元工具）

### 5.1 Stage 0：System Prompt 能力目录（确定性排序，保护 Prompt Cache）
所有已启用的 Extension 按 `name` 升序排列，每个占 1 行（约 20~35 tokens），总目录设置软上限（如 ≤ 2000 tokens）：
```text
[Available Extensions]
- data-test-platform [http]: 本地与云端 data-service 接口测试、实时日志与慢 SQL 诊断 (actions: recent_logs, log_stats, remote_log_search, proxy_call, process_status)
- git-commit-guide [guide]: Git 提交规范与 scope 检查指南 (guide-only)
- lark-suite [mcp]: 飞书文档、任务与多维表格操作 (42 actions indexed; call ext_describe with query)
Workflow: Call `ext_describe(extension, query?)` to inspect instructions and action schemas, then invoke via `ext_call(extension, action, args)`.
```

### 5.2 Stage 1：内置双元工具精确定义

#### 工具 1：`ext_describe`（`ReplaySafe`）
- **Input Schema**：
  ```json
  {
    "type": "object",
    "properties": {
      "extension": { "type": "string", "description": "Extension name from [Available Extensions]" },
      "query": { "type": "string", "description": "Optional search keywords to filter actions when an extension has many actions" },
      "include_guide": { "type": "boolean", "description": "Whether to include GUIDE.md content (default: true when query is empty)" },
      "limit": { "type": "integer", "description": "Max number of matching actions to return (default: 8, max: 20)" }
    },
    "required": ["extension"]
  }
  ```
- **检索与裁剪策略**：
  - 动作数 ≤ 10 且 `query == ""`：返回 `GUIDE.md` + 全部动作精简 Schema；
  - 动作数 > 10 或提供了 `query`：使用 `internal/core/retrieval.BM25` 对每个动作的 `name + description + tags + param_names` 建索引并评分，返回 Top-K（默认 8）最相关动作；
  - 若 `GUIDE.md` 超过 8KB，优先返回标题大纲 + 前 6KB 并提示可用 `read` 查阅原文件路径。

#### 工具 2：`ext_call`（动态风险判定）
- **Input Schema**：
  ```json
  {
    "type": "object",
    "properties": {
      "extension": { "type": "string", "description": "Target extension name" },
      "action": { "type": "string", "description": "Action name within the extension" },
      "args": { "type": "object", "description": "Arguments object matching the action schema" }
    },
    "required": ["extension", "action"]
  }
  ```
- **统一执行管线（7 步）**：
  1. **定位**：解析 Extension 与 Action，检查启用状态与子 Agent 权限（`subagent_policy`）；
  2. **参数校验**：校验必填项、基础类型、`enum`、填充 `default`；缺失或类型错误时返回 `ERR_INVALID_ARGS` 并附带该 Action 的正确 Schema（帮助模型一轮自纠）；
  3. **鉴权解析与脱敏注册**：解析 `auth.token_ref`（`env://` / `auth://` / `cmd://`），并将非空密钥注册到本次调用的 `SecretScrubber`；
  4. **安全门禁**：若 `risk == destructive`，检查 `extension_allow_destructive` 或通过 `tools.SendUIEvent` / `ask_user_question` 弹交互确认；非交互模式且未显式授权时 **Fail-Closed** 返回 `ERR_DENIED`；
  5. **适配器执行**：按超时控制执行（`mcp` 懒启动并续期 `idle_ttl`）；
  6. **输出裁剪与防污染落盘**：
     - 先应用可选 `extract` 字段提取；
     - 经过 `SecretScrubber` 脱敏所有已解析密钥；
     - 若超出 `output_max_bytes` 或 `output_max_lines`，将完整原文落盘到 `~/.gp/tmp/ext-outputs/<session_id>/<ext>_<action>_<ts>.out`，返回截断摘要 + 完整文件路径 + 总字节/行数（供模型按需 `read` 分段查看，并由 `Epoch Observation Masking` 自动折叠旧轮次）；
  7. **结构化回执**：返回 `tools.ToolResult`，在 `Details` 中附带 `{extension, action, adapter_type, duration_ms, truncated, spill_path, risk}` 供 TUI 工具框与可观测日志消费。

### 5.3 标准化错误码契约
所有失败统一返回结构化文本头 `[<ERROR_CODE>] <message>`，让模型做出确定性决策：

| 错误码 | 含义 | 模型预期行为 |
| :--- | :--- | :--- |
| `ERR_NOT_FOUND` | Extension 或 Action 不存在 | 重新核对目录或调用 `ext_describe` |
| `ERR_DISABLED` | Extension 已被用户禁用 | 告知用户通过 `/ext` 启用 |
| `ERR_INVALID_ARGS` | 参数缺失或类型不符（附带正确 Schema） | 按附带 Schema 修正参数重试 |
| `ERR_AUTH_MISSING` | 必需凭证缺失（如环境变量未设或未 `/login`） | 提示用户配置指定环境变量或执行 `/login` |
| `ERR_DENIED` | 高危操作被用户拒绝或子 Agent 无权限 | 立即停止该操作并向用户汇报 |
| `ERR_ADAPTER_OFFLINE` | 目标本地服务未启动或连接被拒（如 `127.0.0.1:9099`） | 提示用户启动对应服务 |
| `ERR_TIMEOUT` | 执行超过设定超时时间 | 缩小查询范围或调整参数 |
| `ERR_UPSTREAM` | 上游 HTTP 非 2xx / CLI 非 0 / MCP 业务报错 | 分析上游错误原因并处理 |

---

## 6. 核心 Go 架构与代码落点

### 6.1 推荐包结构（`internal/extensions/`）
```text
internal/extensions/
├── spec.go             # ExtensionSpec / ActionSpec / AuthSpec / GovernanceSpec 结构体与校验
├── loader.go           # 多目录扫描、优先级覆盖、legacy skills/mcp 桥接加载
├── registry.go         # 线程安全注册中心、BM25 动作索引缓存、热重载 Reload()
├── auth.go             # env:// / auth:// / cmd:// 凭证解析器 + SecretScrubber 脱敏器
├── budget.go           # extract 轻量提取、行数/字节截断与超长落盘
├── adapter.go          # Adapter 统一接口定义
├── adapter_guide.go    # 纯文档适配器
├── adapter_cli.go      # 数组参数模板 + exec.CommandContext（防注入）
├── adapter_script.go   # stdin/stdout JSON 协议适配器
├── adapter_http.go     # HTTP/REST 适配器（复用 http.Client 连接池）
├── adapter_openapi.go  # OpenAPI 3.0 解析与按需动作生成
├── adapter_mcp.go      # 包装 internal/mcp.Client：懒启动、idle_ttl 回收、自动重连
└── tools.go            # 向 internal/tools.Registry 注册 ext_describe 与 ext_call
```

### 6.2 核心 Go 接口定义
```go
package extensions

import (
	"context"
	"encoding/json"
	"time"
)

type RiskLevel string

const (
	RiskRead        RiskLevel = "read"
	RiskWrite       RiskLevel = "write"
	RiskDestructive RiskLevel = "destructive"
)

type ActionDef struct {
	Name        string
	Description string
	Risk        RiskLevel
	Timeout     time.Duration
	InputSchema json.RawMessage // 已通过 ai.SanitizeToolSchema 规范化
	Tags        []string
}

type InvokeRequest struct {
	Action  string
	Args    map[string]any
	Cwd     string
	AgentID string
	Auth    ResolvedAuth // 已解析的临时凭证（仅内存传递，严禁序列化落盘）
}

type InvokeResult struct {
	Content    string
	IsError    bool
	ErrorCode  string
	Duration   time.Duration
	Truncated  bool
	SpillPath  string
	RawBytes   int
}

type Adapter interface {
	Type() string
	DiscoverActions(ctx context.Context) ([]ActionDef, error)
	Invoke(ctx context.Context, req InvokeRequest) (*InvokeResult, error)
	HealthCheck(ctx context.Context) error
	Close() error
}
```

### 6.3 与 `gp` 既有模块的精确对接
- **`internal/agent/agent.go`**：系统提示词装配时调用 `extReg.RenderDirectoryPrompt()` 写入 Stage 0 目录；
- **`internal/tools/tool.go`**：`ext_describe` 实现 `ReplaySafe`；`ext_call` 实现 `ReplayInterrupt`（后续可按动作级 `risk` 细分）；
- **`internal/core/auth/auth.go`**：`auth://<provider>` 直接调用 `authStore.ResolveCredential(provider)`；
- **`internal/core/retrieval/bm25.go`**：每个多动作 Extension 内部持有一个 `*retrieval.BM25`，按动作索引零外部依赖实现高质量检索；
- **`internal/tui/footer.go`**：可选注册 `ext` StatusSegment（如 `ext:4`），展示当前已启用能力包数量。

---

## 7. 安全、幂等与生命周期治理

| 维度 | 强制规范 |
| :--- | :--- |
| **路径沙箱** | `guide`、`script.exec`、`openapi.spec_file` 校验 `filepath.Clean` 后必须位于允许边界内，防止 `../../etc/passwd` 越界读取。 |
| **密钥零泄漏** | `extension.yaml` 禁止写明文密钥；`SecretScrubber` 自动把长度 ≥ 8 的已解析 token 在输出、错误、落盘摘要中替换为 `[REDACTED]`。 |
| **子 Agent 权限收敛** | 子 Agent 默认只允许调用 `risk == read` 动作（`subagent_policy: read_only`），防止并发子 Agent 误触发外部写入或破坏性操作。 |
| **MCP 懒加载与回收** | 首次 `ext_call`（或无缓存时的 `ext_describe`）才启动子进程；后台轻量定时器每分钟巡检，超过 `idle_ttl` 自动 `Close()`；`gp` 退出 `defer extReg.CloseAll()` 保证零僵尸进程。 |
| **故障完全隔离** | 单个 `extension.yaml` 语法错误只记录该扩展状态为 `error`（在 `/ext` 面板标红展示原因），绝不阻塞 `gp` 启动或其他扩展运行。 |

---

## 8. 向下兼容与平滑迁移策略

为保证现有用户零成本过渡，启用 `extension_mode: "unified"` 后自动执行以下桥接：
1. **旧 `skills/` 自动桥接**：`internal/core/resources.ResourceLoader` 发现的每个 `SKILL.md` 自动包装为 `adapter.type = guide` 的虚拟 Extension（同名 `extension.yaml` 优先覆盖旧 Skill）；
2. **旧 `mcp.json` 自动桥接**：`internal/mcp.DiscoverConfig` 发现的每个 MCP Server 自动包装为 `adapter.type = mcp` 的懒加载 Extension（启用空闲回收 + Schema 自动清洗）；
3. **旧命令兼容**：`/skills` 与 `/mcp` 保留为 `/ext` 的过滤视图别名（或平滑过渡），用户习惯不受破坏。

---

## 9. TUI 统一管理交互（`/ext`）

复用与 `/statusline`、`/skills`、`/mcp` 一致的 `OverlayState` 圆角交互面板：
```text
╭─ Extensions (4/5 on) ─────────────────────────────────────────────────────────────────╮
│ Search: ▌                                                                             │
│ ───────────────────────────────────────────────────────────────────────────────────── │
│ › [x] data-test-platform  本地与云端接口测试与慢SQL诊断   [http · 5 actions · online] │
│   [x] git-commit-guide    Git 提交规范与 scope 检查      [guide · project]            │
│   [x] postgres-local      本地只读数据库查询             [mcp · idle · 12 actions]    │
│   [ ] legacy-heavy-mcp    重型外部服务                   [mcp · disabled]             │
│ ───────────────────────────────────────────────────────────────────────────────────── │
│ ↑/↓ move · Space toggle · t healthcheck · Ctrl+R reload · Type to filter · Esc close  │
╰───────────────────────────────────────────────────────────────────────────────────────╯
```
- 快捷指令：`/ext`（打开弹层）、`/ext reload`（热重载清单）、`/ext enable|disable <name>`、`/ext test <name>`。

---

## 10. 典型接入模板库

### 10.1 HTTP 服务样例：`data-test-platform`（基于真实路由）
文件位置：`<repo>/.gp/extensions/data-test-platform/extension.yaml`

```yaml
api_version: gp.ext/v1
name: data-test-platform
version: "1.0.0"
description: 本地与云端 data-service 接口测试、实时日志检索与慢 SQL 诊断平台
triggers: [data-service, 慢SQL, 日志, 接口测试, trace_id, openapi]
guide: GUIDE.md

governance:
  default_risk: read
  subagent_policy: read_only
  output_max_bytes: 16384
  output_max_lines: 160
  timeout: 15s

healthcheck:
  http_get: /api/process/status
  timeout: 3s

adapter:
  type: http
  base_url: "${DATA_TEST_PLATFORM_URL:-http://127.0.0.1:9099}"

actions:
  - name: recent_logs
    description: 获取最近的本地/云端日志条目（含日志级别、caller、trace_id 与 SQL 信息）
    risk: read
    method: GET
    path: /api/logs/recent
    params:
      limit: { type: integer, default: 50, description: "返回条数" }

  - name: log_stats
    description: 获取 SQL 性能统计与 Top 慢 SQL 排行榜（耗时、行数、源码行号）
    risk: read
    method: GET
    path: /api/logs/stats

  - name: remote_log_search
    description: 按关键词、trace_id 或时间范围检索 Dev/Test 云端服务日志
    risk: read
    method: POST
    path: /api/logs/remote/search
    params:
      keyword:    { type: string, description: "关键词或 trace_id" }
      start_time: { type: string, description: "起始时间（可选）" }
      end_time:   { type: string, description: "结束时间（可选）" }

  - name: proxy_call
    description: 通过测试平台代理调用指定 data-service 接口（自动注入网关鉴权与 trace_id）
    risk: write
    method: POST
    path: /api/proxy
    params:
      method:  { type: string, required: true, enum: [GET, POST, PUT, DELETE] }
      url:     { type: string, required: true, description: "目标接口路径或完整 URL" }
      headers: { type: object, description: "附加请求头" }
      body:    { type: object, description: "JSON 请求体" }

  - name: process_status
    description: 查看本地 data-service 进程运行状态
    risk: read
    method: GET
    path: /api/process/status
```

### 10.2 Script 脚本样例（任意语言 15 行接入）
```yaml
api_version: gp.ext/v1
name: supplier-diag
description: 供应商数据一致性诊断脚本
adapter:
  type: script
  exec: ./scripts/diag.py
  interpreter: python3
auth:
  type: bearer
  token_ref: env://INTERNAL_API_TOKEN
actions:
  - name: check_qc_tags
    description: 检查指定供应商的 QC 标签映射是否缺失
    risk: read
    params:
      supplier_id: { type: string, required: true }
```

---

## 11. 分阶段落地路线图与验收标准

### Phase 1：核心框架与轻量适配器（MVP）
- **范围**：`internal/extensions/`（`spec` / `loader` / `registry` / `auth` / `budget` / `guide` / `cli` / `script` / `http`）+ `ext_describe` & `ext_call` 元工具 + System Prompt Stage 0 目录注入 + `data-test-platform` 样例验证。
- **验收标准**：
  - 首轮新增工具定义固定为 2 个（`ext_describe` / `ext_call`），`req.Tools` 不随扩展数量增长；
  - `cli` 参数包含 `; rm -rf /` 等恶意字符时不会被 shell 解释；
  - `auth.token_ref` 解析出的密钥在错误输出与截断摘要中全部脱敏为 `[REDACTED]`；
  - `data-test-platform` 接入后，模型可在 2 步内完成 `ext_describe` → `ext_call("data-test-platform", "log_stats")` 查出慢 SQL。

### Phase 2：MCP 懒加载与旧生态统一桥接
- **范围**：`adapter_mcp.go`（懒启动 + 工具目录磁盘缓存 + `idle_ttl` 回收 + `ai.SanitizeToolSchema`）+ 旧 `skills/` 与 `mcp.json` 自动桥接 + `settings.extension_mode` 切换开关。
- **验收标准**：
  - 配置 5 个 MCP Server 时 `gp` 启动耗时不增加（不预拉子进程）；
  - 空闲超过 `idle_ttl` 后子进程自动回收，再次调用自动无缝重启；
  - Kimi/Moonshot 调用带非标准 Schema 的 MCP 工具不再出现 HTTP 400。

### Phase 3：OpenAPI 自动发现与 `/ext` TUI 面板
- **范围**：`adapter_openapi.go`（OpenAPI 3.0 解析 + BM25 动作检索）+ TUI `/ext` 交互式管理弹层（启停、热重载、连通性探测）+ 底部 Statusline 可选 `ext` 指标段。
- **验收标准**：
  - 挂载 140+ 接口的 `data-service/openapi.yaml` 时，Stage 0 仅占 1 行目录（< 40 tokens），`ext_describe("data-service-api", "GetSupplierQcTagIds")` 毫秒级精准召回目标接口；
  - `/ext` 面板支持实时启停、按 `t` 探测连通性、按 `Ctrl+R` 热重载，无需重启 `gp`。
