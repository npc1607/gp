# gp — General Purpose Agent Harness

> **`gp`**，意为 **“狗屁 (GouPi)”**。
> 这是作者的一点恶趣味：世间太多冠冕堂皇的概念，不如一个简单抗造、真正跑在生产上的轻量系统。
> 名字叫狗屁，但干的都是最硬核的工程底座。

---

## 🌟 并非仅为 Coding 而生：通用 Agent 运行时基础设施

`gp` 是一个基于 Go 语言构建的**通用、高性能、工业级 AI Agent 运行时底座 (General-purpose Agent Harness)**。

软件编码（Coding）只是其内置工具集所展现的一种典型场景。在架构底层，`gp` 是一套能够驱动**复杂业务流协作、故障应急战情室、博弈对局推演、自动化运维以及多智能体协商**的通用外壳：

- **🚀 Goroutine 协程级轻量子 Agent**：单 Agent 内存占用 **`< 1MB`**，启动延迟 **`< 1ms`**。彻底终结传统框架“派生 5 个子 Agent 启动 5 个独立操作系统进程吞噬 1GB 内存导致 OOM”的噩梦；
- **🛡️ 吸收 Pi Durable 精髓的持久韧性**：基于 SQLite WAL 事务与任务检查点（Checkpoint）。无论服务器断电、容器重启还是被 `kill -9` 强杀，执行 `gp resume` 即可**秒级断点续跑**，具备 Exactly-Once 语义与自底向上级联取消；
- **🚦 全局资源守卫 (Governor)**：内置高精度并发信号量与滑动窗口 Token 桶限流，海量协程高并发协同下保证大模型 API **零 429 报错**；
- **🖥️ 纯粹极简的沉浸式 TUI**：对齐 Pi 原生视觉哲学，拒绝廉价低质 Emoji，单栏全宽沉浸式会话流，原生支持**多轮调用折叠组 (`▾ 7 calls · read×4 · 3 thoughts · 1.4s`)** 与精准代码 Diff 实时高亮；
- **🔌 无限扩展性与 Pi 生态对齐**：原生支持 MCP（Model Context Protocol）协议网关、嵌入式纯 Go JavaScript 扩展引擎（免编译动态热加载 `.js` 脚本）、`SKILL.md` 知识库与全套斜杠指令集（Slash Commands）。

---

## 🏗️ 架构全景

```text
                           gp Core Harness
 ┌─────────────────────────────────────────────────────────────────┐
 │                      通用会话与工作流引擎                        │
 │  - SessionTree (有向无环分支树, 支持撤销/重试/纯内存会话)          │
 │  - Governor (并发信号量 Semaphore + 滑动窗口 Token 桶限流)       │
 │  - Blackboard (线程安全共享黑板, 跨 Agent 事实与状态同步)        │
 │  - Task DAG (基于拓扑唤醒的有向无环图调度与自愈修复回路)           │
 └───────────────────────────────┬─────────────────────────────────┘
                                 │
     ┌───────────────────────────┼───────────────────────────┐
     ▼                           ▼                           ▼
┌──────────────┐        ┌─────────────────┐        ┌──────────────────┐
│ Durable 存储 │        │ 协程级子 Agent  │        │ 全场景接入面     │
│ - SQLite WAL │        │ - Goroutine 池  │        │ - Bubbletea TUI  │
│ - Checkpoint │        │ - A2A 问答交互  │        │ - Print 管道 (-p)│
│ - 幂等重放   │        │ - 最小权限裁剪  │        │ - JSONL / RPC    │
└──────────────┘        └─────────────────┘        └──────────────────┘
                                 │
     ┌───────────────────────────┴───────────────────────────┐
     ▼                                                       ▼
┌─────────────────────────────────┐       ┌───────────────────────────┐
│           基础能力扩展          │       │        外部生态桥接       │
│ - 内置工具 (read/write/edit/bash)│       │ - MCP 协议网关 (stdio)    │
│ - 大模型驱动 (OpenAI/DeepSeek)  │       │ - 嵌入式 Goja JS 脚本引擎  │
│ - 斜杠指令集 (/help, /diff, ...)│       │ - SKILL.md / AGENTS.md 规范│
└─────────────────────────────────┘       └───────────────────────────┘
```

---

## 📖 核心文档索引

- **系统技术方案**：[Go语言高性能通用Coding-Agent-Harness技术方案.md](./docs/Go语言高性能通用Coding-Agent-Harness技术方案.md)
- **工程架构与 Pi 源码映射**：[ARCHITECTURE.md](./ARCHITECTURE.md)
- **敏捷实施推进计划**：[DEV_PLAN.md](./DEV_PLAN.md)
- **TUI 交互与插件扩展图解**：[TUI交互架构与插件扩展设计图解.html](./docs/TUI交互架构与插件扩展设计图解.html)

---

## 🛠️ 构建与快速上手

### 1. 编译全量包与运行测试
```bash
# 运行全量单元测试与数据竞争检测 (-race)
make test

# 编译本地单一二进制
make build

# 一键跨平台静态交叉编译 (输出 darwin-arm64, darwin-amd64, linux-amd64, windows-amd64)
make cross-compile
```

### 2. 命令行与交互模式运行
```bash
# 查看版本元数据
./bin/gp -v

# 单次管道处理模式 (-p)
./bin/gp -p "分析当前目录的代码结构"

# 启动沉浸式交互 TUI 界面
./bin/gp

# 崩溃恢复与断点续跑 (从 SQLite 检查点自动恢复未竟任务)
./bin/gp resume
```

### 3. 配置管理
支持在 `~/.gp/settings.json`（全局）或 `<cwd>/.gp/settings.json`（项目级）配置模型与端点，支持环境变量最高优先级覆盖：
```bash
export GP_MODEL="deepseek-chat"
export GP_BASE_URL="https://api.deepseek.com"
export GP_API_KEY="sk-..."
```
