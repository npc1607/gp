package rpc

import "gp/internal/agp"

// 协议帧与常量统一由 internal/agp 定义，这里保留别名，既有调用方（cmd/gp、
// 服务端实现与测试）无需改动即可继续使用 rpc.Request / rpc.Event 等符号。
type (
	Request    = agp.Request
	RunOptions = agp.RunOptions
	Usage      = agp.Usage
	Event      = agp.Event
)

const (
	OpHello        = agp.OpHello
	OpRun          = agp.OpRun
	OpResetSession = agp.OpResetSession
	OpAbort        = agp.OpAbort
	OpPing         = agp.OpPing
)

const (
	EventHelloAck      = agp.EventHelloAck
	EventStarted       = agp.EventStarted
	EventThinking      = agp.EventThinking
	EventToolStart     = agp.EventToolStart
	EventToolEnd       = agp.EventToolEnd
	EventSubagentStart = agp.EventSubagentStart
	EventSubagentEnd   = agp.EventSubagentEnd
	EventDelta         = agp.EventDelta
	EventFinal         = agp.EventFinal
	EventResetAck      = agp.EventResetAck
	EventAbortAck      = agp.EventAbortAck
	EventPong          = agp.EventPong
	EventError         = agp.EventError
)
