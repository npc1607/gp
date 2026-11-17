package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"gp/internal/core/auth"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// PrimaryAgentID 主协调 Agent 的默认标识
const PrimaryAgentID = "root"

// HeaderInfo 包含终端会话基础元数据（Model、Provider、SessionID、Cwd 等）
type HeaderInfo struct {
	AppName       string
	Version       string
	ModelName     string
	ProviderName  string
	ThinkingLevel string
	SessionID     string
	TokenUsage    string
	Cwd           string
	BaseURL       string
	APIKey        string
}

// MainUIModel 全局终端界面状态模型（融合 pi-tidy + pi-zentui 视觉与交互）
type MainUIModel struct {
	header        HeaderInfo
	config        UIConfig
	theme         Theme
	activeTabID   string
	tabIDs        []string
	agentBuffers  map[string]*AgentViewBuffer
	workflowTree  *WorkflowTreeModel
	tray          *ActiveTrayModel
	working       *WorkingLineState
	footer        *FooterState
	authStore     *auth.Store
	overlay       *OverlayState
	askUser       *AskUserDialogState
	inputArea     textinput.Model
	width         int
	height        int
	ready         bool
	showDAG       bool
	tickCount     int
	compCursor    int
	onUserSubmit  func(prompt string)
	onModelChange func(entry auth.ModelEntry) error
	onAuthChange  func(providerID string)
	fpsTicker     time.Duration
}

// NewMainUIModel 创建全局终端界面主模型
func NewMainUIModel(header HeaderInfo, onSubmit func(prompt string)) *MainUIModel {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Focus()

	defaultWidth := 100
	defaultHeight := 30
	cfg := DefaultUIConfig()
	th := DefaultTheme()

	m := &MainUIModel{
		header:       header,
		config:       cfg,
		theme:        th,
		activeTabID:  PrimaryAgentID,
		tabIDs:       []string{PrimaryAgentID},
		agentBuffers: make(map[string]*AgentViewBuffer),
		workflowTree: NewWorkflowTreeModel(),
		tray:         NewActiveTrayModel(),
		working:      &WorkingLineState{},
		footer:       NewFooterState(header.Cwd),
		authStore:    auth.NewStore(""),
		overlay:      &OverlayState{},
		inputArea:    ti,
		width:        defaultWidth,
		height:       defaultHeight,
		onUserSubmit: onSubmit,
		fpsTicker:    33 * time.Millisecond,
	}

	bodyW, bodyH := m.computeBodySize(defaultWidth, defaultHeight)
	mainBuf := NewAgentViewBuffer(PrimaryAgentID, "Orchestrator", bodyW, bodyH)
	mainBuf.SetModelInfo(header.ModelName, header.ProviderName)
	m.agentBuffers[PrimaryAgentID] = mainBuf

	return m
}

func (m *MainUIModel) computeBodySize(totalW, totalH int) (int, int) {
	if totalW < 20 {
		totalW = 20
	}
	if totalH < 12 {
		totalH = 12
	}
	deductH := EditorHeight(m.config)
	if m.config.FooterStyle != "hidden" {
		deductH++
	}
	if len(m.tabIDs) > 1 {
		deductH++
	}
	if compRows := m.completionMenuHeight(); compRows > 0 {
		deductH += compRows
	}
	if m.askUser != nil && m.askUser.Active {
		if m.askUser.Collapsed {
			deductH += 1
		} else {
			deductH += 14
		}
	}
	bodyH := totalH - deductH
	if bodyH < 5 {
		bodyH = 5
	}
	bodyW := totalW
	if m.showDAG && len(m.workflowTree.nodes) > 0 {
		leftW := totalW * 32 / 100
		if leftW < 20 {
			leftW = 20
		}
		bodyW = totalW - leftW - 1
		if bodyW < 20 {
			bodyW = 20
		}
	}
	return bodyW, bodyH
}

func (m *MainUIModel) completionMenuHeight() int {
	if m.overlay != nil && m.overlay.Kind != OverlayNone {
		return 0
	}
	matches := MatchSlashCommands(m.inputArea.Value())
	if len(matches) == 0 {
		return 0
	}
	if len(matches) > 6 {
		return 6
	}
	return len(matches)
}

func (m *MainUIModel) resizeAllBuffers() {
	bodyW, bodyH := m.computeBodySize(m.width, m.height)
	m.inputArea.Width = m.width - 4
	for _, buf := range m.agentBuffers {
		buf.Resize(bodyW, bodyH)
		if buf.Dirty {
			buf.SyncViewport(m.theme, m.config, m.working.SpinnerGlyph(m.config.WorkingSpinner), false)
		}
	}
}

// EnsureAgentBuffer 确保指定 Agent 的缓冲区已初始化并在选项卡中登记
func (m *MainUIModel) EnsureAgentBuffer(agentID, roleName string) *AgentViewBuffer {
	if agentID == "" {
		agentID = PrimaryAgentID
	}
	buf, exists := m.agentBuffers[agentID]
	if !exists {
		bodyW, bodyH := m.computeBodySize(m.width, m.height)
		buf = NewAgentViewBuffer(agentID, roleName, bodyW, bodyH)
		buf.SetModelInfo(m.header.ModelName, m.header.ProviderName)
		m.agentBuffers[agentID] = buf
		m.tabIDs = append(m.tabIDs, agentID)
		m.resizeAllBuffers()
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

// Config 返回当前 UI 配置
func (m *MainUIModel) Config() UIConfig {
	return m.config
}

// SetAuthStore 注入自定义 auth.json 凭证存储
func (m *MainUIModel) SetAuthStore(s *auth.Store) {
	if s != nil {
		m.authStore = s
	}
}

// SetModelChangeHandler 注册 /model 切换模型时的运行时回调
func (m *MainUIModel) SetModelChangeHandler(fn func(entry auth.ModelEntry) error) {
	m.onModelChange = fn
}

// SetAuthChangeHandler 注册 /login 或 /logout 变更凭证时的回调
func (m *MainUIModel) SetAuthChangeHandler(fn func(providerID string)) {
	m.onAuthChange = fn
}

func (m *MainUIModel) activeTabIndex() int {
	for i, id := range m.tabIDs {
		if id == m.activeTabID {
			return i
		}
	}
	return 0
}

func (m *MainUIModel) cycleNextTab() {
	if len(m.tabIDs) <= 1 {
		return
	}
	idx := m.activeTabIndex()
	m.activeTabID = m.tabIDs[(idx+1)%len(m.tabIDs)]
	m.syncActiveBuffer(true)
}

func (m *MainUIModel) cyclePrevTab() {
	if len(m.tabIDs) <= 1 {
		return
	}
	idx := m.activeTabIndex()
	m.activeTabID = m.tabIDs[(idx-1+len(m.tabIDs))%len(m.tabIDs)]
	m.syncActiveBuffer(true)
}

func (m *MainUIModel) syncActiveBuffer(autoScroll bool) {
	if buf, ok := m.agentBuffers[m.activeTabID]; ok {
		buf.SyncViewport(m.theme, m.config, m.working.SpinnerGlyph(m.config.WorkingSpinner), autoScroll)
	}
}

// Init 启动备用屏幕与 30 FPS 刷新时钟
func (m *MainUIModel) Init() tea.Cmd {
	return tea.Batch(
		tea.EnterAltScreen,
		tickEvery(m.fpsTicker),
		m.tray.spinner.Tick,
		m.workflowTree.spinner.Tick,
	)
}

// Update 处理键盘、鼠标、定时器与 Agent 生命周期消息
func (m *MainUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		m.resizeAllBuffers()
		return m, nil

	case tea.MouseMsg:
		activeBuf, ok := m.agentBuffers[m.activeTabID]
		if !ok {
			return m, nil
		}
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			activeBuf.Scroll(-3)
			return m, nil
		case tea.MouseButtonWheelDown:
			activeBuf.Scroll(3)
			return m, nil
		case tea.MouseButtonLeft:
			if msg.Action == tea.MouseActionPress {
				headerOffset := 0
				if len(m.tabIDs) > 1 {
					headerOffset = 1
				}
				_, bodyH := m.computeBodySize(m.width, m.height)
				relY := msg.Y - headerOffset
				if relY >= 0 && relY < bodyH {
					absLine := activeBuf.Viewport.YOffset + relY
					if activeBuf.Transcript.HandleClick(absLine) {
						activeBuf.SyncViewport(m.theme, m.config, m.working.SpinnerGlyph(m.config.WorkingSpinner), false)
					}
				}
			}
			return m, nil
		}
		return m, nil

	case tea.KeyMsg:
		if m.HandleAskUserKey(msg) {
			return m, nil
		}
		if m.HandleOverlayKey(msg) {
			return m, nil
		}
		matches := MatchSlashCommands(m.inputArea.Value())
		if len(matches) > 0 {
			if m.compCursor >= len(matches) {
				m.compCursor = len(matches) - 1
			}
			switch msg.String() {
			case "up":
				if m.compCursor > 0 {
					m.compCursor--
				}
				return m, nil
			case "down":
				if m.compCursor < len(matches)-1 {
					m.compCursor++
				}
				return m, nil
			case "tab":
				chosen := matches[m.compCursor]
				m.inputArea.SetValue(chosen.Name + " ")
				m.inputArea.CursorEnd()
				m.compCursor = 0
				m.resizeAllBuffers()
				return m, nil
			case "esc":
				m.inputArea.Reset()
				m.compCursor = 0
				m.resizeAllBuffers()
				return m, nil
			case "enter":
				// 若输入为不完整前缀（如 /mo），自动补全并执行选中指令
				val := strings.TrimSpace(m.inputArea.Value())
				exact := false
				for _, cm := range matches {
					if cm.Name == val {
						exact = true
						break
					}
				}
				if !exact {
					chosen := matches[m.compCursor]
					m.inputArea.Reset()
					m.compCursor = 0
					m.resizeAllBuffers()
					m.handleLocalCommand(chosen.Name)
					return m, nil
				}
			}
		}
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "ctrl+o":
			if activeBuf, ok := m.agentBuffers[m.activeTabID]; ok {
				activeBuf.Transcript.ToggleAllExpanded()
				activeBuf.SyncViewport(m.theme, m.config, m.working.SpinnerGlyph(m.config.WorkingSpinner), false)
			}
			return m, nil
		case "ctrl+t":
			if activeBuf, ok := m.agentBuffers[m.activeTabID]; ok {
				activeBuf.Transcript.ToggleLatestThinking()
				activeBuf.SyncViewport(m.theme, m.config, m.working.SpinnerGlyph(m.config.WorkingSpinner), false)
			}
			return m, nil
		case "ctrl+g":
			if len(m.workflowTree.nodes) > 0 {
				m.showDAG = !m.showDAG
				m.resizeAllBuffers()
			}
			return m, nil
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
				if m.handleLocalCommand(val) {
					return m, nil
				}
				if activeBuf, ok := m.agentBuffers[m.activeTabID]; ok {
					activeBuf.RawTextBuffer += "\n[User]: " + val + "\n\n"
					activeBuf.Transcript.AddUserMessage(val, time.Now())
					activeBuf.SyncViewport(m.theme, m.config, m.working.SpinnerGlyph(m.config.WorkingSpinner), true)
				}
				if m.onUserSubmit != nil {
					m.onUserSubmit(val)
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
		prevCompH := m.completionMenuHeight()
		m.inputArea, cmd = m.inputArea.Update(msg)
		if m.completionMenuHeight() != prevCompH {
			m.compCursor = 0
			m.resizeAllBuffers()
		}
		return m, cmd

	case TickMsg:
		m.tickCount++
		if m.working.Active && m.tickCount%3 == 0 {
			m.working.Advance()
			m.syncActiveBuffer(false)
		} else if buf, ok := m.agentBuffers[m.activeTabID]; ok && buf.Dirty {
			buf.SyncViewport(m.theme, m.config, m.working.SpinnerGlyph(m.config.WorkingSpinner), true)
		}
		if m.tickCount%90 == 0 && m.footer != nil {
			m.footer.RefreshGit()
		}
		return m, tickEvery(m.fpsTicker)

	case tea.Msg:
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

		switch specMsg := msg.(type) {
		case AgentStreamDeltaMsg:
			buf := m.EnsureAgentBuffer(specMsg.AgentID, "")
			buf.RawTextBuffer += specMsg.DeltaText
			buf.Transcript.AppendTextDelta(specMsg.DeltaText, m.config)
			buf.SyncViewport(m.theme, m.config, m.working.SpinnerGlyph(m.config.WorkingSpinner), true)
			return m, tea.Batch(cmds...)

		case AgentThinkingDeltaMsg:
			buf := m.EnsureAgentBuffer(specMsg.AgentID, "")
			buf.Transcript.AppendThinkingDelta(specMsg.ThinkingDelta, m.config)
			buf.Dirty = true
			return m, tea.Batch(cmds...)

		case TurnStartMsg:
			buf := m.EnsureAgentBuffer(specMsg.AgentID, "")
			buf.Transcript.StartTurn(m.config)
			if specMsg.AgentID == "" || specMsg.AgentID == PrimaryAgentID {
				m.workflowTree.MarkStarted()
			}
			m.working.Active = true
			m.working.TaskDesc = "Simmering…"
			m.working.ToolName = ""
			m.working.StartedAt = time.Now()
			buf.SyncViewport(m.theme, m.config, m.working.SpinnerGlyph(m.config.WorkingSpinner), true)
			return m, tea.Batch(cmds...)

		case TurnEndMsg:
			buf := m.EnsureAgentBuffer(specMsg.AgentID, "")
			outcome := specMsg.Outcome
			if outcome == "" {
				outcome = "completed"
			}
			buf.Transcript.FinishTurn(outcome, m.config)
			if specMsg.AgentID == "" || specMsg.AgentID == PrimaryAgentID {
				m.workflowTree.MarkStopped()
				m.working.Active = false
				m.working.ToolName = ""
			}
			if m.footer != nil {
				m.footer.RefreshGit()
			}
			buf.SyncViewport(m.theme, m.config, m.working.SpinnerGlyph(m.config.WorkingSpinner), true)
			return m, tea.Batch(cmds...)

		case ToolCallStartMsg:
			buf := m.EnsureAgentBuffer(specMsg.AgentID, "")
			buf.Transcript.StartToolCall(specMsg.CallID, specMsg.ToolName, specMsg.ArgsRaw, m.config)
			m.working.Active = true
			m.working.ToolName = specMsg.ToolName
			buf.SyncViewport(m.theme, m.config, m.working.SpinnerGlyph(m.config.WorkingSpinner), true)
			return m, tea.Batch(cmds...)

		case ToolCallEndMsg:
			buf := m.EnsureAgentBuffer(specMsg.AgentID, "")
			buf.Transcript.FinishToolCall(specMsg.CallID, specMsg.ToolName, specMsg.ArgsRaw, specMsg.Result, specMsg.IsError, specMsg.Duration)
			m.working.ToolName = ""
			buf.SyncViewport(m.theme, m.config, m.working.SpinnerGlyph(m.config.WorkingSpinner), true)
			return m, tea.Batch(cmds...)

		case TokenUsageMsg:
			if m.footer != nil {
				m.footer.InputTokens += specMsg.InputTokens
				m.footer.OutputTokens += specMsg.OutputTokens
				if specMsg.TotalTokens > 0 {
					m.footer.ContextUsed = specMsg.TotalTokens
				} else {
					m.footer.ContextUsed = m.footer.InputTokens + m.footer.OutputTokens
				}
			}
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

		case AgentFlowMsg:
			parentID := specMsg.ParentID
			if parentID == "" {
				parentID = PrimaryAgentID
			}
			pBuf := m.EnsureAgentBuffer(parentID, "")
			pBuf.Transcript.RecordAgentFlow(specMsg)

			cBuf := m.EnsureAgentBuffer(specMsg.ChildID, specMsg.Role)
			if specMsg.Direction == "dispatch" {
				cBuf.SetStatus("RUNNING", "")
				if specMsg.Payload != "" {
					cBuf.Transcript.AddUserMessage(specMsg.Payload, time.Now())
					cBuf.Dirty = true
				}
				m.workflowTree.AddOrUpdateNode(DAGNodeStatusMsg{
					NodeID: specMsg.ChildID,
					Role:   specMsg.Role,
					Status: "RUNNING",
				})
				m.tray.UpdateCoroutine(ActiveCoroutineMsg{
					AgentID:  specMsg.ChildID,
					Role:     specMsg.Role,
					TaskDesc: ClipToWidth(strings.Join(strings.Fields(specMsg.Payload), " "), 40, "…"),
					Active:   true,
				})
			} else {
				status := "DONE"
				errMsg := ""
				if specMsg.IsError {
					status = "FAILED"
					errMsg = specMsg.Payload
				}
				cBuf.SetStatus(status, "")
				m.workflowTree.AddOrUpdateNode(DAGNodeStatusMsg{
					NodeID:   specMsg.ChildID,
					Role:     specMsg.Role,
					Status:   status,
					Output:   specMsg.Payload,
					ErrorMsg: errMsg,
				})
				m.tray.UpdateCoroutine(ActiveCoroutineMsg{
					AgentID: specMsg.ChildID,
					Active:  false,
				})
			}
			if len(m.tray.order) > 0 {
				m.working.Active = true
				m.working.ToolName = fmt.Sprintf("%d subagents", len(m.tray.order))
			}
			pBuf.SyncViewport(m.theme, m.config, m.working.SpinnerGlyph(m.config.WorkingSpinner), parentID == m.activeTabID)
			return m, tea.Batch(cmds...)

		case DAGNodeStatusMsg:
			m.workflowTree.AddOrUpdateNode(specMsg)
			return m, tea.Batch(cmds...)

		case ActiveCoroutineMsg:
			m.tray.UpdateCoroutine(specMsg)
			if specMsg.Active {
				m.working.Active = true
				if specMsg.TaskDesc != "" {
					m.working.TaskDesc = specMsg.TaskDesc
				}
				if m.working.StartedAt.IsZero() {
					m.working.StartedAt = time.Now()
				}
			} else if len(m.tray.order) == 0 {
				m.working.Active = false
			}
			return m, tea.Batch(cmds...)

		case UserSubmitPromptMsg:
			if specMsg.Prompt != "" {
				if activeBuf, ok := m.agentBuffers[m.activeTabID]; ok {
					activeBuf.RawTextBuffer += "\n[User]: " + specMsg.Prompt + "\n\n"
					activeBuf.Transcript.AddUserMessage(specMsg.Prompt, time.Now())
					activeBuf.SyncViewport(m.theme, m.config, m.working.SpinnerGlyph(m.config.WorkingSpinner), true)
				}
				if m.onUserSubmit != nil {
					m.onUserSubmit(specMsg.Prompt)
				}
			}
			return m, tea.Batch(cmds...)

		case ErrorMsg:
			if specMsg.Err != nil {
				m.workflowTree.MarkStopped()
				m.working.Active = false
				if activeBuf, ok := m.agentBuffers[m.activeTabID]; ok {
					activeBuf.RawTextBuffer += "\n[Error]: " + specMsg.Err.Error() + "\n"
					activeBuf.Transcript.AddError(specMsg.Err.Error())
					activeBuf.SyncViewport(m.theme, m.config, m.working.SpinnerGlyph(m.config.WorkingSpinner), true)
				}
			}
			return m, tea.Batch(cmds...)

		case AskUserQuestionMsg:
			m.askUser = NewAskUserDialogState(specMsg)
			m.resizeAllBuffers()
			return m, tea.Batch(cmds...)
		}

		if len(cmds) > 0 {
			return m, tea.Batch(cmds...)
		}
	}

	return m, nil
}

// handleLocalCommand 处理 /login, /logout, /model, /zentui, /tidy, /clear 等内置指令
func (m *MainUIModel) handleLocalCommand(raw string) bool {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "/login":
		if len(fields) >= 3 {
			target := auth.NormalizeProviderID(fields[1])
			if spec, ok := auth.FindProviderSpec(target); ok {
				m.applyLogin(spec.ID, strings.Join(fields[2:], " "))
				return true
			}
		}
		if len(fields) >= 2 {
			target := auth.NormalizeProviderID(fields[1])
			if spec, ok := auth.FindProviderSpec(target); ok {
				m.overlay.Kind = OverlayLoginKey
				m.overlay.TargetProv = spec.ID
				m.overlay.InitKeyInput()
				m.overlay.StatusNote = ""
				return true
			}
		}
		m.overlay.Kind = OverlayLoginSelect
		m.overlay.Cursor = 0
		m.overlay.Providers = auth.RegisteredProviders()
		return true

	case "/logout":
		if len(fields) >= 2 {
			m.applyLogout(fields[1])
			return true
		}
		var ids []string
		if m.authStore != nil {
			if all, err := m.authStore.LoadAll(); err == nil {
				for k := range all {
					ids = append(ids, k)
				}
				sort.Strings(ids)
			}
		}
		m.overlay.Kind = OverlayLogoutSelect
		m.overlay.Cursor = 0
		m.overlay.LoggedInIDs = ids
		return true

	case "/model", "/models":
		if len(fields) == 2 && strings.ToLower(fields[1]) == "refresh" {
			go func() {
				for _, p := range auth.RegisteredProviders() {
					_, _ = m.authStore.RefreshProviderModels(p.ID)
				}
			}()
			m.addSystemNotice("Refreshing model catalog into SQLite in background...", false)
			return true
		}
		models := m.authStore.ListModels(m.header.ModelName, m.header.BaseURL, m.header.APIKey)
		query := ""
		if len(fields) >= 2 {
			query = strings.Join(fields[1:], " ")
			// 若精确匹配某一个模型，直接切换（对齐 Pi /model <exact>）
			var exactMatches []auth.ModelEntry
			qNorm := strings.ToLower(strings.TrimSpace(query))
			for _, me := range models {
				if strings.ToLower(me.ID) == qNorm || strings.ToLower(me.FullName) == qNorm {
					exactMatches = append(exactMatches, me)
				}
			}
			if len(exactMatches) == 1 {
				m.applyModelSelection(exactMatches[0])
				return true
			}
		}
		m.overlay.Kind = OverlayModelSelect
		m.overlay.Cursor = 0
		m.overlay.SearchQuery = query
		m.overlay.Models = models
		return true

	case "/clear":
		if buf, ok := m.agentBuffers[m.activeTabID]; ok {
			buf.RawTextBuffer = ""
			buf.Transcript = NewTranscript(m.header.ModelName, m.header.ProviderName)
			buf.SyncViewport(m.theme, m.config, m.working.SpinnerGlyph(m.config.WorkingSpinner), true)
		}
		return true

	case "/tidy":
		if len(fields) >= 2 && fields[1] == "turn" {
			if buf, ok := m.agentBuffers[m.activeTabID]; ok {
				for i := len(buf.Transcript.Items) - 1; i >= 0; i-- {
					if buf.Transcript.Items[i].Kind == "turn" {
						buf.Transcript.Items[i].Turn.Folded = !buf.Transcript.Items[i].Turn.Folded
						buf.Transcript.Items[i].Turn.FoldManual = true
						buf.SyncViewport(m.theme, m.config, m.working.SpinnerGlyph(m.config.WorkingSpinner), false)
						break
					}
				}
			}
		} else {
			if m.config.CollapseTurns == "afterRun" {
				m.config.CollapseTurns = "off"
			} else {
				m.config.CollapseTurns = "afterRun"
			}
			m.syncActiveBuffer(false)
		}
		return true

	case "/zentui":
		if len(fields) >= 3 && fields[1] == "preset" {
			switch fields[2] {
			case "opencode":
				m.config.EditorStyle = "opencode"
				m.config.UserMessageStyle = "frame"
				m.config.FooterStyle = "starship"
			case "rail":
				m.config.EditorStyle = "accent-rail"
				m.config.UserMessageStyle = "rail"
				m.config.FooterStyle = "starship"
			case "minimalist":
				m.config.EditorStyle = "minimalist"
				m.config.UserMessageStyle = "off"
				m.config.FooterStyle = "hidden"
			}
			m.resizeAllBuffers()
			m.syncActiveBuffer(false)
		}
		return true
	}
	return false
}

func (m *MainUIModel) addSystemNotice(msg string, isErr bool) {
	if buf, ok := m.agentBuffers[m.activeTabID]; ok {
		if isErr {
			buf.Transcript.AddError(msg)
		} else {
			buf.Transcript.AddNotice(msg)
		}
		buf.SyncViewport(m.theme, m.config, m.working.SpinnerGlyph(m.config.WorkingSpinner), true)
	}
}

func (m *MainUIModel) applyLogin(providerID, rawInput string) {
	if m.authStore == nil {
		m.authStore = auth.NewStore("")
	}
	parts := strings.Fields(strings.TrimSpace(rawInput))
	if len(parts) == 0 {
		return
	}
	token := parts[0]
	spec, _ := auth.FindProviderSpec(providerID)
	baseURL := spec.DefaultURL
	if len(parts) >= 2 && (strings.HasPrefix(parts[1], "http://") || strings.HasPrefix(parts[1], "https://")) {
		baseURL = strings.TrimRight(parts[1], "/")
	} else if auth.NormalizeProviderID(providerID) == "qoder" && strings.HasPrefix(token, "sk-sp-") {
		baseURL = "https://coding.dashscope.aliyuncs.com/v1"
	}
	cred := auth.Credential{
		Type:    "api_key",
		Key:     token,
		BaseURL: baseURL,
	}
	if err := m.authStore.Set(providerID, cred); err != nil {
		m.addSystemNotice(fmt.Sprintf("Login %s failed: %v", providerID, err), true)
		return
	}
	go func() {
		_, _ = m.authStore.RefreshProviderModels(providerID)
	}()
	if m.onAuthChange != nil {
		m.onAuthChange(providerID)
	}
	m.addSystemNotice(fmt.Sprintf("✓ Logged in to %s (endpoint: %s, saved to %s)", providerID, baseURL, m.authStore.Path()), false)
}

func (m *MainUIModel) applyLogout(providerID string) {
	if m.authStore == nil {
		return
	}
	removed, err := m.authStore.Remove(providerID)
	if err != nil {
		m.addSystemNotice(fmt.Sprintf("Logout %s failed: %v", providerID, err), true)
		return
	}
	if !removed {
		m.addSystemNotice(fmt.Sprintf("No stored credentials for %s", providerID), false)
		return
	}
	if m.onAuthChange != nil {
		m.onAuthChange(providerID)
	}
	m.addSystemNotice(fmt.Sprintf("✓ Logged out of %s", providerID), false)
}

func (m *MainUIModel) applyModelSelection(entry auth.ModelEntry) {
	if m.onModelChange != nil {
		if err := m.onModelChange(entry); err != nil {
			m.addSystemNotice(fmt.Sprintf("Switch model failed: %v", err), true)
			return
		}
	}
	m.header.ModelName = entry.ID
	if entry.Provider != "" {
		m.header.ProviderName = entry.Provider
	}
	if entry.BaseURL != "" {
		m.header.BaseURL = entry.BaseURL
	}
	for _, buf := range m.agentBuffers {
		buf.SetModelInfo(m.header.ModelName, m.header.ProviderName)
	}
	m.addSystemNotice(fmt.Sprintf("✓ Model: %s", entry.FullName), false)
}

func (m *MainUIModel) renderTabBar() string {
	var tabs []string
	sp := m.working.SpinnerGlyph(m.config.WorkingSpinner)
	if sp == "" {
		sp = "◐"
	}
	for i, id := range m.tabIDs {
		buf := m.agentBuffers[id]
		label := id
		statusGlyph := "○"
		statusColor := m.theme.Muted
		if buf != nil {
			if buf.RoleName != "" {
				label = fmt.Sprintf("%d:%s", i+1, buf.RoleName)
			}
			switch strings.ToUpper(buf.Status) {
			case "RUNNING":
				if id != PrimaryAgentID || m.working.Active {
					statusGlyph = sp
					statusColor = m.theme.Warning
				}
			case "DONE":
				statusGlyph = "✓"
				statusColor = m.theme.Success
			case "FAILED":
				statusGlyph = "✗"
				statusColor = m.theme.Error
			}
		}
		if id == m.activeTabID {
			tabs = append(tabs, m.theme.BoldFg(m.theme.Accent, "● "+label))
		} else {
			tabs = append(tabs, m.theme.Fg(statusColor, statusGlyph)+" "+m.theme.Fg(m.theme.Muted, label))
		}
	}
	hint := m.theme.Fg(m.theme.Dim, "  (Tab switch · Ctrl+G dag)")
	return ClipToWidth(" "+strings.Join(tabs, m.theme.Fg(m.theme.BorderMuted, " │ "))+hint, m.width, "…")
}

// View 渲染完整 pi-tidy + pi-zentui 界面
func (m *MainUIModel) View() string {
	width := m.width
	if width < 20 {
		width = 20
	}
	height := m.height
	if height < 12 {
		height = 12
	}

	var elements []string

	if len(m.tabIDs) > 1 {
		elements = append(elements, lipgloss.NewStyle().MaxWidth(width).MaxHeight(1).Render(m.renderTabBar()))
	}

	bodyW, bodyH := m.computeBodySize(width, height)
	var transcriptContent string
	if buf, ok := m.agentBuffers[m.activeTabID]; ok {
		transcriptContent = buf.Viewport.View()
	}

	if m.showDAG && len(m.workflowTree.nodes) > 0 {
		leftW := width - bodyW - 1
		leftContent := m.workflowTree.Render(leftW, bodyH)
		rightView := lipgloss.NewStyle().
			Width(bodyW).
			Height(bodyH).
			MaxHeight(bodyH).
			MaxWidth(bodyW).
			Render(transcriptContent)
		dividerLines := make([]string, bodyH)
		for i := range dividerLines {
			dividerLines[i] = "│"
		}
		divider := m.theme.Fg(m.theme.BorderMuted, strings.Join(dividerLines, "\n"))
		elements = append(elements, lipgloss.JoinHorizontal(lipgloss.Top, leftContent, divider, rightView))
	} else {
		mainView := lipgloss.NewStyle().
			Width(width).
			Height(bodyH).
			MaxHeight(bodyH).
			MaxWidth(width).
			Render(transcriptContent)
		elements = append(elements, mainView)
	}

	// rpiv-ask-user-question 交互式问卷弹层
	if m.askUser != nil && m.askUser.Active {
		if dlg := RenderAskUserDialog(m.askUser, width, m.theme); dlg != "" {
			elements = append(elements, dlg)
		}
	}

	// 交互式选择器弹层 (/model, /login, /logout)
	if m.overlay != nil && m.overlay.Kind != OverlayNone {
		if ov := RenderOverlay(m.overlay, width, m.header.ModelName, m.authStore, m.theme, m.config); ov != "" {
			elements = append(elements, ov)
		}
	}

	// pi-zentui Opencode Editor (内嵌 Working Line)
	rightHint := "ctrl+o fold · ctrl+t think"
	if len(m.workflowTree.nodes) > 0 {
		rightHint = "ctrl+g dag · " + rightHint
	}
	editorStr := RenderEditor(m.inputArea, width, m.theme, m.config, EditorMeta{
		ModelName:     m.header.ModelName,
		ProviderName:  m.header.ProviderName,
		ThinkingLevel: m.header.ThinkingLevel,
		RightHint:     rightHint,
	}, m.working)
	elements = append(elements, editorStr)

	// 斜杠指令补全提示菜单（紧贴编辑器下方，对齐 pi-zentui）
	if matches := MatchSlashCommands(m.inputArea.Value()); len(matches) > 0 && (m.overlay == nil || m.overlay.Kind == OverlayNone) {
		if comp := RenderCompletionMenu(matches, m.compCursor, width, m.theme); comp != "" {
			elements = append(elements, comp)
		}
	}

	// pi-zentui Starship Footer
	if m.config.FooterStyle != "hidden" && m.footer != nil {
		footerStr := m.footer.Render(width, m.theme, m.config)
		if footerStr != "" {
			elements = append(elements, footerStr)
		}
	}

	return lipgloss.JoinVertical(lipgloss.Left, elements...)
}
