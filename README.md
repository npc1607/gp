# gp — General-Purpose Go Agent Harness

`gp` 是一个基于 Go 语言构建的高性能通用 AI Agent 运行时底座（General-Purpose Agent Harness）。它以单二进制形态提供协程级多智能体编排、SQLite WAL 断点续跑、MCP 与 JS 扩展网关，以及深度复刻 [`pi-tidy`](https://github.com/Andy8647/pi-tidy) 与 [`pi-zentui`](https://github.com/lmilojevicc/pi-zentui) 交互体验的原生终端界面。

---

## 核心特性

### 1. `pi-tidy` × `pi-zentui` 沉浸式终端 UI
基于 Bubbletea + Lipgloss 从零重构，兼顾高信息密度与透明背景视觉秩序：

- **Turn & Tool 分层折叠（`pi-tidy`）**
  - **Turn 折叠**：完整一轮执行结束后自动收拢为单行摘要 `▸ took 12s · 󰚩 LLM 8s · tools 4s · 5 calls`，展开后左侧 `│` 轨道线直接汇入最终回复框的 `├` 转角。
  - **Tool Group 折叠**：连续工具调用收敛为 `▸ 5 calls ·  bash×3  󰈙 read×2 · 1.2s`，支持鼠标左键点击任意摘要行/工具行逐层展开，或使用 `Ctrl+O` 全局切换。
  - **关键操作常驻（`AlwaysShowTools`）**：`edit`、`write`、`subagent` 默认以独立圆角透明框（`╭╮╰╯`）常驻展示，不吞没核心变更与派发视图。
  - **Thinking 与错误去重**：推理过程折叠为单行摘要，按 `Ctrl+T` 或点击展开圆角思考框；连续重复错误自动合并为 `✗ error ×N`。
- **代码 Diff 与 Markdown 增强**
  - **并排双栏 Diff（Split Diff）**：`edit` 工具与 `diff` 代码块在宽屏下以 `old │ new` 双栏 + 行号 + `┃` 变更条 + 背景着色渲染，窄屏（`< 72` 列）自动回退统一 Diff。
  - **代码块分割线 & 颜色圆点**：Markdown 围栏代码块渲染为内嵌分割线区块（`─ go 󰆒 ───`），正文中的 `#rrggbb` 十六进制色值自动前置对应颜色的 `●` 指示点。
  - **Bash 词法高亮**：命令、参数 flag、字符串、变量与管道符分色渲染。
- **模块化编辑器与状态栏（`pi-zentui`）**
  - **Opencode Editor**：上下分割线 + 左侧 Accent Rail `│` + 上下留白 + 底部模型/Provider/Thinking 元数据行；运行中将 **Working Line**（内置 `claude-inspired`、`braille`、`star-bloom`、`pinwheel`、`pulse` 五套动画）直接嵌入顶边框。
  - **Starship Footer**：实时展示目录名、Git 分支与工作区脏状态（`*`）、Go 运行时版本、上下文占用（`ctx: X/128k`）、`↑in ↓out` Token 统计、费用与会话时长。
  - **实时切换预设**：输入 `/zentui preset opencode|rail|minimalist` 或 `/tidy` 即可即时调整界面布局。

### 2. 协程级子 Agent 与实时数据流可视化
- **轻量并发派发**：主协调 Agent（`root`）通过内置 `subagent` 工具以 Goroutine 并发派生子 Agent，支持按角色裁剪工具权限（`scout`、`reviewer`、`planner`、`implementer`、`verifier` 及自定义角色）。
- **Agent 间数据流卡片**：`subagent` 调用在主视图中实时展示每一路派发与返回：
  ```text
  ╭─ 󰚩 subagent 4 tasks: pro1, con1, pro2, con2 · 3.1s ──────────╮
  │  ✓ root ──▶ pro1 2.4s                                        │
  │    in : 正方一辩，80-120字开篇立论...                        │
  │    out: 工具的意义从来是放大能力，而不是替代能力...          │
  │  ◐ root ──▶ con1 1.8s                                        │
  │    in : 反方一辩，80-120字开篇立论...                        │
  │    out: streaming… (Tab to inspect)                          │
  ╰──────────────────────────────────────────────────────────────╯
  ```
- **多视角切换 & DAG 拓扑**：存在子 Agent 时顶部自动出现带实时状态图标（`◐` / `✓` / `✗`）的选项卡栏，按 `Tab` / `Shift+Tab` 即可切入任意子 Agent 查看其独立思考与输出；按 `Ctrl+G` 可开关左侧 32% 宽度的 **DAG 任务拓扑侧栏**。

### 3. Durable 持久韧性与并发调度
- **SQLite WAL + Checkpoint**：所有会话、生成轮次与工具执行状态原子落盘，异常中断后执行 `gp resume` 即可断点恢复。
- **全局资源守卫（Governor）**：内置并发信号量与 Token 桶限流，配合线程安全共享黑板（Blackboard）与 Task DAG 调度，支撑高并发多智能体协同。
- **生态扩展**：原生兼容 OpenAI / DeepSeek 接口规范，支持 stdio MCP Server 接入与纯 Go 嵌入式 JavaScript（Goja）工具扩展热加载。

---

## 快捷键与斜杠指令

| 快捷键 / 指令 | 作用 |
| --- | --- |
| `Enter` | 发送消息或执行内置 UI 指令 |
| `Ctrl+O` | 展开 / 折叠所有 Turn 与工具调用详情 |
| `Ctrl+T` | 展开 / 折叠最近一条 Thinking 思考框 |
| `Ctrl+G` | 打开 / 关闭左侧 DAG 任务拓扑侧栏（存在子任务时可用） |
| `Tab` / `Shift+Tab` | 在 `root` 与各子 Agent 视图间循环切换 |
| `Up` / `Down` / `PgUp` / `PgDown` / 滚轮 | 滚动对话视口 |
| 鼠标左键点击 | 点击 Turn 摘要、工具组标题或单行工具原地展开/折叠 |
| `/zentui preset opencode\|rail\|minimalist` | 实时切换编辑器、用户消息与状态栏样式预设 |
| `/tidy` / `/tidy turn` | 开关 Turn 自动折叠 / 手动切换最近一轮折叠 |
| `/clear` | 清空当前 Agent 缓冲区 |

---

## 目录结构

```text
.
├── cmd/gp/                 # CLI 主入口与模式路由
├── internal/
│   ├── agent/              # ReAct 状态循环、结构化生命周期事件与断点恢复
│   ├── ai/                 # 模型抽象契约与 OpenAI / DeepSeek 流式适配
│   ├── core/
│   │   ├── session/        # 会话分支树
│   │   ├── settings/       # 层叠配置管理 (~/.gp/settings.json & <cwd>/.gp/settings.json)
│   │   ├── storage/        # 内存与 SQLite WAL 持久化引擎
│   │   └── task/           # 任务所有权树与 Checkpoint 重放状态机
│   ├── extensions/js/      # Goja JS 扩展引擎
│   ├── mcp/                # MCP 客户端与工具桥接
│   ├── modes/              # interactive (TUI) / print (-p) / resume 运行模式
│   ├── scheduler/          # Governor 限流、Blackboard 黑板与 Task DAG 调度器
│   ├── tools/builtin/      # read / write / edit / bash / subagent / ask_peer 及执行接线
│   └── tui/                # pi-tidy + pi-zentui 原生终端渲染引擎
└── docs/                   # 技术方案文档
```

---

## 构建与运行

### 1. 测试与编译

```bash
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

# 启动交互式 TUI（默认包含 read, write, edit, bash, subagent）
./bin/gp

# 指定模型进入交互模式（注意：Go flag 需放在位置参数前）
./bin/gp -model deepseek-flash

# 单次非交互管道模式 (-p)
./bin/gp -model deepseek-flash -p "分析当前目录的代码结构"

# 从 SQLite 检查点恢复最近中断的任务
./bin/gp resume
```

### 3. 配置与环境变量

支持通过 `~/.gp/settings.json`（全局）或 `<cwd>/.gp/settings.json`（项目级）进行配置，环境变量优先级最高（兼容 `OPENAI_BASE_URL` / `OPENAI_API_KEY` 备选）：

```bash
export GP_MODEL="deepseek-flash"
export GP_BASE_URL="https://api.deepseek.com/v1"
export GP_API_KEY="sk-..."
```

可选常用启动参数：
- `-model`：指定模型标识
- `-tools`：逗号分隔的工具白名单（如 `-tools read,bash,subagent`）
- `-db`：指定 SQLite 状态库路径（默认 `.gp/gp.db`）
- `-mode`：运行模式（`interactive`、`print`、`resume`）
