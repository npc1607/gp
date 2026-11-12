# gp - High Performance Go Coding Agent Harness

> 基于 Go 语言构建的现代化、高性能、通用 AI Coding Agent Harness。
> 深度参考 Pi 架构，原生支持 **Goroutine 协程级轻量子 Agent 调度与管理**。

---

## 一、项目设计哲学与定位

`gp` 的目标是成为具备极高扩展性、极速冷启动、极低内存消耗的通用 Agent 基础设施：
1. **极致轻量**：子 Agent 为纯粹的 **Goroutine（协程）**，单 Agent 运行时内存 **< 1MB**，启动耗时 **< 1ms**。
2. **多模式对齐**：完全对齐 Pi 的 4 种运行模式（`interactive`, `print`, `json`, `rpc`）。
3. **安全与隔离**：彻底杜绝全局状态污染（Cwd / 环境变量按上下文注入），支持只读沙箱与高危命令人机审批。
4. **易于扩展**：标准 Go 接口抽象 + 支持嵌入式 JS 脚本与 MCP 协议动态扩展。

---

## 二、工程目录结构与 Pi 源码映射对照表

本工程采用了业界标准的 **Standard Go Project Layout**，与 `docx/pi` 源码形成了清晰的一一对应关系：

```text
/Users/pcong/Project/gp
├── cmd/
│   └── gp/                    # CLI 命令行入口 (main.go)
│
├── internal/
│   ├── ai/                    # [对齐 Pi 的 packages/ai]
│   │   ├── types.go           # Message, Role, ToolCall, TokenUsage, StreamChunk
│   │   ├── provider.go        # Provider 统一流式推理抽象接口
│   │   └── providers/         # OpenAI / Anthropic / Gemini / DeepSeek 驱动实现
│   │
│   ├── agent/                 # [对齐 Pi 的 packages/agent]
│   │   ├── agent.go           # Agent 实例定义与状态机
│   │   └── loop.go            # Agent 推理与多轮工具调用主循环 (AgentLoop)
│   │
│   ├── core/                  # [对齐 Pi 的 packages/coding-agent/src/core]
│   │   ├── session/           # 会话分支树 (SessionTree)、持久化与纯内存会话
│   │   ├── compaction/        # 上下文滑动窗口智能压缩与摘要折叠
│   │   ├── resources/         # 统一资源发现 (Skills, ContextFiles, PromptTemplates)
│   │   └── settings/          # 配置管理 (~/.gp/settings.json 与项目级配置)
│   │
│   ├── scheduler/             # [★ Go 专属核心革新]
│   │   ├── governor.go        # 协程资源调度器 (并发信号量 Semaphore + Token 桶)
│   │   ├── blackboard.go      # 多 Agent 线程安全共享黑板 (Blackboard)
│   │   └── dag.go             # DAG 依赖拓扑排序与就绪事件触发引擎
│   │
│   ├── tools/                 # [对齐 Pi 的 packages/coding-agent/src/core/tools]
│   │   ├── tool.go            # Tool 统一契约接口、ToolContext 与 Registry 注册中心
│   │   └── builtin/           # read, edit, write, bash, grep, find 等内置工具
│   │
│   ├── mcp/                   # [对齐 Pi 的 packages/mcp]
│   │   └── client.go          # Model Context Protocol stdio / sse 传输客户端
│   │
│   ├── extensions/            # [对齐 Pi 的 packages/coding-agent/src/core/extensions]
│   │   ├── extension.go       # Go 原生扩展契约
│   │   └── js/                # 基于 Goja 的嵌入式 JavaScript/TypeScript 脚本引擎
│   │
│   ├── tui/                   # [对齐 Pi 的 packages/tui]
│   │   └── model.go           # 基于 Charmbracelet Bubbletea 的终端 UI 渲染引擎
│   │
│   └── modes/                 # [对齐 Pi 的 packages/coding-agent/src/modes]
│       ├── modes.go           # 统一运行器接口
│       ├── interactive/       # TUI 全屏交互
│       ├── print/             # 单次直接输出 (-p)
│       ├── jsonl/             # 结构化 JSONL 事件流 (--mode json)
│       └── rpc/               # JSON-RPC 长连接守护进程 (--mode rpc)
│
├── pkg/
│   └── sdk/                   # 暴露给外部 Go 项目调用的 SDK 包 (对齐 Pi SDK)
├── docx/pi/                   # Pi 原版源码仓库 (作为架构对照参考)
├── go.mod
└── ARCHITECTURE.md            # 本详细架构规范
```

---

## 三、如何像 Pi 一样方便地进行扩展？

`gp` 在设计上严格遵循**“开闭原则（OCP）”**与**“依赖倒置原则（DIP）”**，提供四种不同层次的扩展机制：

### 1. 扩展新工具（Add a New Tool）
只需实现 `internal/tools/tool.go` 中的 `Tool` 接口：
```go
type MyCustomTool struct{}

func (t *MyCustomTool) Name() string { return "my_custom_tool" }
func (t *MyCustomTool) Description() string { return "工具说明..." }
func (t *MyCustomTool) InputSchema() json.RawMessage { return []byte(`{"type":"object"}`) }
func (t *MyCustomTool) Execute(ctx *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
    // 业务逻辑...
    return &tools.ToolResult{Content: "执行成功"}, nil
}

// 注册
registry.Register(&MyCustomTool{})
```

### 2. 扩展新大模型驱动（Add a New Model Provider）
只需实现 `internal/ai/types.go` 中的 `Provider` 接口：
```go
type MyProvider struct{}

func (p *MyProvider) Name() string { return "my_provider" }
func (p *MyProvider) StreamChat(ctx context.Context, req *ai.ChatRequest) (<-chan ai.StreamChunk, error) {
    ch := make(chan ai.StreamChunk)
    go func() {
        defer close(ch)
        // 发起 HTTP 请求并将流式 token 写入 ch
    }()
    return ch, nil
}
```

### 3. 动态脚本扩展（免编译挂载 JS 扩展）
在 `internal/extensions/js` 中集成了 Goja 引擎，用户可在本地编写 `plugin.js`：
```javascript
// 免重新编译 Go，直接热加载！
gp.registerTool({
  name: "weather",
  description: "查询天气",
  execute: function(args) {
    return "北京晴天 20°C";
  }
});
```

### 4. 接入海量 MCP 工具（Model Context Protocol）
配置 `~/.gp/mcp.json`，`gp` 会自动将 MCP 服务暴露的工具桥接为内置 `Tool`：
```json
{
  "mcpServers": {
    "git": {
      "command": "uvx",
      "args": ["mcp-server-git"]
    }
  }
}
```

---

## 四、快速上手与验证

### 1. 编译全部包
```bash
cd /Users/pcong/Project/gp
go build ./...
```

### 2. 运行 CLI
```bash
# 查看版本
go run ./cmd/gp -v

# 单次命令直出模式
go run ./cmd/gp -p "分析当前目录"
```
