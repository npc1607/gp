package tools

import "sync"

var (
	pluginToolMu    sync.RWMutex
	pluginFactories []func() Tool

	uiSenderMu sync.RWMutex
	uiSender   func(msg any)
)

// RegisterPluginTool 注册一个可插拔内置工具工厂（通常由 plugins/* 包在 init() 中调用）
func RegisterPluginTool(factory func() Tool) {
	if factory == nil {
		return
	}
	pluginToolMu.Lock()
	defer pluginToolMu.Unlock()
	pluginFactories = append(pluginFactories, factory)
}

// ListPluginTools 实例化并返回所有已挂载的插件工具
func ListPluginTools() []Tool {
	pluginToolMu.RLock()
	factories := append([]func() Tool(nil), pluginFactories...)
	pluginToolMu.RUnlock()

	out := make([]Tool, 0, len(factories))
	for _, f := range factories {
		if t := f(); t != nil {
			out = append(out, t)
		}
	}
	return out
}

// SetUIEventSender 由交互式 TUI 运行时注入消息派发器，供交互型插件工具向界面发送弹窗请求
func SetUIEventSender(fn func(msg any)) {
	uiSenderMu.Lock()
	defer uiSenderMu.Unlock()
	uiSender = fn
}

// SendUIEvent 向当前交互式 TUI 发送事件；非交互模式下返回 false
func SendUIEvent(msg any) bool {
	uiSenderMu.RLock()
	fn := uiSender
	uiSenderMu.RUnlock()
	if fn == nil {
		return false
	}
	fn(msg)
	return true
}
