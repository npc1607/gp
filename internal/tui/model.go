package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// HeaderInfo 包含终端顶部状态条所需的基础元数据
type HeaderInfo struct {
	AppName    string
	Version    string
	ModelName  string
	SessionID  string
	TokenUsage string
}

// MainUIModel 全局终端界面状态模型（符合 TEA 单向数据流架构）
type MainUIModel struct {
	header       HeaderInfo
	activeTabID  string                      // 当前处于焦点的前台 Agent ID ("main", "scout", "impl")
	tabIDs       []string                    // 选项卡顺序索引列表
	agentBuffers map[string]*AgentViewBuffer // 各子 Agent 虚拟缓冲池 (相互隔离)
	workflowTree *WorkflowTreeModel          // 左侧 DAG 树组件
	tray         *ActiveTrayModel            // 屏幕底端多任务动态泳道托盘
	inputArea    textinput.Model             // 底部用户输入框
	width        int
	height       int
	ready        bool
	onUserSubmit func(prompt string)
	fpsTicker    time.Duration // 30 FPS 防闪烁刷新时钟
}

// NewMainUIModel 创建全局终端界面主模型
func NewMainUIModel(header HeaderInfo, onSubmit func(prompt string)) *MainUIModel {
	ti := textinput.New()
	ti.Placeholder = "输入指令或提问，按 Enter 发送..."
	ti.Prompt = "> "
	ti.Focus()

	defaultWidth := 100
	defaultHeight := 30

	m := &MainUIModel{
		header:       header,
		activeTabID:  "main",
		tabIDs:       []string{"main"},
		agentBuffers: make(map[string]*AgentViewBuffer),
		workflowTree: NewWorkflowTreeModel(),
		tray:         NewActiveTrayModel(),
		inputArea:    ti,
		width:        defaultWidth,
		height:       defaultHeight,
		onUserSubmit: onSubmit,
		fpsTicker:    33 * time.Millisecond,
	}

	leftW := defaultWidth * 35 / 100
	if leftW < 20 {
		leftW = 20
	}
	rightW := defaultWidth - leftW - 1
	if rightW < 20 {
		rightW = 20
	}
	bodyH := defaultHeight - 10
	if bodyH < 5 {
		bodyH = 5
	}

	m.agentBuffers["main"] = NewAgentViewBuffer("main", "Orchestrator", rightW, bodyH)

	return m
}

// EnsureAgentBuffer 确保指定 Agent 的缓冲区已初始化并在选项卡中登记
func (m *MainUIModel) EnsureAgentBuffer(agentID, roleName string) *AgentViewBuffer {
	if agentID == "" {
		agentID = "main"
	}
	buf, exists := m.agentBuffers[agentID]
	if !exists {
		leftW := m.width * 35 / 100
		if leftW < 20 {
			leftW = 20
		}
		rightW := m.width - leftW - 1
		if rightW < 20 {
			rightW = 20
		}
		bodyH := m.height - 10
		if bodyH < 5 {
			bodyH = 5
		}

		buf = NewAgentViewBuffer(agentID, roleName, rightW, bodyH)
		m.agentBuffers[agentID] = buf
		m.tabIDs = append(m.tabIDs, agentID)
	} else if roleName != "" && buf.RoleName == "" {
		buf.RoleName = roleName
	}
	return buf
}

// ActiveTabID 返回当前处于聚焦状态的 Agent ID
func (m *MainUIModel) ActiveTabID() string {
	return m.activeTabID
}

// TabIDs 返回所有已登记的 Agent ID 列表
func (m *MainUIModel) TabIDs() []string {
	cp := make([]string, len(m.tabIDs))
	copy(cp, m.tabIDs)
	return cp
}

// AgentBuffer 获取指定 Agent 的缓冲区
func (m *MainUIModel) AgentBuffer(id string) *AgentViewBuffer {
	return m.agentBuffers[id]
}

// WorkflowTree 获取 DAG 拓扑树组件引用
func (m *MainUIModel) WorkflowTree() *WorkflowTreeModel {
	return m.workflowTree
}

// Tray 获取动态任务泳道组件引用
func (m *MainUIModel) Tray() *ActiveTrayModel {
	return m.tray
}

// InputArea 获取输入框模型引用
func (m *MainUIModel) InputArea() *textinput.Model {
	return &m.inputArea
}

// activeTabIndex 查找当前焦点选项卡的索引
func (m *MainUIModel) activeTabIndex() int {
	for i, id := range m.tabIDs {
		if id == m.activeTabID {
			return i
		}
	}
	return 0
}

// cycleNextTab 平滑循环切换聚焦的子 Agent 选项卡
func (m *MainUIModel) cycleNextTab() {
	if len(m.tabIDs) <= 1 {
		return
	}
	idx := m.activeTabIndex()
	nextIdx := (idx + 1) % len(m.tabIDs)
	m.activeTabID = m.tabIDs[nextIdx]
}

// cyclePrevTab 反向平滑循环切换聚焦的子 Agent 选项卡
func (m *MainUIModel) cyclePrevTab() {
	if len(m.tabIDs) <= 1 {
		return
	}
	idx := m.activeTabIndex()
	prevIdx := (idx - 1 + len(m.tabIDs)) % len(m.tabIDs)
	m.activeTabID = m.tabIDs[prevIdx]
}

// Init 启动全局双缓冲备用屏幕与 30 FPS 刷新时钟
func (m *MainUIModel) Init() tea.Cmd {
	return tea.Batch(
		tea.EnterAltScreen,
		tickEvery(m.fpsTicker),
		m.tray.spinner.Tick,
		m.workflowTree.spinner.Tick,
	)
}

// Update 处理来自用户按键与协程派发的消息
func (m *MainUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true

		leftW := m.width * 35 / 100
		if leftW < 20 {
			leftW = 20
		}
		rightW := m.width - leftW - 1
		if rightW < 20 {
			rightW = 20
		}
		bodyH := m.height - 10
		if bodyH < 5 {
			bodyH = 5
		}

		m.inputArea.Width = m.width - 4
		for _, buf := range m.agentBuffers {
			buf.Resize(rightW, bodyH)
		}
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "tab":
			m.cycleNextTab()
			return m, nil
		case "shift+tab":
			m.cyclePrevTab()
			return m, nil
		case "enter":
			val := strings.TrimSpace(m.inputArea.Value())
			if val != "" {
				m.inputArea.Reset()
				if m.onUserSubmit != nil {
					m.onUserSubmit(val)
				}
				if activeBuf, ok := m.agentBuffers[m.activeTabID]; ok {
					activeBuf.AppendDelta("\n[User]: " + val + "\n\n")
				}
			}
			return m, nil
		case "up":
			if activeBuf, ok := m.agentBuffers[m.activeTabID]; ok {
				activeBuf.Scroll(-1)
				return m, nil
			}
		case "down":
			if activeBuf, ok := m.agentBuffers[m.activeTabID]; ok {
				activeBuf.Scroll(1)
				return m, nil
			}
		case "pgup":
			if activeBuf, ok := m.agentBuffers[m.activeTabID]; ok {
				activeBuf.Scroll(-5)
				return m, nil
			}
		case "pgdown":
			if activeBuf, ok := m.agentBuffers[m.activeTabID]; ok {
				activeBuf.Scroll(5)
				return m, nil
			}
		}

		var cmd tea.Cmd
		m.inputArea, cmd = m.inputArea.Update(msg)
		return m, cmd

	case TickMsg:
		// 30 FPS 刷新时钟
		return m, tickEvery(m.fpsTicker)

	case tea.Msg:
		// 检查是否为 Spinner 帧事件
		var cmds []tea.Cmd
		var cmd tea.Cmd
		m.tray.spinner, cmd = m.tray.spinner.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.workflowTree.spinner, cmd = m.workflowTree.spinner.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}

		// 检查专有业务消息
		switch specMsg := msg.(type) {
		case AgentStreamDeltaMsg:
			buf := m.EnsureAgentBuffer(specMsg.AgentID, "")
			buf.AppendDelta(specMsg.DeltaText)
			return m, tea.Batch(cmds...)

		case AgentStatusChangedMsg:
			buf := m.EnsureAgentBuffer(specMsg.AgentID, specMsg.RoleName)
			buf.SetStatus(specMsg.NewStatus, specMsg.ToolName)
			m.workflowTree.AddOrUpdateNode(DAGNodeStatusMsg{
				NodeID: specMsg.AgentID,
				Role:   specMsg.RoleName,
				Status: specMsg.NewStatus,
			})
			return m, tea.Batch(cmds...)

		case DAGNodeStatusMsg:
			m.workflowTree.AddOrUpdateNode(specMsg)
			return m, tea.Batch(cmds...)

		case ActiveCoroutineMsg:
			m.tray.UpdateCoroutine(specMsg)
			return m, tea.Batch(cmds...)

		case UserSubmitPromptMsg:
			if specMsg.Prompt != "" {
				if m.onUserSubmit != nil {
					m.onUserSubmit(specMsg.Prompt)
				}
				if activeBuf, ok := m.agentBuffers[m.activeTabID]; ok {
					activeBuf.AppendDelta("\n[User]: " + specMsg.Prompt + "\n\n")
				}
			}
			return m, tea.Batch(cmds...)

		case ErrorMsg:
			if specMsg.Err != nil {
				if activeBuf, ok := m.agentBuffers[m.activeTabID]; ok {
					activeBuf.AppendDelta("\n[Error]: " + specMsg.Err.Error() + "\n")
				}
			}
			return m, tea.Batch(cmds...)
		}

		if len(cmds) > 0 {
			return m, tea.Batch(cmds...)
		}
	}

	return m, nil
}

// renderHeader 渲染顶部状态条
func (m *MainUIModel) renderHeader() string {
	appName := m.header.AppName
	if appName == "" {
		appName = "gp"
	}
	version := m.header.Version
	if version == "" {
		version = "v0.1.0-alpha"
	}
	modelName := m.header.ModelName
	if modelName == "" {
		modelName = "antigravity/gemini-4-argon"
	}
	sessionID := m.header.SessionID
	if sessionID == "" {
		sessionID = "sess_main"
	}
	tokenUsage := m.header.TokenUsage
	if tokenUsage == "" {
		tokenUsage = "0 tok"
	}

	headerText := fmt.Sprintf(" %s %s │ Model: %s │ Session: %s │ Tokens: %s",
		appName, version, modelName, sessionID, tokenUsage)

	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("15")).
		Background(lipgloss.Color("236")).
		Width(m.width)

	return headerStyle.Render(headerText)
}

// renderTabBar 渲染选项卡栏
func (m *MainUIModel) renderTabBar() string {
	var tabs []string
	for i, id := range m.tabIDs {
		var tabTitle string
		if buf, ok := m.agentBuffers[id]; ok && buf.RoleName != "" {
			tabTitle = fmt.Sprintf("[%d: %s (%s)]", i+1, buf.RoleName, id)
		} else {
			tabTitle = fmt.Sprintf("[%d: %s]", i+1, id)
		}

		if id == m.activeTabID {
			activeStyle := lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("231")).
				Background(lipgloss.Color("63")).
				Padding(0, 1)
			tabs = append(tabs, activeStyle.Render(tabTitle))
		} else {
			inactiveStyle := lipgloss.NewStyle().
				Foreground(lipgloss.Color("243")).
				Background(lipgloss.Color("237")).
				Padding(0, 1)
			tabs = append(tabs, inactiveStyle.Render(tabTitle))
		}
	}

	return lipgloss.JoinHorizontal(lipgloss.Top, tabs...)
}

// View 双缓冲批量渲染
func (m *MainUIModel) View() string {
	width := m.width
	if width < 40 {
		width = 40
	}
	height := m.height
	if height < 15 {
		height = 15
	}

	// 1. 渲染 Header
	header := m.renderHeader()

	// 2. 渲染 Tabs 栏
	tabs := m.renderTabBar()

	// 3. 计算分栏尺寸 (左侧 35%, 右侧 65%)
	leftW := width * 35 / 100
	if leftW < 20 {
		leftW = 20
	}
	rightW := width - leftW - 1
	if rightW < 20 {
		rightW = 20
	}
	bodyH := height - 10
	if bodyH < 5 {
		bodyH = 5
	}

	// 渲染左侧 DAG 树
	leftContent := m.workflowTree.Render(leftW, bodyH)

	// 渲染右侧活跃 Agent 缓冲视口
	var rightContent string
	if buf, ok := m.agentBuffers[m.activeTabID]; ok {
		rightContent = buf.Viewport.View()
	} else {
		rightContent = "(暂无内容 / Empty buffer)"
	}
	rightBoxStyle := lipgloss.NewStyle().
		Width(rightW).
		Height(bodyH).
		MaxHeight(bodyH).
		MaxWidth(rightW)
	rightView := rightBoxStyle.Render(rightContent)

	// 分割线
	dividerLines := make([]string, bodyH)
	for i := range dividerLines {
		dividerLines[i] = "│"
	}
	divider := lipgloss.NewStyle().Foreground(lipgloss.Color("238")).Render(strings.Join(dividerLines, "\n"))

	mainBody := lipgloss.JoinHorizontal(lipgloss.Top, leftContent, divider, rightView)

	// 4. 渲染底部动态任务泳道 (Active Coroutine Tray)
	trayDivider := lipgloss.NewStyle().Foreground(lipgloss.Color("238")).Render(strings.Repeat("─", width))
	tray := m.tray.Render(width)

	// 5. 渲染用户输入框与快捷键提示
	inputView := m.inputArea.View()
	shortcuts := lipgloss.NewStyle().
		Foreground(lipgloss.Color("241")).
		Render(" [Enter: 发送 | Tab/Shift+Tab: 切换 Agent | Ctrl+C: 退出 | ↑/↓: 滚动视口]")
	footer := lipgloss.JoinVertical(lipgloss.Left, inputView, shortcuts)

	return lipgloss.JoinVertical(lipgloss.Left, header, tabs, mainBody, trayDivider, tray, footer)
}
