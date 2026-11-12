# gp (Go Coding Agent Harness)

`gp` 是一个基于 Go 语言的高性能、模块化 AI Coding Agent Harness。深度参考了 Pi（`docx/pi`）的架构思想，并在 Go 运行时中原生实现了 **Goroutine 协程级轻量子 Agent** 与高扩展性插件生态。

## ✨ 核心特性

- **🚀 协程级子 Agent**：基于 Go 原生 Goroutine 派生子 Agent，单 Agent 内存 `< 1MB`，启动耗时 `< 1ms`。
- **🛡️ 全局资源调度**：内置并发信号量与 Token 桶，严格防范并发失控与模型 API 429 报错。
- **📦 模块化对齐 Pi**：完全拆解并重构了 Pi 的 AI 抽象、会话树（SessionTree）、工具注册中心与多模式运行架构。
- **🔌 灵活易扩展**：原生支持 Go 接口扩展、嵌入式 JavaScript/TypeScript 动态热插拔扩展以及 MCP（Model Context Protocol）协议服务。

## 📖 核心文档导航

- **项目详细技术方案**：[Go语言高性能通用Coding-Agent-Harness技术方案.md](./docs/Go语言高性能通用Coding-Agent-Harness技术方案.md)
- **详细架构与 Pi 源码映射**：[ARCHITECTURE.md](./ARCHITECTURE.md)
- **六周敏捷实施推进计划**：[DEV_PLAN.md](./DEV_PLAN.md)

## 🛠️ 编译与测试

```bash
# 编译全量包
go build ./...

# 运行 CLI
go run ./cmd/gp -v
```
