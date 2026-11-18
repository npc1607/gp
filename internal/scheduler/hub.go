package scheduler

// CoordHub 聚合多 Agent 相互通信的三大拓扑引擎：
// A. 共享黑板 (Blackboard)
// B. 点对点 A2A 信箱 (MailboxRouter)
// C. 事件广播总线 (EventBus)
type CoordHub struct {
	Blackboard *Blackboard
	Mailbox    *MailboxRouter
	EventBus   *EventBus
}

// NewCoordHub 创建包含三大通信拓扑的协同中枢
func NewCoordHub() *CoordHub {
	return &CoordHub{
		Blackboard: NewBlackboard(),
		Mailbox:    NewMailboxRouter(),
		EventBus:   NewEventBus(128),
	}
}
