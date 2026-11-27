# gp — General-Purpose Go Agent Harness

`gp` 是一个基于 Go 语言构建的高性能通用 AI Agent 运行时底座（General-Purpose Agent Harness）。它以单二进制形态提供协程级多智能体编排、SQLite WAL 断点续跑、Epoch 冻结上下文缓存与混合检索记忆引擎、AGP（Agent Gateway Protocol）JSONL 服务网关，以及融合 [`pi-tidy`](https://github.com/Andy8647/pi-tidy) 与 [`pi-zentui`](https://github.com/lmilojevicc/pi-zentui) 交互体验的原生终端界面。

---

## 核心特性

### 1. 长程上下文工程与无损记忆体系（E4-A + 混合检索）
面向长对话工具调用循环与高前缀缓存（Prompt Cache）命中率设计，兼顾零第三方依赖与断点安全：

- **Epoch 冻结观测掩码（Observation Masking）**
  - 按逻辑工具调用轮次推进冻结边界（默认步长 `16` 个工具调用轮次、保留最近 `5` 个工具调用轮次原文），仅对超出冻结边界的历史大段工具输出原地折叠为轻量占位符，确保同一 Epoch 内消息前缀字节级稳定，支撑上游 94%–99.5% KV Cache 命中率。
- **L1 结构化约束账本（`ContextLedger`）**
  - 在 `Compact()` 阶段由模型增量抽取 `add` / `supersede` / `resolve` 约束事件并附带原始 `provenance` 指针；同一 Epoch 内冻结账本渲染顺序（`CanonicalRender`），避免频繁变更打穿缓存前缀。
- **零依赖混合检索引擎（`internal/core/retrieval`）**
  - 集成 `CodeTokenize`（驼峰 / `snake_case` / 路径 / 中文分词）、`BM25` 稀疏倒排、`HashNGramEmbedder(128)` 本地向量、`HeuristicExtractor` + `EntityGraph` 异构实体图、`PersonalizedPageRank`（含 Hub 度惩罚）、`ACTRTracker` 间隔强化、`ZlibDensityPenalty` 重复日志压制与 `SelectBudgetedMMR` 预算去重选择。
  - **路径 A（Epoch 冻结 `<working_set>` 主动注入）**：对已被 `Compact()` 截断或被 Epoch 掩码的工具历史进行跨跳召回，按 `(epochID, compactionAnchorID)` 冻结并持久化快照（`SchemaVersion=2`），自动剥离飞书网关注入的 `[角色人设]...[飞书上下文: ...]` 外壳；未发生压缩且无折叠的短会话保持零额外注入与零构建开销。
  - **路径 B（只读回查工具组）**：`NewAgent` 自动挂载 `search_context`（全分支混合图检索定位 `entry_id` 预览）、`recall_entries`（按 `entry_id` 精确取回无损原文）与 `restore_observation`（按 `call_id` 分页恢复被折叠工具输出）。
- **可观测性与 SQLite 兼容迁移**
  - 每轮记录 `context_metrics`（含 `strategy_mode`、`epoch_id`、`ledger_hash`、`working_set_tokens`、`retrieval_build_ms`、`graph_matched_entities`、`cache_read_tokens`、`recall_calls` / `restore_calls` / `search_calls` 等），支持旧版 `gp.db` 启动时幂等 `ALTER TABLE` 升级。

### 2. AGP 网关与安全策略守卫（`gp serve` / `rpc`）
- **AGP（Agent Gateway Protocol）JSONL 协议**（`internal/agp` + `internal/modes/rpc`）
  - 支持 Unix Socket / TCP 逐行 JSON 双向通信，涵盖 `hello`、`run`、`abort`、`ping`、`approval_reply`、`question_reply`、`reset_session`、`list_sessions`、`resume_session`、`delete_session` 及只读可观测接口 `ctx_stat`。
  - 持久化活跃会话映射（支持 `/new` / `/resume` 重启恢复）与 Provider 连接复用，适配飞书网关、IDE 插件与自动化前端接入。
- **ExecutionPolicy 与 RiskGuard 拦截**
  - 支持工作目录、允许/禁止路径列表、读写与 Bash 执行开关及 `CapabilityRule` 外部能力约束；越界访问或高风险命令自动下发 `approval_request` 挂起等待人工审批或通过 `ask_user_question` 发起结构化问卷交互。

### 3. `pi-tidy` × `pi-zentui` 沉浸式终端 UI
基于 Bubbletea + Lipgloss 从零重构，兼顾高信息密度与透明背景视觉秩序：

- **Turn & Tool 分层折叠（`pi-tidy`）**
  - **Turn 折叠**：完整一轮执行结束后自动收拢为单行摘要，展开后左侧 `│` 轨道线直接汇入最终回复框的 `├` 转角。
  - **Tool Group 折叠**：连续工具调用收敛为摘要行，支持鼠标左键点击任意摘要行/工具行逐层展开，或使用 `Ctrl+O` 全局切换。
  - **关键操作常驻（`AlwaysShowTools`）**：`edit`、`write`、`subagent` 默认以独立圆角透明框（`╭╮╰╯`）常驻展示。
  - **Thinking 与错误去重**：推理过程折叠为单行摘要（`Ctrl+T` 或点击展开）；连续重复错误自动合并。
- **代码 Diff 与 Markdown 增强**
  - 宽屏下 `edit` 与 `diff` 代码块支持并排双栏 Diff（`old │ new` + 行号 + `┃` 变更条），窄屏（`< 72` 列）自动回退统一 Diff；围栏代码块附带语言分割线与 `#rrggbb` 颜色圆点预览；Bash 命令词法高亮。
- **模块化编辑器与状态栏（`pi-zentui`）**
  - 支持 `opencode` / `rail` / `minimalist` 三套实时布局预设、Working Line 顶边框动画、Starship Footer（目录、Git 分支与脏状态、Go 版本、上下文占用与 Token/耗时统计）、`Vim` 编辑模式（`/vim`）、`InputBus` 运行中入队/挤压合并，以及 `KeymapRegistry` 统一快捷键管理。

### 4. 协程级子 Agent 与多生态扩展
- **轻量并发派发与拓扑可视化**：主 Agent（`root`）通过 `subagent` 工具以 Goroutine 并发派生只读侦察或执行角色子 Agent，支持 `Tab` / `Shift+Tab` 切换视角及 `Ctrl+G` 开关 **DAG 任务拓扑侧栏**；内置 `Governor` 限流与共享 `Blackboard`。
- **Durable 持久韧性**：SQLite WAL + `SessionTree`（支持工具参数清洗与断链自愈）+ Task Checkpoint 原子落盘，中断后可执行 `gp resume` 断点续跑。
- **多 Provider 与插件拓展**：内置 OpenAI / DeepSeek、Anthropic 原生（`/v1/messages` + `cache_control` 断点 + Thinking Signature 回放）以及 `plugins/qoder`（自动鉴权轮换与缓存字段映射）、`plugins/askuser`、stdio MCP Server、Skills 渐进披露（超预算自动收起子文档按需读取）与纯 Go 嵌入式 JavaScript（Goja）扩展工具。

---

## 快捷键与斜杠指令

| 快捷键 / 指令 | 作用 |
| --- | --- |
| `Enter` | 发送消息或执行内置 UI 指令（Agent 运行中自动入队） |
| `Esc` | 运行中立即中断当前推理；空闲时回退队列指令、清空输入框或切换 Vim Normal 模式 |
| `Alt/Cmd+↑` (`Ctrl+↑`) | 将最新一条未处理队列指令回退到输入框 |
| `Ctrl+E` | 将队列中多条待处理指令原地挤压合并为 1 条 |
| `Ctrl+S` | 将当前模型保存为全局默认启动模型 |
| `Ctrl+O` | 展开 / 折叠所有 Turn 与工具调用详情 |
| `Ctrl+T` | 展开 / 折叠最近一条 Thinking 思考框 |
| `Ctrl+G` | 打开 / 关闭左侧 DAG 任务拓扑侧栏（存在子任务时可用） |
| `Ctrl+]` | 在 `AskUser` 交互式问卷弹窗打开时折叠 / 展开弹窗 |
| `Tab` / `Shift+Tab` | 在 `root` 与各子 Agent 视图间循环切换 |
| `Up` / `Down` / `PgUp` / `PgDown` / 滚轮 | 滚动对话视口 |
| 鼠标左键点击 | 点击 Turn 摘要、工具组标题或单行工具原地展开/折叠 |
| `/models` / `/login` / `/logout` | 选择切换模型或管理 Provider 登录态 |
| `/resume` | 浏览并恢复历史 SQLite 会话 |
| `/compact [focus]` | 手动触发上下文两级压缩并更新 `ContextLedger` |
| `/skills` / `/mcp` | 查看并管理当前加载的 Skills 与 MCP Server 状态 |
| `/zentui preset opencode\|rail\|minimalist` | 实时切换编辑器、用户消息与状态栏样式预设 |
| `/tidy` / `/tidy turn` | 开关 Turn 自动折叠 / 手动切换最近一轮折叠 |
| `/vim [on\|off]` | 开关输入框 Vim 键位模式 |
| `/clear` | 清空当前 Agent 缓冲区 |

---

## 目录结构

```text
.
├── cmd/gp/                       # CLI 主入口、模式路由与日志初始化
├── internal/
│   ├── agent/                    # ReAct 循环、Epoch 掩码、WorkingSet 注入、回查工具与指标采集
│   ├── agp/                      # Agent Gateway Protocol (AGP) JSONL 帧与策略协议定义
│   ├── ai/providers/
│   │   ├── anthropic/            # 原生 Anthropic /v1/messages 适配器 (含 cache_control 与 Thinking)
│   │   └── openai/               # OpenAI / DeepSeek 兼容流式适配器 (含 cached_tokens 映射)
│   ├── core/
│   │   ├── auth/                 # 本地凭证与多域鉴权管理
│   │   ├── compaction/           # ContextLedger 状态机、两级摘要压缩与 Token 估算
│   │   ├── resources/            # Skills / AGENTS.md 发现与渐进披露 (Progressive Disclosure)
│   │   ├── retrieval/            # 纯标准库混合检索引擎 (BM25 + Dense + PPR + ACT-R + MMR)
│   │   ├── session/              # SessionTree 无损分支树与压缩锚点偏移计算
│   │   ├── settings/             # 层叠配置 (~/.gp/settings.json & <cwd>/.gp/settings.json)
│   │   ├── storage/sqlite/       # 内存与 SQLite WAL 存储引擎及 context_metrics 迁移
│   │   └── task/                 # 任务所有权树与 Checkpoint 断点续跑状态机
│   ├── extensions/js/            # Goja 嵌入式 JS 扩展工具引擎
│   ├── mcp/                      # stdio MCP 客户端与工具桥接
│   ├── modes/                    # interactive (TUI) / print (-p) / rpc (AGP serve) / subagent 装配
│   ├── scheduler/                # Governor 并发限流、Blackboard 黑板与 Task DAG 调度
│   ├── tools/builtin/            # read / write / edit / bash / subagent / ask_peer 内置工具
│   └── tui/                      # pi-tidy + pi-zentui 渲染引擎、Vim 状态与 KeymapRegistry
├── plugins/                      # askuser 交互问卷与 qoder 多域鉴权推理插件
└── docs/                         # 架构设计、DTOC 防污染及线上 Cache 观测记录文档
```

---

## 构建与运行

### 1. 检查、测试与编译

```bash
# 运行静态检查 (go vet + golangci-lint)
make lint

# 运行全量单元测试与竞态检测 (-race)
make test

# 编译本地二进制至 ./bin/gp
make build

# 跨平台静态交叉编译 (darwin-arm64 / darwin-amd64 / linux-amd64 / windows-amd64)
make cross-compile
```

### 2. 启动方式

```bash
# 查看版本号
./bin/gp -v

# 启动交互式 TUI（默认激活 read, write, edit, bash, subagent, ask_user_question，
# 并由 Agent 自动挂载 restore_observation, recall_entries, search_context）
./bin/gp

# 开启调试日志（写入 ./logs/gp-debug-<时间戳>.log，非 debug 模式下 TUI 默认静默 slog）
./bin/gp --debug

# 指定模型进入交互模式（注意：Go flag 需放在位置参数前）
./bin/gp -model deepseek-flash

# 单次非交互输出模式 (-p)
./bin/gp -model deepseek-flash -p "分析当前目录的代码结构"

# 从 SQLite 检查点恢复最近中断的任务（可选传入 session_id）
./bin/gp resume

# 启动 AGP RPC 服务网关（默认监听 unix:///tmp/gp-agent.sock，可改 tcp://127.0.0.1:9527）
./bin/gp serve --listen unix:///tmp/gp-agent.sock
# 或通过 Makefile：make serve
```

### 3. 配置与环境变量

支持通过 `~/.gp/settings.json`（全局）或 `<cwd>/.gp/settings.json`（项目级）进行层叠配置，环境变量优先级最高（兼容 `OPENAI_BASE_URL` / `OPENAI_API_KEY`）：

```bash
export GP_MODEL="deepseek-flash"
export GP_BASE_URL="https://api.deepseek.com/v1"
export GP_API_KEY="sk-..."
# 可选：开启调试日志文件输出
export GP_DEBUG=1
```

常用启动参数：
- `-model`：指定模型标识（默认由配置文件决定，缺省为 `antigravity/gemini-4-argon`）
- `-tools`：逗号分隔的工具白名单（如 `-tools read,bash,subagent`）
- `-db`：指定 SQLite 状态库路径（默认 `~/.gp/gp.db`）
- `-mode`：运行模式（`interactive`、`print`、`rpc`、`resume`）
- `-listen`：`--mode rpc` 下的监听地址；`gp serve` 子命令支持 `--listen` / `--db` / `--model` / `--tools`
- `-debug`：开启详细调试日志到 `./logs/gp-debug-*.log`
