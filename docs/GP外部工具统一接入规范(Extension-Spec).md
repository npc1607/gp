# GP 外部工具统一接入规范（Extension Spec v1）

> 本文档定义 `gp` 新一代外部能力统一接入架构（**Extension / ToolPack**），用于取代当前割裂、高 Token 消耗、重常驻进程的 `Skill + MCP` 双轨接入方式，并与现有 `gp` 上下文引擎（BM25 检索、Epoch Observation Masking、ReplayMode、Auth Store）深度融合。

---

## 1. 背景与现状痛点

当前 `gp` 通过两套独立机制接入外部能力：
- **Skill**（`internal/core/resources/`）：仅提供纯文本 `SKILL.md` 说明，缺少结构化动作契约，模型需手工拼 `bash` 命令并解析非结构化输出；
- **MCP**（`internal/mcp/`）：启动时全量拉起所有 stdio 子进程并将全部工具 Schema 塞入每轮请求。

在实际工程使用中暴露出 5 个核心痛点：
1. **首轮 Token 爆炸**：几十上百个 MCP 工具 Schema 常驻请求体，首轮未干活即可消耗 10k~20k input tokens。
2. **协议过重、接入成本高**：像 `data-test-platform` 这类本身已有 HTTP API 或 CLI 的本地工具，为了给 Agent 使用还需额外开发并维护常驻 MCP Server。
3. **知识与执行割裂**：同一个外部能力被迫拆分成“一份 Skill 文档 + 一套 MCP 配置”，难以分发、启停和版本管理。
4. **第三方 Schema 方言冲突**：部分外部 MCP 返回的非标准 JSON Schema（如 `items: true` / tuple array）会直接触发 Moonshot/Kimi 等严格上游的 HTTP 400。
5. **治理边界粗糙**：缺少统一的风险分级（只读/写/高危）、重放策略（`ReplaySafe`/`ReplayInterrupt`）、输出体积控制与懒加载/空闲回收机制。

---

## 2. 核心设计原则

1. **统一抽象（One Pack = Docs + Actions + Governance）**
   - 外部能力以 **Extension 能力包** 为唯一管理单元，在一个目录内同时声明元数据、使用指引（`GUIDE.md`）、可执行动作、鉴权方式与安全边界。
2. **从轻到重的多协议光谱（Zero-Boilerplate Adapters）**
   - 不强绑 MCP，按接入复杂度提供 6 种原生适配器：
   ```text
   guide (纯知识) → cli (命令行) → script (stdin/stdout JSON) → http / openapi (REST 服务) → mcp (长连接生态) → go plugin (内核扩展)
   ```
3. **渐进式披露（Progressive Disclosure）**
   - 常驻上下文仅保留极简能力索引表；详细指引与动作 Schema 仅在模型按需调用元工具时动态加载，彻底消除工具列表膨胀与脏 Schema 污染。
4. **Harness 原生治理**
   - 复用 `gp` 既有基础设施：`auth.db` 凭证管理、`bm25` 动作级检索、`ReplayMode` 幂等控制、输出预算截断与 `Epoch Observation Masking`。

---

## 3. 目录结构与发现规则

### 3.1 能力包目录规范
每个 Extension 位于独立目录，最小仅需一个 `extension.yaml`：
```text
<ext-name>/
├── extension.yaml      # 必填：能力包清单（元数据、适配器、动作、权限）
├── GUIDE.md            # 可选：深度使用指南、最佳实践、排错手册（按需注入）
└── scripts/            # 可选：script / cli 适配器使用的本地脚本或二进制
```

### 3.2 发现路径与覆盖优先级（由低到高）
1. 全局用户目录：`~/.gp/extensions/<name>/extension.yaml`
2. 工程共享目录：`<repo>/.agents/extensions/<name>/extension.yaml`
3. 工程专用目录：`<repo>/.gp/extensions/<name>/extension.yaml`

同名 Extension 按“就近覆盖”原则生效；同时向下兼容现有 `skills/`（自动视作 `adapter.type = guide` 的 Extension）和 `mcp.json`（自动视作 `adapter.type = mcp` 的 Extension）。

---

## 4. `extension.yaml` 清单规范

### 4.1 顶层结构
```yaml
api_version: gp.ext/v1
name: data-test-platform            # 唯一标识（小写字母/数字/连字符）
version: "1.0.0"
description: 本地与云端 data-service 接口测试、实时日志检索与慢 SQL 诊断平台
triggers:                           # 轻量意图触发词（供 Stage 0 索引与检索加权）
  - data-service
  - 慢SQL
  - 日志
  - 接口测试
  - trace_id
guide: GUIDE.md                     # 可选：按需加载的详细说明文件
enabled: true                       # 默认启用状态（可被用户配置覆盖）

auth:                               # 可选：统一鉴权声明（禁止明文硬编码密钥）
  type: bearer                      # none | bearer | header | basic
  header: Authorization             # 可选：自定义 header 名称
  token_ref: env://DATA_TEST_TOKEN  # 支持 env://<VAR> | auth://<provider> | cmd://<command>

adapter:                            # 必填：底层传输适配器配置
  type: http                        # guide | cli | script | http | openapi | mcp
  base_url: "${DATA_TEST_PLATFORM_URL:-http://127.0.0.1:9099}"
  timeout: 15s
  idle_ttl: 10m                     # 仅对有状态适配器（mcp）生效：空闲超时自动回收进程

defaults:                           # 可选：本能力包所有动作的默认治理策略
  risk: read                        # read | write | destructive
  output_max_bytes: 16384           # 单次输出上限（超限自动落盘并返回截断摘要 + 文件路径）
  output_max_lines: 200

actions:                            # 动作清单（openapi/mcp 适配器可自动发现，也可白名单精选）
  - name: recent_logs
    description: 获取最近的本地/云端日志流
    risk: read
    method: GET
    path: /api/logs/recent
    params:
      limit:
        type: integer
        default: 50
        description: 返回最近日志条数
```

### 4.2 六种适配器规范

| `adapter.type` | 适用场景 | 关键配置字段 | 生命周期 |
| :--- | :--- | :--- | :--- |
| **`guide`** | 纯工作流规范、代码风格、排错 SOP（替代原 Skill） | 仅需 `guide: GUIDE.md`，`actions` 可为空 | 无进程，零开销 |
| **`cli`** | 成熟命令行工具（`kubectl`、`gh`、`lark-cli` 等） | `command`、`args_template`、`cwd`、`env`、`output: text\|json` | 每次调用短生命周期子进程 |
| **`script`** | 复杂签名、多步聚合、私有 SDK 封装 | `exec: ./scripts/run.py`；约定 **`stdin` 接收 JSON 参数，`stdout` 返回 JSON 结果** | 每次调用短生命周期子进程 |
| **`http`** | REST / RPC 内部服务（如 `data-test-platform`） | `base_url`、动作级 `method` / `path` / `query` / `headers` / `body_template` / `extract`（可选 JSONPath 裁剪） | 复用 Go `http.Client` 连接池 |
| **`openapi`** | 大型 OpenAPI 3.0 服务（100+ 接口） | `spec_url` 或 `spec_file`、`base_url`、`include_tags` / `exclude_paths` | 启动解析规范并建 BM25 索引，按需查 Schema |
| **`mcp`** | 兼容既有 MCP Server 生态 | `transport: stdio\|http`、`command`/`args` 或 `url`、`idle_ttl` | **懒启动（首次调用拉起）+ 空闲 TTL 回收 + 自动 Schema 清洗** |

### 4.3 `script` 适配器极简协议（替代 80% 轻量自研 MCP）
任何语言（Python / Go / Node / Shell）只需 10~20 行即可接入，无需实现 JSON-RPC 握手与常驻服务：
- **输入（stdin）**：
  ```json
  {
    "action": "query_qc_tags",
    "args": { "supplier_id": "sup_123" },
    "context": { "cwd": "/path/to/repo", "agent_id": "root" }
  }
  ```
- **输出（stdout）**：
  ```json
  {
    "ok": true,
    "content": "Found 3 QC tags...",
    "data": { "tags": ["t1", "t2", "t3"] },
    "error": ""
  }
  ```
- 退出码非 0 且 stdout 非合法 JSON 时，自动将 stderr 包装为结构化错误返回给模型。

---

## 5. 模型交互契约：渐进式披露（Two-Stage Loading）

为避免外部工具挤占主上下文窗口并杜绝第三方 Schema 污染，主模型默认只看到**能力目录 + 2 个内置元工具**：

### 5.1 Stage 0：System Prompt 能力目录（常驻，极低 Token）
每启用一个 Extension 仅注入 1 行紧凑索引（约 20~30 tokens）：
```text
[Available Extensions]
- data-test-platform: 本地与云端 data-service 接口测试、实时日志与慢 SQL 诊断 [actions: recent_logs, log_stats, remote_log_search, proxy_call, process_status]
- git-commit-guide: Git 提交规范与 scope 检查指南 [guide-only]
- lark-suite: 飞书文档、任务与多维表格操作 [actions: 42 indexed, use ext_describe with query]
Use `ext_describe(extension, query?)` before calling an unfamiliar extension, then invoke via `ext_call(extension, action, args)`.
```

### 5.2 Stage 1：内置双元工具契约

1. **`ext_describe`**（`ReplaySafe`）
   - 参数：`{ "extension": string, "query"?: string, "limit"?: int }`
   - 行为：
     - 返回该 Extension 的 `GUIDE.md` 摘要（或全文）；
     - 若动作数 ≤ 10，直接返回所有动作的精简参数 Schema；
     - 若动作数 > 10（如 `openapi` 140 接口或大型 `mcp`），复用 `gp` 现有 `internal/core/retrieval/bm25.go` 按 `query` 检索并返回 **Top-K（默认 5）** 最相关动作 Schema。

2. **`ext_call`**（风险等级由目标动作的 `risk` 动态决定）
   - 参数：`{ "extension": string, "action": string, "args": object }`
   - 执行管线：
     1. **查找与鉴权**：解析目标 Extension 与 Action，动态解析 `auth.token_ref`（不落盘、不进日志）；
     2. **参数校验与 Schema 清洗**：校验必填项/类型，自动应用 `ai.SanitizeToolSchema`；
     3. **安全门禁**：`write` / `destructive` 动作触发确认或权限策略校验；
     4. **适配器执行**：`http`/`cli`/`script` 直接执行；`mcp` 若未启动则懒启动并续期空闲计时器；
     5. **结果防污染处理**：按 `output_max_bytes` / `output_max_lines` 裁剪（可选 `extract` 字段提取），超长原始结果写入会话临时文件并在返回值中给出路径提示，无缝衔接 `Epoch Observation Masking`。

---

## 6. 安全、幂等与生命周期治理

| 维度 | 规则 |
| :--- | :--- |
| **风险分级 (`risk`)** | `read`：只读无副作用，默认自动放行，映射为 `tools.ReplaySafe`；<br>`write`：有外部状态变更，映射为 `tools.ReplayInterrupt`；<br>`destructive`：删除/覆盖/线上变更，强制二次确认（`ask_user` / 审批门禁）。 |
| **密钥隔离** | `extension.yaml` 禁止明文写入密钥；支持 `env://`、`auth://`（复用 `~/.gp/auth.db`）、`cmd://`。错误日志与观测输出自动脱敏已解析的 token 值。 |
| **进程回收** | `mcp` 等有状态适配器首次调用才拉起，默认 `idle_ttl: 10m` 无调用自动关闭；`gp` 退出时统一清理全部子进程。 |
| **故障隔离** | 单个 Extension 配置错误、服务未启动或超时仅返回结构化错误提示，绝不阻塞 `gp` 启动或影响主对话。 |
| **热插拔** | 增删改 `extension.yaml` 后支持在 TUI 中一键 `/ext reload`，无需重启 `gp`。 |

---

## 7. TUI 统一管理交互（`/ext`）

用统一的 `/ext` 交互式弹层面板整合原有 `/skills` 与 `/mcp` 的碎片化管理（风格与现有 `/statusline`、`/skills` 一致）：
- `/ext`：弹出圆角管理面板，显示所有已发现能力包、适配器类型（`[http]` / `[cli]` / `[script]` / `[mcp]` / `[guide]`）、动作数、启用状态 `[x]`/`[ ]`、运行状态（`idle` / `running` / `offline`）；
- 快捷键：`↑/↓` 移动、`Space` 实时启停、`t` 测试连通性（healthcheck）、`r` 热重载清单、`Esc` 关闭；
- 快捷指令：`/ext reload`、`/ext enable|disable <name>`、`/ext test <name>`。

---

## 8. 实战接入样例：`data-test-platform`

基于 `/Users/pcong/Project/services/data-test-platform/internal/server/server.go` 现有真实路由，零代码接入只需创建 `<repo>/.gp/extensions/data-test-platform/extension.yaml`：

```yaml
api_version: gp.ext/v1
name: data-test-platform
version: "1.0.0"
description: 本地与云端 data-service 接口测试、实时日志检索与慢 SQL 诊断平台
triggers: [data-service, 慢SQL, 日志, 接口测试, trace_id, openapi]
guide: GUIDE.md

adapter:
  type: http
  base_url: "${DATA_TEST_PLATFORM_URL:-http://127.0.0.1:9099}"
  timeout: 15s

defaults:
  risk: read
  output_max_bytes: 16384
  output_max_lines: 160

actions:
  - name: recent_logs
    description: 获取最近的本地/云端日志条目（支持按级别、关键词、仅 SQL 或慢 SQL 过滤）
    risk: read
    method: GET
    path: /api/logs/recent
    params:
      limit: { type: integer, default: 50, description: "返回条数" }

  - name: log_stats
    description: 获取 SQL 性能统计与 Top 慢 SQL 排行榜（含耗时、行数、源码调用位置）
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

---

## 9. 分阶段落地路线图

- **Phase 1（核心规范与轻量适配器）**
  - 实现 `internal/extensions/`：清单发现与校验、`guide` / `cli` / `script` / `http` 适配器、鉴权引用解析、输出预算截断；
  - 注册内置元工具 `ext_describe`（含 BM25 动作检索）与 `ext_call`；
  - 将 `data-test-platform` 作为首个样例接入并端到端跑通。
- **Phase 2（MCP 懒加载与生态统一）**
  - 将现有 `internal/mcp/` 封装为 `mcp` 适配器，支持懒启动、空闲 TTL 回收与自动 Schema 清洗；
  - 自动将现有 `skills/` 与 `mcp.json` 映射进统一 Extension 注册中心，保持 100% 向下兼容。
- **Phase 3（OpenAPI 自动发现与 `/ext` TUI 面板）**
  - 实现 `openapi` 适配器（自动解析 `openapi.yaml` + BM25 索引）；
  - 在 TUI 中落地 `/ext` 交互式管理面板（启停、热重载、连通性探测），并在底部 statusline 可选展示活跃扩展数。
