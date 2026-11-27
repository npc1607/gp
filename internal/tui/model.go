package tui

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"gp/internal/ai"
	"gp/internal/core/auth"
	"gp/internal/core/compaction"
	"gp/internal/core/storage"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
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
	header          HeaderInfo
	config          UIConfig
	theme           Theme
	activeTabID     string
	tabIDs          []string
	agentBuffers    map[string]*AgentViewBuffer
	workflowTree    *WorkflowTreeModel
	tray            *ActiveTrayModel
	working         *WorkingLineState
	footer          *FooterState
	authStore       *auth.Store
	overlay         *OverlayState
	askUser             *AskUserDialogState
	inputArea           textinput.Model
	zoomArea            textarea.Model
	editorZoomed        bool
	editorSelectedAll   bool
	inputBus            *InputBus
	inputHistory        *InputHistory
	vim                 *VimState
	keymap              *KeymapRegistry
	selection           TextSelectionState
	mouseEnabled        bool
	pendingCmd          tea.Cmd
	primaryBusy         bool
	interruptedThisTurn bool
	width               int
	height              int
	ready               bool
	showDAG             bool
	tickCount           int
	compCursor          int
	onUserSubmit        func(prompt string)
	onCancel            func()
	onModelChange       func(entry auth.ModelEntry, persist bool) error
	onThinkingChange    func(level string) error
	onAuthChange        func(providerID string)
	onListSessions   func() ([]storage.SessionSummary, error)
	onResumeSession  func(sessionID string) ([]ai.Message, error)
	onCompact        func(focus string) (int64, int64, error)
	resourceHandlers ResourceHandlers
	fpsTicker        time.Duration
}

// NewMainUIModel 创建全局终端界面主模型
func NewMainUIModel(header HeaderInfo, onSubmit func(prompt string)) *MainUIModel {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = "Type a message, /model, /login, or Ctrl+O to toggle folds..."
	ti.CharLimit = 0
	ti.Focus()

	za := textarea.New()
	za.Prompt = ""
	za.Placeholder = "Multi-line editor (Alt+Enter send · Ctrl+X compact)..."
	za.CharLimit = 0
	za.ShowLineNumbers = false
	za.SetHeight(ZoomedEditorHeight - 3)

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
		zoomArea:     za,
		inputBus:     NewInputBus(),
		inputHistory: NewInputHistory(),
		vim:          NewVimState(),
		keymap:       NewDefaultKeymap(),
		mouseEnabled: true,
		width:        defaultWidth,
		height:       defaultHeight,
		onUserSubmit: onSubmit,
		fpsTicker:    33 * time.Millisecond,
	}

	m.footer.ContextWindow = compaction.ResolveContextWindow(header.ModelName)
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
	if m.editorZoomed {
		deductH = ZoomedEditorHeight
	}
	if m.config.FooterStyle != "hidden" {
		deductH++
	}
	if len(m.tabIDs) > 1 {
		deductH++
	}
	if compRows := m.completionMenuHeight(); compRows > 0 {
		deductH += compRows
	}
	if m.inputBus != nil && m.inputBus.Len() > 0 {
		deductH++
	}
	if m.askUser != nil && m.askUser.Active {
		if m.askUser.Collapsed {
			deductH += 1
		} else {
			deductH += 14
		}
	}
	bodyH := max(totalH-deductH, 5)
	bodyW := totalW
	if m.showDAG && len(m.workflowTree.nodes) > 0 {
		leftW := max(totalW*32/100, 20)
		bodyW = max(totalW-leftW-1, 20)
	}
	return bodyW, bodyH
}

var leakedMouseSeqRe = regexp.MustCompile(`^(?:\x1b)?\[?<\d+;\d+;\d+[Mm]$`)

// isLeakedMouseSeq 过滤终端高速滚轮/鼠标上报被拆包后泄漏为普通 KeyRunes 的 SGR 鼠标转义序列（如 [<65;71;14M）
func isLeakedMouseSeq(msg tea.KeyMsg) bool {
	if msg.Type != tea.KeyRunes || len(msg.Runes) < 6 {
		return false
	}
	return leakedMouseSeqRe.MatchString(string(msg.Runes))
}

func (m *MainUIModel) completionMenuHeight() int {
	if m.overlay != nil && m.overlay.Kind != OverlayNone {
		return 0
	}
	matches := MatchSlashCommands(m.inputArea.Value())
	if len(matches) == 0 {
		return 0
	}
	// +2 对应 RenderCompletionMenu 外框上下边框行，确保与编辑器及底部状态栏保持清晰间距
	if len(matches) > 6 {
		return 6 + 2
	}
	return len(matches) + 2
}

func (m *MainUIModel) resizeAllBuffers() {
	bodyW, bodyH := m.computeBodySize(m.width, m.height)
	m.zoomArea.SetWidth(max(m.width-4, 12))
	m.zoomArea.SetHeight(ZoomedEditorHeight - 3)
	m.inputArea.Width = m.width - 4
	for _, buf := range m.agentBuffers {
		buf.Resize(bodyW, bodyH)
		if buf.Dirty {
			buf.SyncViewport(m.theme, m.config, m.transcriptPulseGlyph(), false)
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

// SetCancelHandler 注册按下 Esc 中断当前主 Agent 推理的回调
func (m *MainUIModel) SetCancelHandler(fn func()) {
	m.onCancel = fn
}

// InputBus 返回输入指令总线队列实例
func (m *MainUIModel) InputBus() *InputBus {
	return m.inputBus
}

// Keymap 返回快捷键注册中心，支持外部查询、绑定或重绑定快捷键
func (m *MainUIModel) Keymap() *KeymapRegistry {
	return m.keymap
}

// SetCompactHandler 注册 /compact 上下文压缩回调
func (m *MainUIModel) SetCompactHandler(fn func(focus string) (int64, int64, error)) {
	m.onCompact = fn
}

// SetThinkingChangeHandler 注册思考深度变更回调
func (m *MainUIModel) SetThinkingChangeHandler(fn func(level string) error) {
	m.onThinkingChange = fn
}

// Footer 返回底部状态栏实例，供外部模块或插件访问
func (m *MainUIModel) Footer() *FooterState {
	return m.footer
}

// RegisterStatusSegment 向底部 Statusline 注册一个自定义指标段
func (m *MainUIModel) RegisterStatusSegment(seg StatusSegment) {
	if m.footer != nil {
		m.footer.RegisterSegment(seg)
	}
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

// SetModelChangeHandler 注册 /models 切换模型与 Ctrl+S 保存默认模型时的运行时回调
func (m *MainUIModel) SetModelChangeHandler(fn func(entry auth.ModelEntry, persist bool) error) {
	m.onModelChange = fn
}

// SetAuthChangeHandler 注册 /login 或 /logout 变更凭证时的回调
func (m *MainUIModel) SetAuthChangeHandler(fn func(providerID string)) {
	m.onAuthChange = fn
}

// SetSessionHandlers 注册 /resume 列举与恢复历史会话的运行时回调
func (m *MainUIModel) SetSessionHandlers(
	listFn func() ([]storage.SessionSummary, error),
	resumeFn func(sessionID string) ([]ai.Message, error),
) {
	m.onListSessions = listFn
	m.onResumeSession = resumeFn
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

// transcriptPulseGlyph 返回用于 Transcript 内部步骤的克制呼吸指示点（与底部 WorkingLine 视觉去重）
func (m *MainUIModel) transcriptPulseGlyph() string {
	if m.working == nil {
		return ""
	}
	return m.working.BreathGlyph(m.theme)
}

func (m *MainUIModel) syncActiveBuffer(autoScroll bool) {
	if buf, ok := m.agentBuffers[m.activeTabID]; ok {
		buf.SyncViewport(m.theme, m.config, m.transcriptPulseGlyph(), autoScroll)
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
		if msg.Button == tea.MouseButtonWheelUp {
			m.selection.Clear()
			activeBuf.Scroll(-3)
			return m, nil
		}
		if msg.Button == tea.MouseButtonWheelDown {
			m.selection.Clear()
			activeBuf.Scroll(3)
			return m, nil
		}
		if m.selection.Active && msg.Action == tea.MouseActionMotion {
			m.selection.Update(msg.X, msg.Y)
			return m, nil
		}
		if m.selection.Active && msg.Action == tea.MouseActionRelease {
			m.selection.Update(msg.X, msg.Y)
			if m.selection.HasRange {
				frame := m.renderView(false)
				copied := ExtractSelectedText(frame, m.selection)
				m.selection.Clear()
				if copied != "" {
					if err := CopyTextToClipboard(copied); err != nil {
						m.selection.SetToast("✗ Copy failed", 2500*time.Millisecond)
					} else {
						m.selection.SetToast(fmt.Sprintf("✓ Copied %d chars", len([]rune(copied))), 2500*time.Millisecond)
					}
				}
				return m, nil
			}
			// 纯单击（未拖动）：执行折叠/展开点击命中
			m.selection.Clear()
			headerOffset := 0
			if len(m.tabIDs) > 1 {
				headerOffset = 1
			}
			_, bodyH := m.computeBodySize(m.width, m.height)
			relY := msg.Y - headerOffset
			if relY >= 0 && relY < bodyH {
				absLine := activeBuf.Viewport.YOffset + relY
				if activeBuf.Transcript.HandleClick(absLine) {
					activeBuf.SyncViewport(m.theme, m.config, m.transcriptPulseGlyph(), false)
				}
			}
			return m, nil
		}
		if msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress {
			m.selection.Start(msg.X, msg.Y)
			return m, nil
		}
		return m, nil

	case tea.KeyMsg:
		if isLeakedMouseSeq(msg) {
			return m, nil
		}
		if m.HandleAskUserKey(msg) {
			return m, nil
		}
		if m.HandleOverlayKey(msg) {
			return m, nil
		}
		var matches []SlashCommand
		if !m.editorZoomed {
			matches = MatchSlashCommands(m.inputArea.Value())
		}
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
		if m.editorSelectedAll {
			switch msg.String() {
			case "backspace", "delete", "ctrl+w", "ctrl+u":
				m.setActiveInputText("")
				m.editorSelectedAll = false
				return m, nil
			case "ctrl+a":
				// keep selected
			default:
				m.editorSelectedAll = false
				if msg.Type == tea.KeyRunes && len(msg.Runes) > 0 {
					m.setActiveInputText("")
				}
			}
		}
		if cmd, handled := m.HandleVimKey(msg); handled {
			return m, cmd
		}
		if m.keymap != nil {
			if cmd, handled := m.keymap.Dispatch(msg.String(), m); handled {
				return m, cmd
			}
		}

		var cmd tea.Cmd
		if m.editorZoomed {
			m.zoomArea, cmd = m.zoomArea.Update(msg)
			return m, cmd
		}
		prevCompH := m.completionMenuHeight()
		m.inputArea, cmd = m.inputArea.Update(msg)
		if m.completionMenuHeight() != prevCompH {
			m.compCursor = 0
			m.resizeAllBuffers()
		}
		return m, cmd

	case TickMsg:
		m.tickCount++
		if m.working.Active {
			m.working.TickFrame()
			if m.tickCount%3 == 0 {
				m.syncActiveBuffer(false)
			} else if buf, ok := m.agentBuffers[m.activeTabID]; ok && buf.Dirty {
				autoScroll := buf.AutoScroll
				buf.AutoScroll = false
				buf.SyncViewport(m.theme, m.config, m.transcriptPulseGlyph(), autoScroll)
			}
		} else if buf, ok := m.agentBuffers[m.activeTabID]; ok && buf.Dirty {
			autoScroll := buf.AutoScroll
			buf.AutoScroll = false
			buf.SyncViewport(m.theme, m.config, m.transcriptPulseGlyph(), autoScroll)
		}
		if m.tickCount%90 == 0 && m.footer != nil {
			m.footer.RefreshGit()
		}
		if m.pendingCmd != nil {
			pc := m.pendingCmd
			m.pendingCmd = nil
			return m, tea.Batch(tickEvery(m.fpsTicker), pc)
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
			if (specMsg.AgentID == "" || specMsg.AgentID == PrimaryAgentID) && m.working.Active {
				m.working.Phase = "writing"
				m.working.TaskDesc = "Writing…"
				m.working.ToolName = ""
			}
			if buf.LastSync.IsZero() || time.Since(buf.LastSync) >= 33*time.Millisecond {
				buf.SyncViewport(m.theme, m.config, m.transcriptPulseGlyph(), true)
				buf.LastSync = time.Now()
			} else {
				buf.Dirty = true
				buf.AutoScroll = true
			}
			return m, tea.Batch(cmds...)

		case AgentThinkingDeltaMsg:
			buf := m.EnsureAgentBuffer(specMsg.AgentID, "")
			buf.Transcript.AppendThinkingDelta(specMsg.ThinkingDelta, m.config)
			if specMsg.AgentID == "" || specMsg.AgentID == PrimaryAgentID {
				m.working.Active = true
				m.working.Phase = "thinking"
				m.working.TaskDesc = "Thinking…"
				m.working.ToolName = ""
				if m.working.StartedAt.IsZero() {
					m.working.StartedAt = time.Now()
				}
			}
			buf.Dirty = true
			return m, tea.Batch(cmds...)

		case TurnStartMsg:
			buf := m.EnsureAgentBuffer(specMsg.AgentID, "")
			buf.Transcript.StartTurn(m.config)
			if specMsg.AgentID == "" || specMsg.AgentID == PrimaryAgentID {
				m.workflowTree.MarkStarted()
			}
			m.working.Active = true
			m.working.Phase = "connecting"
			m.working.TaskDesc = "Connecting…"
			m.working.ToolName = ""
			m.working.StartedAt = time.Now()
			buf.SyncViewport(m.theme, m.config, m.transcriptPulseGlyph(), true)
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
				m.working.Phase = ""
				m.working.TaskDesc = ""
				m.working.ToolName = ""
			}
			if m.footer != nil {
				m.footer.RefreshGit()
			}
			buf.SyncViewport(m.theme, m.config, m.transcriptPulseGlyph(), true)
			return m, tea.Batch(cmds...)

		case ToolCallStartMsg:
			buf := m.EnsureAgentBuffer(specMsg.AgentID, "")
			buf.Transcript.StartToolCall(specMsg.CallID, specMsg.ToolName, specMsg.ArgsRaw, m.config)
			m.working.Active = true
			m.working.Phase = "tool"
			m.working.TaskDesc = "Running…"
			m.working.ToolName = specMsg.ToolName
			buf.SyncViewport(m.theme, m.config, m.transcriptPulseGlyph(), true)
			return m, tea.Batch(cmds...)

		case ToolCallEndMsg:
			buf := m.EnsureAgentBuffer(specMsg.AgentID, "")
			buf.Transcript.FinishToolCall(specMsg.CallID, specMsg.ToolName, specMsg.ArgsRaw, specMsg.Result, specMsg.IsError, specMsg.Duration)
			m.working.ToolName = ""
			if m.working.Active {
				m.working.Phase = "thinking"
				m.working.TaskDesc = "Thinking…"
			}
			buf.SyncViewport(m.theme, m.config, m.transcriptPulseGlyph(), true)
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

		case ContextMetricsMsg:
			if m.footer != nil {
				m.footer.ApplyContextMetrics(specMsg)
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
			pBuf.SyncViewport(m.theme, m.config, m.transcriptPulseGlyph(), parentID == m.activeTabID)
			return m, tea.Batch(cmds...)

		case DAGNodeStatusMsg:
			m.workflowTree.AddOrUpdateNode(specMsg)
			return m, tea.Batch(cmds...)

		case ActiveCoroutineMsg:
			m.tray.UpdateCoroutine(specMsg)
			if specMsg.Active {
				m.working.Active = true
				if specMsg.AgentID == "" || specMsg.AgentID == PrimaryAgentID {
					m.primaryBusy = true
				}
				if specMsg.TaskDesc != "" {
					m.working.TaskDesc = specMsg.TaskDesc
				}
				if m.working.StartedAt.IsZero() {
					m.working.StartedAt = time.Now()
				}
			} else {
				if len(m.tray.order) == 0 {
					m.working.Active = false
				}
				if specMsg.AgentID == "" || specMsg.AgentID == PrimaryAgentID {
					m.primaryBusy = false
					wasInterrupted := m.interruptedThisTurn
					m.interruptedThisTurn = false
					if !wasInterrupted && m.inputBus != nil && m.inputBus.Len() > 0 {
						if squeezed, count, ok := m.inputBus.DequeueSqueezed(); ok {
							if count > 1 {
								m.addSystemNotice(fmt.Sprintf("⚡ Coalesced %d queued commands", count), false)
							}
							m.dispatchPrompt(squeezed.Text)
						}
					}
					m.resizeAllBuffers()
				}
			}
			return m, tea.Batch(cmds...)

		case UserSubmitPromptMsg:
			if specMsg.Prompt != "" {
				m.submitUserPrompt(specMsg.Prompt)
			}
			return m, tea.Batch(cmds...)

		case CompactionDoneMsg:
			m.primaryBusy = false
			m.working.Active = false
			if specMsg.Err != nil {
				m.addSystemNotice(fmt.Sprintf("Compact failed: %v", specMsg.Err), true)
			} else {
				if m.footer != nil {
					m.footer.RecordCompaction(specMsg.AfterTokens)
				}
				m.addSystemNotice(fmt.Sprintf("✓ Compacted context: %s → %s tokens",
					FormatTokenCompact(specMsg.BeforeTokens), FormatTokenCompact(specMsg.AfterTokens)), false)
			}
			return m, tea.Batch(cmds...)

		case ErrorMsg:
			if specMsg.Err != nil {
				m.workflowTree.MarkStopped()
				m.working.Active = false
				if activeBuf, ok := m.agentBuffers[m.activeTabID]; ok {
					activeBuf.RawTextBuffer += "\n[Error]: " + specMsg.Err.Error() + "\n"
					activeBuf.Transcript.AddError(specMsg.Err.Error())
					activeBuf.SyncViewport(m.theme, m.config, m.transcriptPulseGlyph(), true)
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
	case "/resume":
		if m.onListSessions == nil || m.onResumeSession == nil {
			m.addSystemNotice("Session storage is not enabled in this mode", true)
			return true
		}
		sessions, err := m.onListSessions()
		if err != nil {
			m.addSystemNotice(fmt.Sprintf("List sessions failed: %v", err), true)
			return true
		}
		query := ""
		if len(fields) >= 2 {
			query = strings.Join(fields[1:], " ")
			for _, s := range sessions {
				if s.SessionID == query {
					m.applyResumeSession(s.SessionID)
					return true
				}
			}
		}
		m.overlay.Kind = OverlayResumeSelect
		m.overlay.Cursor = 0
		m.overlay.SearchQuery = query
		m.overlay.Sessions = sessions
		return true

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

	case "/models":
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
			// 若精确匹配某一个模型，直接切换
			var exactMatches []auth.ModelEntry
			qNorm := strings.ToLower(strings.TrimSpace(query))
			for _, me := range models {
				if strings.ToLower(me.ID) == qNorm || strings.ToLower(me.FullName) == qNorm {
					exactMatches = append(exactMatches, me)
				}
			}
			if len(exactMatches) == 1 {
				m.applyModelSelection(exactMatches[0], false)
				return true
			}
		}
		m.overlay.Kind = OverlayModelSelect
		m.overlay.Cursor = 0
		m.overlay.SearchQuery = query
		m.overlay.Models = models
		m.overlay.ThinkingLevel = m.header.ThinkingLevel
		m.overlay.syncThinkingOptionsForCursor(m.header.ProviderName, m.header.ModelName)
		return true

	case "/clear":
		if buf, ok := m.agentBuffers[m.activeTabID]; ok {
			buf.RawTextBuffer = ""
			buf.Transcript = NewTranscript(m.header.ModelName, m.header.ProviderName)
			buf.SyncViewport(m.theme, m.config, m.transcriptPulseGlyph(), true)
		}
		if m.footer != nil {
			m.footer.ContextUsed = 0
		}
		return true

	case "/skills":
		return m.handleSkillsCommand(fields)

	case "/mcp":
		return m.handleMCPCommand(fields)

	case "/ext":
		return m.handleExtCommand(fields)

	case "/compact":
		if m.primaryBusy {
			m.addSystemNotice("Cannot compact while agent is running.", true)
			return true
		}
		if m.onCompact == nil {
			m.addSystemNotice("Context compaction is unavailable in current mode.", true)
			return true
		}
		focus := ""
		if len(fields) > 1 {
			focus = strings.Join(fields[1:], " ")
		}
		m.primaryBusy = true
		m.working.Active = true
		m.working.TaskDesc = "Compacting context…"
		m.working.StartedAt = time.Now()
		before, after, err := m.onCompact(focus)
		m.primaryBusy = false
		m.working.Active = false
		if err != nil {
			m.addSystemNotice(fmt.Sprintf("Compact failed: %v", err), true)
		} else {
			if m.footer != nil {
				m.footer.ContextUsed = after
			}
			m.addSystemNotice(fmt.Sprintf("✓ Compacted context: %s → %s tokens",
				FormatTokenCompact(before), FormatTokenCompact(after)), false)
		}
		return true

	case "/tidy":
		if len(fields) >= 2 && fields[1] == "turn" {
			if buf, ok := m.agentBuffers[m.activeTabID]; ok {
				for _, v := range slices.Backward(buf.Transcript.Items) {
					if v.Kind == "turn" {
						v.Turn.Folded = !v.Turn.Folded
						v.Turn.FoldManual = true
						buf.SyncViewport(m.theme, m.config, m.transcriptPulseGlyph(), false)
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

	case "/vim":
		if m.vim == nil {
			m.vim = NewVimState()
		}
		if len(fields) >= 2 {
			switch strings.ToLower(fields[1]) {
			case "on", "true", "1":
				m.vim.SetEnabled(true)
			case "off", "false", "0":
				m.vim.SetEnabled(false)
			default:
				m.vim.Toggle()
			}
		} else {
			m.vim.Toggle()
		}
		if m.vim.Enabled {
			m.addSystemNotice("✓ Vim mode enabled (Esc -> NORMAL, i/a -> INSERT)", false)
		} else {
			m.addSystemNotice("✓ Vim mode disabled", false)
		}
		return true

	case "/copy":
		buf, ok := m.agentBuffers[m.activeTabID]
		if !ok || buf == nil {
			return true
		}
		var text string
		if len(fields) >= 2 && strings.EqualFold(fields[1], "all") {
			text = strings.TrimSpace(ansi.Strip(buf.Transcript.Render(max(m.width, 80), m.theme, m.config, "")))
		} else {
			// 默认复制最近一条非空助手回复
			for i := len(buf.Transcript.Items) - 1; i >= 0; i-- {
				it := buf.Transcript.Items[i]
				if it.Kind != "turn" || it.Turn == nil {
					continue
				}
				if strings.TrimSpace(it.Turn.FinalText) != "" {
					text = strings.TrimSpace(it.Turn.FinalText)
					break
				}
				for j := len(it.Turn.Steps) - 1; j >= 0; j-- {
					if it.Turn.Steps[j].Kind == "text" && strings.TrimSpace(it.Turn.Steps[j].Text) != "" {
						text = strings.TrimSpace(it.Turn.Steps[j].Text)
						break
					}
				}
				if text != "" {
					break
				}
			}
		}
		if text == "" {
			m.addSystemNotice("No text available to copy.", true)
			return true
		}
		if err := CopyTextToClipboard(text); err != nil {
			m.addSystemNotice(fmt.Sprintf("Copy failed: %v", err), true)
			return true
		}
		m.selection.SetToast(fmt.Sprintf("✓ Copied %d chars", len([]rune(text))), 2500*time.Millisecond)
		m.addSystemNotice(fmt.Sprintf("✓ Copied %d chars to clipboard", len([]rune(text))), false)
		return true

	case "/mouse":
		enable := !m.mouseEnabled
		if len(fields) >= 2 {
			switch strings.ToLower(fields[1]) {
			case "on", "enable", "true":
				enable = true
			case "off", "disable", "false":
				enable = false
			}
		}
		m.mouseEnabled = enable
		if enable {
			m.pendingCmd = tea.EnableMouseCellMotion
			m.addSystemNotice("✓ TUI mouse enabled (drag to copy · click to fold · wheel to scroll)", false)
		} else {
			m.selection.Clear()
			m.pendingCmd = tea.DisableMouse
			m.addSystemNotice("✓ TUI mouse disabled (native terminal text selection active; run /mouse on to restore)", false)
		}
		return true

	case "/statusline":
		if m.footer == nil {
			return true
		}
		if len(fields) == 1 || fields[1] == "list" {
			m.overlay.Close()
			m.overlay.Kind = OverlayStatuslineSelect
			m.overlay.Segments = m.footer.ListSegments()
			return true
		}
		switch fields[1] {
		case "reset":
			m.footer.ResetSegments()
			m.addSystemNotice("✓ Statusline segments reset to defaults", false)
		case "toggle":
			if len(fields) < 3 {
				m.addSystemNotice("Usage: /statusline toggle <id>", true)
				return true
			}
			en, ok := m.footer.ToggleSegment(fields[2])
			if !ok {
				m.addSystemNotice(fmt.Sprintf("Unknown statusline segment: %s", fields[2]), true)
				return true
			}
			state := "off"
			if en {
				state = "on"
			}
			m.addSystemNotice(fmt.Sprintf("✓ Statusline segment %q -> %s", fields[2], state), false)
		case "on", "off":
			if len(fields) < 3 {
				m.addSystemNotice(fmt.Sprintf("Usage: /statusline %s <id>", fields[1]), true)
				return true
			}
			want := fields[1] == "on"
			if !m.footer.SetSegmentEnabled(fields[2], want) {
				m.addSystemNotice(fmt.Sprintf("Unknown statusline segment: %s", fields[2]), true)
				return true
			}
			m.addSystemNotice(fmt.Sprintf("✓ Statusline segment %q -> %s", fields[2], fields[1]), false)
		default:
			m.addSystemNotice("Usage: /statusline [list|toggle|on|off|reset] [id]", true)
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
		buf.SyncViewport(m.theme, m.config, m.transcriptPulseGlyph(), true)
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

func (m *MainUIModel) applyModelSelection(entry auth.ModelEntry, persist bool) {
	if m.onModelChange != nil {
		if err := m.onModelChange(entry, persist); err != nil {
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
	if m.footer != nil {
		cw := compaction.ResolveContextWindow(entry.FullName)
		if cw <= 0 {
			cw = compaction.ResolveContextWindow(entry.ID)
		}
		m.footer.ContextWindow = cw
	}
	if persist {
		m.addSystemNotice(fmt.Sprintf("✓ Default model saved: %s (used on next startup)", entry.FullName), false)
	} else {
		m.addSystemNotice(fmt.Sprintf("✓ Model: %s", entry.FullName), false)
	}
}

func (m *MainUIModel) applyThinkingSelection(level string) {
	if m.onThinkingChange != nil {
		if err := m.onThinkingChange(level); err != nil {
			m.addSystemNotice(fmt.Sprintf("Set thinking level failed: %v", err), true)
			return
		}
	}
	m.header.ThinkingLevel = level
}

// resetSubagentViews 清理上一轮对话产生的子 Agent Tab、DAG 拓扑树及托盘状态，恢复主视图显示
func (m *MainUIModel) resetSubagentViews() {
	hadSubagents := len(m.tabIDs) > 1 || m.showDAG || len(m.workflowTree.nodes) > 0
	m.activeTabID = PrimaryAgentID
	m.tabIDs = []string{PrimaryAgentID}
	for id := range m.agentBuffers {
		if id != PrimaryAgentID {
			delete(m.agentBuffers, id)
		}
	}
	m.showDAG = false
	m.workflowTree.Reset()
	m.tray.Reset()
	if hadSubagents {
		m.resizeAllBuffers()
	}
}

func (m *MainUIModel) applyResumeSession(sessionID string) {
	if m.onResumeSession == nil {
		return
	}
	msgs, err := m.onResumeSession(sessionID)
	if err != nil {
		m.addSystemNotice(fmt.Sprintf("Resume %s failed: %v", sessionID, err), true)
		return
	}
	m.header.SessionID = sessionID
	m.resetSubagentViews()

	buf := m.EnsureAgentBuffer(PrimaryAgentID, "Orchestrator")
	buf.RawTextBuffer = ""
	buf.Transcript = NewTranscript(m.header.ModelName, m.header.ProviderName)

	var activeTurn *TurnItem
	for _, msg := range msgs {
		switch msg.Role {
		case ai.RoleUser:
			if activeTurn != nil {
				buf.Transcript.FinishTurn("completed", m.config)
				activeTurn = nil
			}
			buf.Transcript.AddUserMessage(msg.Content, msg.Timestamp)
		case ai.RoleAssistant:
			if activeTurn == nil {
				activeTurn = buf.Transcript.StartTurn(m.config)
			}
			if msg.Thinking != "" {
				buf.Transcript.AppendThinkingDelta(msg.Thinking, m.config)
			}
			if msg.Content != "" {
				buf.Transcript.AppendTextDelta(msg.Content, m.config)
			}
			for _, tc := range msg.ToolCalls {
				buf.Transcript.StartToolCall(tc.ID, tc.Function.Name, string(tc.Function.Arguments), m.config)
			}
			if len(msg.ToolCalls) == 0 {
				buf.Transcript.FinishTurn("completed", m.config)
				activeTurn = nil
			}
		case ai.RoleTool:
			buf.Transcript.FinishToolCall(msg.ToolCallID, "", "", msg.Content, false, 0)
		}
	}
	if activeTurn != nil {
		buf.Transcript.FinishTurn("completed", m.config)
	}
	if m.footer != nil {
		m.footer.ContextWindow = compaction.ResolveContextWindow(m.header.ModelName)
		m.footer.ContextUsed = compaction.EstimateMessagesTokens(msgs)
	}
	buf.Transcript.AddNotice(fmt.Sprintf("✓ Resumed conversation %s (%d messages)", sessionID, len(msgs)))
	buf.SyncViewport(m.theme, m.config, m.transcriptPulseGlyph(), true)
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
	return m.renderView(true)
}

func (m *MainUIModel) renderView(applySelection bool) string {
	width := max(m.width, 20)
	height := max(m.height, 12)

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

	// 输入指令总线待处理队列提示条
	if m.inputBus != nil {
		if snap := m.inputBus.Snapshot(); len(snap) > 0 {
			elements = append(elements, m.renderQueueBar(snap, width))
		}
	}

	// pi-zentui Opencode Editor (内嵌 Working Line)
	foldKey := "ctrl+o"
	dagKey := "ctrl+g"
	stopKey := "esc"
	if m.keymap != nil {
		foldKey = m.keymap.HintLabel("toggle_folds", foldKey)
		dagKey = m.keymap.HintLabel("toggle_dag", dagKey)
		stopKey = strings.ToLower(m.keymap.HintLabel("interrupt_or_rollback", stopKey))
	}
	rightHint := "ctrl+x zoom · " + foldKey + " fold"
	if toast := m.selection.ActiveToast(); toast != "" {
		rightHint = toast + " · " + rightHint
	}
	if m.editorSelectedAll {
		rightHint = "Backspace/Del clear · Type replace · Esc cancel"
	} else {
		if m.primaryBusy {
			rightHint = stopKey + " stop · " + rightHint
		}
		if len(m.workflowTree.nodes) > 0 {
			rightHint = dagKey + " dag · " + rightHint
		}
	}
	vimBadge := ""
	if m.vim != nil && m.vim.Enabled {
		vimBadge = string(m.vim.Mode)
	}
	meta := EditorMeta{
		ModelName:     m.header.ModelName,
		ProviderName:  m.header.ProviderName,
		ThinkingLevel: m.header.ThinkingLevel,
		VimBadge:      vimBadge,
		SelectedAll:   m.editorSelectedAll,
		RightHint:     rightHint,
	}
	var editorStr string
	if m.editorZoomed {
		editorStr = RenderZoomedEditor(m.zoomArea, width, m.theme, m.config, meta, m.working)
	} else {
		editorStr = RenderEditor(m.inputArea, width, m.theme, m.config, meta, m.working)
	}
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

	out := lipgloss.JoinVertical(lipgloss.Left, elements...)
	if applySelection && m.selection.Active && m.selection.HasRange {
		return ApplySelectionHighlight(out, m.selection, m.theme)
	}
	return out
}
