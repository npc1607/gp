package tui

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gp/internal/ai"
	"gp/internal/core/auth"
	"gp/internal/core/storage"
	_ "gp/internal/ai/providers/openai"
	_ "gp/plugins/qoder"

	tea "github.com/charmbracelet/bubbletea"
)

// createTestMainUIModel 为单元测试创建标准 MainUIModel 实例
func createTestMainUIModel(t *testing.T, onSubmit func(prompt string)) *MainUIModel {
	t.Helper()
	header := HeaderInfo{
		AppName:    "gp",
		Version:    "v0.1.0-alpha",
		ModelName:  "antigravity/gemini-4-argon",
		SessionID:  "sess_test",
		TokenUsage: "128 tok",
	}
	m := NewMainUIModel(header, onSubmit)
	t.Cleanup(func() {
		// 清理与测试相关的任何临时模型状态
	})
	return m
}

// TC-TUI-001: 多 Agent 缓冲隔离
func TestTUI_TC_TUI_001_BufferIsolation(t *testing.T) {
	fn := "MainUIModel.AgentBufferIsolation"
	m := createTestMainUIModel(t, nil)

	inputs := struct {
		agentA string
		deltaA string
		agentB string
		deltaB string
	}{
		agentA: "agent-a",
		deltaA: "tokA",
		agentB: "agent-b",
		deltaB: "tokB",
	}

	_, _ = m.Update(AgentStreamDeltaMsg{AgentID: inputs.agentA, DeltaText: inputs.deltaA})
	_, _ = m.Update(AgentStreamDeltaMsg{AgentID: inputs.agentB, DeltaText: inputs.deltaB})

	bufA := m.AgentBuffer(inputs.agentA)
	bufB := m.AgentBuffer(inputs.agentB)

	if bufA == nil {
		t.Fatalf("%s(inputs=%+v): agentA buffer got nil, want non-nil", fn, inputs)
	}
	if bufB == nil {
		t.Fatalf("%s(inputs=%+v): agentB buffer got nil, want non-nil", fn, inputs)
	}

	// 验证 RawTextBuffer 隔离
	if !strings.Contains(bufA.RawTextBuffer, inputs.deltaA) {
		t.Fatalf("%s(inputs=%+v): bufA raw text got %q, want contains %q", fn, inputs, bufA.RawTextBuffer, inputs.deltaA)
	}
	if strings.Contains(bufA.RawTextBuffer, inputs.deltaB) {
		t.Fatalf("%s(inputs=%+v): bufA leaked agentB content, got %q, want not contains %q", fn, inputs, bufA.RawTextBuffer, inputs.deltaB)
	}

	if !strings.Contains(bufB.RawTextBuffer, inputs.deltaB) {
		t.Fatalf("%s(inputs=%+v): bufB raw text got %q, want contains %q", fn, inputs, bufB.RawTextBuffer, inputs.deltaB)
	}
	if strings.Contains(bufB.RawTextBuffer, inputs.deltaA) {
		t.Fatalf("%s(inputs=%+v): bufB leaked agentA content, got %q, want not contains %q", fn, inputs, bufB.RawTextBuffer, inputs.deltaA)
	}

	// 验证 Viewport 内容物理隔离
	viewA := bufA.Viewport.View()
	viewB := bufB.Viewport.View()
	if !strings.Contains(viewA, inputs.deltaA) {
		t.Fatalf("%s(inputs=%+v): bufA viewport got %q, want contains %q", fn, inputs, viewA, inputs.deltaA)
	}
	if strings.Contains(viewA, inputs.deltaB) {
		t.Fatalf("%s(inputs=%+v): bufA viewport leaked agentB content, got %q, want not contains %q", fn, inputs, viewA, inputs.deltaB)
	}
	if !strings.Contains(viewB, inputs.deltaB) {
		t.Fatalf("%s(inputs=%+v): bufB viewport got %q, want contains %q", fn, inputs, viewB, inputs.deltaB)
	}
	if strings.Contains(viewB, inputs.deltaA) {
		t.Fatalf("%s(inputs=%+v): bufB viewport leaked agentA content, got %q, want not contains %q", fn, inputs, viewB, inputs.deltaA)
	}
}

// TC-TUI-002: Tab 键环形焦点切换
func TestTUI_TC_TUI_002_TabCycling(t *testing.T) {
	fn := "MainUIModel.TabCycling"
	m := createTestMainUIModel(t, nil)

	m.EnsureAgentBuffer("scout", "ScoutRole")
	m.EnsureAgentBuffer("impl", "ImplRole")

	inputs := []struct {
		step   int
		keyMsg tea.KeyMsg
		want   string
	}{
		{step: 1, keyMsg: tea.KeyMsg{Type: tea.KeyTab}, want: "scout"},
		{step: 2, keyMsg: tea.KeyMsg{Type: tea.KeyTab}, want: "impl"},
		{step: 3, keyMsg: tea.KeyMsg{Type: tea.KeyTab}, want: PrimaryAgentID},
	}

	for _, step := range inputs {
		updated, _ := m.Update(step.keyMsg)
		m = updated.(*MainUIModel)
		got := m.ActiveTabID()
		if got != step.want {
			t.Fatalf("%s(step=%d, inputs=%+v): got activeTabID %q, want %q", fn, step.step, step.keyMsg, got, step.want)
		}
	}
}

// TC-TUI-003: Shift+Tab 逆向切焦点
func TestTUI_TC_TUI_003_ShiftTabCycling(t *testing.T) {
	fn := "MainUIModel.ShiftTabCycling"
	m := createTestMainUIModel(t, nil)

	m.EnsureAgentBuffer("scout", "ScoutRole")
	m.EnsureAgentBuffer("impl", "ImplRole")

	input := tea.KeyMsg{Type: tea.KeyShiftTab}
	want := "impl"

	updated, _ := m.Update(input)
	m = updated.(*MainUIModel)
	got := m.ActiveTabID()
	if got != want {
		t.Fatalf("%s(inputs=%+v): got activeTabID %q, want %q", fn, input, got, want)
	}
}

// TC-TUI-004: 单Tab与空Tab边界切换
func TestTUI_TC_TUI_004_SingleOrEmptyTabBoundary(t *testing.T) {
	fn := "MainUIModel.SingleOrEmptyTabBoundary"

	tests := []struct {
		name       string
		setupModel func(m *MainUIModel)
		inputKey   tea.KeyMsg
		wantTab    string
	}{
		{
			name: "single_tab_next",
			setupModel: func(m *MainUIModel) {
				// 仅有默认的 PrimaryAgentID Tab
			},
			inputKey: tea.KeyMsg{Type: tea.KeyTab},
			wantTab:  PrimaryAgentID,
		},
		{
			name: "single_tab_prev",
			setupModel: func(m *MainUIModel) {
				// 仅有默认的 PrimaryAgentID Tab
			},
			inputKey: tea.KeyMsg{Type: tea.KeyShiftTab},
			wantTab:  PrimaryAgentID,
		},
		{
			name: "empty_tab_next",
			setupModel: func(m *MainUIModel) {
				m.tabIDs = []string{}
				m.activeTabID = ""
			},
			inputKey: tea.KeyMsg{Type: tea.KeyTab},
			wantTab:  "",
		},
		{
			name: "empty_tab_prev",
			setupModel: func(m *MainUIModel) {
				m.tabIDs = []string{}
				m.activeTabID = ""
			},
			inputKey: tea.KeyMsg{Type: tea.KeyShiftTab},
			wantTab:  "",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			m := createTestMainUIModel(t, nil)
			tc.setupModel(m)

			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s(inputs=%+v): unexpected panic: %v, want graceful handling", fn, tc.name, r)
				}
			}()

			updated, _ := m.Update(tc.inputKey)
			res := updated.(*MainUIModel)
			got := res.ActiveTabID()
			if got != tc.wantTab {
				t.Fatalf("%s(inputs=%+v): activeTabID got %q, want %q", fn, tc.name, got, tc.wantTab)
			}
		})
	}
}

// TC-TUI-005: 30 FPS 刷新时钟调度
func TestTUI_TC_TUI_005_FPSSafety(t *testing.T) {
	fn := "MainUIModel.FPSSafety"
	m := createTestMainUIModel(t, nil)

	initCmd := m.Init()
	if initCmd == nil {
		t.Fatalf("%s(Init): got nil tea.Cmd, want non-nil batch command", fn)
	}

	tickInput := TickMsg(time.Now())
	updated, nextCmd := m.Update(tickInput)
	if updated == nil {
		t.Fatalf("%s(inputs=%+v): got nil model, want non-nil", fn, tickInput)
	}
	if nextCmd == nil {
		t.Fatalf("%s(inputs=%+v): got nil next tick cmd, want non-nil tea.Cmd", fn, tickInput)
	}
}

// TC-TUI-006: 动态任务泳道增删
func TestTUI_TC_TUI_006_ActiveTrayAddRemove(t *testing.T) {
	fn := "ActiveTrayModel.UpdateCoroutine"
	tray := NewActiveTrayModel()

	// 初始检查
	initialRender := tray.Render(80)
	wantPlaceholder := "No active background coroutines"
	if !strings.Contains(initialRender, wantPlaceholder) {
		t.Fatalf("%s(initial): rendered output %q does not contain want placeholder %q", fn, initialRender, wantPlaceholder)
	}

	// 1. 投递活跃协程 coroutine1
	addMsg := ActiveCoroutineMsg{
		AgentID:  "coroutine1",
		Role:     "Scout",
		TaskDesc: "reading AST symbols",
		Elapsed:  600 * time.Millisecond,
		Active:   true,
	}
	tray.UpdateCoroutine(addMsg)

	if got, want := len(tray.order), 1; got != want {
		t.Fatalf("%s(inputs=%+v): tray order length got %d, want %d", fn, addMsg, got, want)
	}
	if got, want := tray.order[0], "coroutine1"; got != want {
		t.Fatalf("%s(inputs=%+v): tray order[0] got %q, want %q", fn, addMsg, got, want)
	}
	renderedAdd := tray.Render(80)
	if !strings.Contains(renderedAdd, "[Scout]") || !strings.Contains(renderedAdd, "reading AST symbols") {
		t.Fatalf("%s(inputs=%+v): rendered output %q does not contain Scout info", fn, addMsg, renderedAdd)
	}

	// 2. 投递注销协程 coroutine1
	removeMsg := ActiveCoroutineMsg{
		AgentID: "coroutine1",
		Active:  false,
	}
	tray.UpdateCoroutine(removeMsg)

	if got, want := len(tray.order), 0; got != want {
		t.Fatalf("%s(inputs=%+v): tray order length got %d, want %d", fn, removeMsg, got, want)
	}
	renderedRemove := tray.Render(80)
	if !strings.Contains(renderedRemove, wantPlaceholder) {
		t.Fatalf("%s(inputs=%+v): rendered output %q does not contain placeholder %q", fn, removeMsg, renderedRemove, wantPlaceholder)
	}
}

// TC-TUI-007: 极小终端窗口缩放保护
func TestTUI_TC_TUI_007_WindowResizeClampProtection(t *testing.T) {
	fn := "MainUIModel.WindowResize"

	tests := []struct {
		name      string
		sizeMsg   tea.WindowSizeMsg
		wantLeftW int
		wantBodyH int
	}{
		{
			name:      "small_clamp_30x10",
			sizeMsg:   tea.WindowSizeMsg{Width: 30, Height: 10},
			wantLeftW: 20,
			wantBodyH: 5,
		},
		{
			name:      "extreme_clamp_10x5",
			sizeMsg:   tea.WindowSizeMsg{Width: 10, Height: 5},
			wantLeftW: 20,
			wantBodyH: 5,
		},
		{
			name:      "standard_120x40",
			sizeMsg:   tea.WindowSizeMsg{Width: 120, Height: 40},
			wantLeftW: 42,
			wantBodyH: 33, // 40 - 6 (opencode editor) - 1 (starship footer)
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			m := createTestMainUIModel(t, nil)
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s(inputs=%+v): unexpected panic: %v", fn, tc.sizeMsg, r)
				}
			}()

			updated, _ := m.Update(tc.sizeMsg)
			m = updated.(*MainUIModel)

			activeBuf := m.AgentBuffer(m.ActiveTabID())
			if activeBuf == nil {
				t.Fatalf("%s(inputs=%+v): active buffer got nil, want valid buffer", fn, tc.sizeMsg)
			}
			if got, want := activeBuf.Viewport.Height, tc.wantBodyH; got != want {
				t.Fatalf("%s(inputs=%+v): viewport height got %d, want %d", fn, tc.sizeMsg, got, want)
			}

			view := m.View()
			if view == "" {
				t.Fatalf("%s(inputs=%+v): view rendered empty string, want valid layout", fn, tc.sizeMsg)
			}
		})
	}
}

// TC-TUI-008: 输入框提交与内容清空
func TestTUI_TC_TUI_008_UserInputSubmit(t *testing.T) {
	fn := "MainUIModel.UserInputSubmit"
	var receivedPrompt string

	m := createTestMainUIModel(t, func(prompt string) {
		receivedPrompt = prompt
	})

	inputPrompt := "run plan"
	m.inputArea.SetValue(inputPrompt)

	enterKey := tea.KeyMsg{Type: tea.KeyEnter}
	updated, _ := m.Update(enterKey)
	m = updated.(*MainUIModel)

	if got, want := receivedPrompt, inputPrompt; got != want {
		t.Fatalf("%s(inputs=%+v): callback prompt got %q, want %q", fn, inputPrompt, got, want)
	}

	if got, want := m.InputArea().Value(), ""; got != want {
		t.Fatalf("%s(inputs=%+v): input area value got %q, want cleared %q", fn, inputPrompt, got, want)
	}

	activeBuf := m.AgentBuffer(m.ActiveTabID())
	wantEcho := "[User]: run plan"
	if !strings.Contains(activeBuf.RawTextBuffer, wantEcho) {
		t.Fatalf("%s(inputs=%+v): active buffer raw text got %q, want contains %q", fn, inputPrompt, activeBuf.RawTextBuffer, wantEcho)
	}
}

// TC-TUI-009: 空白输入提交拦截
func TestTUI_TC_TUI_009_EmptyInputIntercept(t *testing.T) {
	fn := "MainUIModel.EmptyInputIntercept"
	var callbackInvoked bool

	m := createTestMainUIModel(t, func(prompt string) {
		callbackInvoked = true
	})

	inputPrompt := "   "
	m.inputArea.SetValue(inputPrompt)

	activeBuf := m.AgentBuffer(m.ActiveTabID())
	originalBuffer := activeBuf.RawTextBuffer

	enterKey := tea.KeyMsg{Type: tea.KeyEnter}
	updated, _ := m.Update(enterKey)
	m = updated.(*MainUIModel)

	if callbackInvoked {
		t.Fatalf("%s(inputs=%q): callback was unexpectedly invoked, want not invoked", fn, inputPrompt)
	}

	currentBuffer := m.AgentBuffer(m.ActiveTabID()).RawTextBuffer
	if got, want := currentBuffer, originalBuffer; got != want {
		t.Fatalf("%s(inputs=%q): buffer content changed, got %q, want %q", fn, inputPrompt, got, want)
	}
}

// TC-TUI-010: 独立视口上下滚动
func TestTUI_TC_TUI_010_ViewportIndependentScroll(t *testing.T) {
	fn := "MainUIModel.ViewportIndependentScroll"
	m := createTestMainUIModel(t, nil)
	m.EnsureAgentBuffer("sub", "SubAgent")

	// 给 main Tab 注入 100 行输出
	var lines strings.Builder
	for i := 1; i <= 100; i++ {
		lines.WriteString(fmt.Sprintf("Line %03d: output log entry\n", i))
	}
	_, _ = m.Update(AgentStreamDeltaMsg{
		AgentID:   PrimaryAgentID,
		DeltaText: lines.String(),
	})

	mainBuf := m.AgentBuffer(PrimaryAgentID)
	subBuf := m.AgentBuffer("sub")

	initMainYOffset := mainBuf.Viewport.YOffset
	initSubYOffset := subBuf.Viewport.YOffset
	if initSubYOffset != 0 {
		t.Fatalf("%s(init): subBuf initial YOffset got %d, want 0", fn, initSubYOffset)
	}

	// 1. KeyUp (-1)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = updated.(*MainUIModel)
	if got, want := mainBuf.Viewport.YOffset, initMainYOffset-1; got != want {
		t.Fatalf("%s(KeyUp): mainBuf YOffset got %d, want %d", fn, got, want)
	}
	if got, want := subBuf.Viewport.YOffset, 0; got != want {
		t.Fatalf("%s(KeyUp): subBuf YOffset got %d, want %d", fn, got, want)
	}

	// 2. KeyDown (+1)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(*MainUIModel)
	if got, want := mainBuf.Viewport.YOffset, initMainYOffset; got != want {
		t.Fatalf("%s(KeyDown): mainBuf YOffset got %d, want %d", fn, got, want)
	}
	if got, want := subBuf.Viewport.YOffset, 0; got != want {
		t.Fatalf("%s(KeyDown): subBuf YOffset got %d, want %d", fn, got, want)
	}

	// 3. KeyPgUp (-5)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	m = updated.(*MainUIModel)
	if got, want := mainBuf.Viewport.YOffset, initMainYOffset-5; got != want {
		t.Fatalf("%s(KeyPgUp): mainBuf YOffset got %d, want %d", fn, got, want)
	}
	if got, want := subBuf.Viewport.YOffset, 0; got != want {
		t.Fatalf("%s(KeyPgUp): subBuf YOffset got %d, want %d", fn, got, want)
	}

	// 4. KeyPgDown (+5)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	m = updated.(*MainUIModel)
	if got, want := mainBuf.Viewport.YOffset, initMainYOffset; got != want {
		t.Fatalf("%s(KeyPgDown): mainBuf YOffset got %d, want %d", fn, got, want)
	}
	if got, want := subBuf.Viewport.YOffset, 0; got != want {
		t.Fatalf("%s(KeyPgDown): subBuf YOffset got %d, want %d", fn, got, want)
	}
}

// TC-TUI-011: Ctrl+C 退出事件捕获
func TestTUI_TC_TUI_011_CtrlCQuit(t *testing.T) {
	fn := "MainUIModel.CtrlCQuit"
	m := createTestMainUIModel(t, nil)

	input := tea.KeyMsg{Type: tea.KeyCtrlC}
	_, cmd := m.Update(input)

	if cmd == nil {
		t.Fatalf("%s(inputs=%+v): got nil cmd, want tea.Quit", fn, input)
	}

	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("%s(inputs=%+v): got msg type %T, want tea.QuitMsg", fn, input, msg)
	}
}

// Table-Driven 6 种状态徽标渲染用例测试
func TestTUI_DAGTree_BadgeRendering_TableDriven(t *testing.T) {
	fn := "WorkflowTreeModel.renderBadge"
	tree := NewWorkflowTreeModel()

	tests := []struct {
		name    string
		input   DAGNodeItem
		wantSub string
		wantErr error
		remark  string
	}{
		{
			name:    "badge_running",
			input:   DAGNodeItem{Status: "RUNNING"},
			wantSub: "[RUNNING]",
			wantErr: nil,
			remark:  "正向",
		},
		{
			name:    "badge_blocked",
			input:   DAGNodeItem{Status: "BLOCKED"},
			wantSub: "⏸ [BLOCKED]",
			wantErr: nil,
			remark:  "正向",
		},
		{
			name:    "badge_waiting_alias",
			input:   DAGNodeItem{Status: "WAITING"},
			wantSub: "⏸ [BLOCKED]",
			wantErr: nil,
			remark:  "别名兼容",
		},
		{
			name:    "badge_repair_with_attempts",
			input:   DAGNodeItem{Status: "REPAIR", Attempt: 1, MaxAttempts: 3},
			wantSub: "↻ [REPAIR: 1/3]",
			wantErr: nil,
			remark:  "边界",
		},
		{
			name:    "badge_repair_no_max",
			input:   DAGNodeItem{Status: "REPAIR", Attempt: 1, MaxAttempts: 0},
			wantSub: "↻ [REPAIR]",
			wantErr: nil,
			remark:  "缺省边界",
		},
		{
			name:    "badge_done",
			input:   DAGNodeItem{Status: "DONE"},
			wantSub: "✓ [DONE]",
			wantErr: nil,
			remark:  "正向",
		},
		{
			name:    "badge_success_alias",
			input:   DAGNodeItem{Status: "SUCCESS"},
			wantSub: "✓ [DONE]",
			wantErr: nil,
			remark:  "别名兼容",
		},
		{
			name:    "badge_approval",
			input:   DAGNodeItem{Status: "APPROVAL"},
			wantSub: "⚠ [APPROVAL]",
			wantErr: nil,
			remark:  "正向",
		},
		{
			name:    "badge_failed",
			input:   DAGNodeItem{Status: "FAILED"},
			wantSub: "✗ [FAILED]",
			wantErr: nil,
			remark:  "异常",
		},
		{
			name:    "badge_unknown_fallback",
			input:   DAGNodeItem{Status: "UNKNOWN_STATE"},
			wantSub: "[UNKNOWN_STATE]",
			wantErr: nil,
			remark:  "容错分支",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Helper()
			got := tree.renderBadge(&tc.input)
			if !strings.Contains(got, tc.wantSub) {
				t.Fatalf("%s(inputs=%+v): got %q, want substring %q", fn, tc.input, got, tc.wantSub)
			}
		})
	}
}

// TestTUI_DAGTree_RenderTree_Integration 验证整树渲染集成
func TestTUI_DAGTree_RenderTree_Integration(t *testing.T) {
	fn := "WorkflowTreeModel.Render"
	tree := NewWorkflowTreeModel()
	tree.SetWorkflowName("Feature DAG")

	tree.AddOrUpdateNode(DAGNodeStatusMsg{NodeID: "t1", Role: "Scout", Status: "RUNNING"})
	tree.AddOrUpdateNode(DAGNodeStatusMsg{NodeID: "t2", Role: "Planner", Status: "DONE"})
	tree.AddOrUpdateNode(DAGNodeStatusMsg{NodeID: "t3", Role: "Repairer", Status: "REPAIR", Attempt: 2, MaxAttempts: 3})
	tree.AddOrUpdateNode(DAGNodeStatusMsg{NodeID: "t4", Role: "Reviewer", Status: "FAILED", ErrorMsg: "syntax error"})

	output := tree.Render(45, 20)
	for _, want := range []string{"Feature DAG", "Scout", "[RUNNING]", "Planner", "[DONE]", "↻ [REPAIR: 2/3]", "syntax error"} {
		if !strings.Contains(output, want) {
			t.Fatalf("%s: rendered tree output missing %q, got:\n%s", fn, want, output)
		}
	}
}

// TestTUI_AgentStatusChangedMsg 验证 Agent 状态变更消息更新
func TestTUI_AgentStatusChangedMsg(t *testing.T) {
	fn := "MainUIModel.AgentStatusChangedMsg"
	m := createTestMainUIModel(t, nil)

	msg := AgentStatusChangedMsg{
		AgentID:   "sub-1",
		RoleName:  "Worker",
		NewStatus: "RUNNING",
		ToolName:  "bash",
	}
	_, _ = m.Update(msg)

	buf := m.AgentBuffer("sub-1")
	if buf == nil {
		t.Fatalf("%s(inputs=%+v): buffer got nil, want non-nil", fn, msg)
	}
	if got, want := buf.Status, "RUNNING"; got != want {
		t.Fatalf("%s(inputs=%+v): status got %q, want %q", fn, msg, got, want)
	}
	if got, want := buf.ActiveTool, "bash"; got != want {
		t.Fatalf("%s(inputs=%+v): tool got %q, want %q", fn, msg, got, want)
	}
}

// TestTUI_ErrorMsg 验证 ErrorMsg 写入活动缓冲
func TestTUI_ErrorMsg(t *testing.T) {
	fn := "MainUIModel.ErrorMsg"
	m := createTestMainUIModel(t, nil)

	errInput := errors.New("critical pipeline failure")
	_, _ = m.Update(ErrorMsg{Err: errInput})

	activeBuf := m.AgentBuffer(m.ActiveTabID())
	wantText := "critical pipeline failure"
	if !strings.Contains(activeBuf.RawTextBuffer, wantText) {
		t.Fatalf("%s(inputs=%v): buffer got %q, want contains %q", fn, errInput, activeBuf.RawTextBuffer, wantText)
	}
}

// 兼容别名映射，确保现有调用完全兼容
func TestTUI_AgentViewBufferIsolation(t *testing.T) { TestTUI_TC_TUI_001_BufferIsolation(t) }
func TestTUI_TabCycling(t *testing.T)               { TestTUI_TC_TUI_002_TabCycling(t) }
func TestTUI_ActiveTray(t *testing.T)               { TestTUI_TC_TUI_006_ActiveTrayAddRemove(t) }
func TestTUI_WindowResize(t *testing.T)             { TestTUI_TC_TUI_007_WindowResizeClampProtection(t) }
func TestTUI_FPSSafety(t *testing.T)                { TestTUI_TC_TUI_005_FPSSafety(t) }
func TestTUI_UserInput(t *testing.T)                { TestTUI_TC_TUI_008_UserInputSubmit(t) }
func TestTUI_ScrollAndKeys(t *testing.T)            { TestTUI_TC_TUI_010_ViewportIndependentScroll(t) }
func TestTUI_ZeroBufferSafety(t *testing.T)         { TestTUI_TC_TUI_004_SingleOrEmptyTabBoundary(t) }
func TestTUI_DAGTreeStatusBadges(t *testing.T)      { TestTUI_DAGTree_BadgeRendering_TableDriven(t) }

// ── pi-tidy + pi-zentui 新架构专项测试 ──────────────────────────────────────

func TestTUI_PiTidy_TurnAndToolGroupFolding(t *testing.T) {
	m := createTestMainUIModel(t, nil)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 35})

	_, _ = m.Update(UserSubmitPromptMsg{Prompt: "inspect repo"})
	_, _ = m.Update(TurnStartMsg{AgentID: PrimaryAgentID, Turn: 0})
	_, _ = m.Update(AgentThinkingDeltaMsg{AgentID: PrimaryAgentID, ThinkingDelta: "checking files first"})
	_, _ = m.Update(ToolCallStartMsg{AgentID: PrimaryAgentID, CallID: "c1", ToolName: "bash", ArgsRaw: `{"command":"git status"}`})
	_, _ = m.Update(ToolCallEndMsg{AgentID: PrimaryAgentID, CallID: "c1", ToolName: "bash", ArgsRaw: `{"command":"git status"}`, Result: "clean\n", Duration: 120 * time.Millisecond})
	_, _ = m.Update(ToolCallStartMsg{AgentID: PrimaryAgentID, CallID: "c2", ToolName: "read", ArgsRaw: `{"path":"go.mod"}`})
	_, _ = m.Update(ToolCallEndMsg{AgentID: PrimaryAgentID, CallID: "c2", ToolName: "read", ArgsRaw: `{"path":"go.mod"}`, Result: "module gp\n", Duration: 50 * time.Millisecond})
	_, _ = m.Update(AgentStreamDeltaMsg{AgentID: PrimaryAgentID, DeltaText: "All clean with `go.mod` and #89b4fa."})
	_, _ = m.Update(TurnEndMsg{AgentID: PrimaryAgentID, Turn: 0, Outcome: "completed"})

	view := m.View()
	// 1. User message framed
	if !strings.Contains(view, "inspect repo") {
		t.Fatalf("expected user prompt in view, got:\n%s", view)
	}
	// 2. Settled turn folded into summary line
	if !strings.Contains(view, "▸ took") || !strings.Contains(view, "2 calls") {
		t.Fatalf("expected folded turn summary line (▸ took ... 2 calls), got:\n%s", view)
	}
	// 3. Final reply framed with rounded corners & model name
	if !strings.Contains(view, "╭") || !strings.Contains(view, "gemini-4-argon") || !strings.Contains(view, "╰") {
		t.Fatalf("expected rounded final reply frame with model name, got:\n%s", view)
	}
	// 4. Ctrl+O expands all turns & tool groups
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	expandedView := m.View()
	if !strings.Contains(expandedView, "▾ took") || !strings.Contains(expandedView, "▾ 2 calls") {
		t.Fatalf("expected expanded turn & tool group after Ctrl+O, got:\n%s", expandedView)
	}
	// 5. Ctrl+T toggles thinking box
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	thinkView := m.View()
	if !strings.Contains(thinkView, "checking files first") {
		t.Fatalf("expected thinking content after Ctrl+T, got:\n%s", thinkView)
	}
}

func TestTUI_PiTidy_SplitDiffAndRuledCodeBlocks(t *testing.T) {
	th := DefaultTheme()
	cfg := DefaultUIConfig()

	// Split diff at wide width
	split := RenderSplitDiff("line1\noldValue\nline3", "line1\nnewValue\nline3", 90, 20, true, 72, th)
	joined := strings.Join(split, "\n")
	if !strings.Contains(joined, "oldValue") || !strings.Contains(joined, "newValue") || !strings.Contains(joined, "│") {
		t.Fatalf("expected split diff with old/new panes, got:\n%s", joined)
	}

	// Narrow width falls back to unified diff
	unified := RenderSplitDiff("oldValue", "newValue", 50, 20, true, 72, th)
	uJoined := strings.Join(unified, "\n")
	if !strings.Contains(uJoined, "- ") || !strings.Contains(uJoined, "+ ") {
		t.Fatalf("expected unified diff fallback at narrow width, got:\n%s", uJoined)
	}

	// Ruled code blocks + hex swatches
	md := RenderMarkdownWithCodeBlocks("Color #89b4fa:\n```go\nfmt.Println(1)\n```", 60, th, cfg, th.Accent, true)
	mdJoined := strings.Join(md, "\n")
	if !strings.Contains(mdJoined, "● #89b4fa") {
		t.Fatalf("expected hex swatch ● #89b4fa, got:\n%s", mdJoined)
	}
	if !strings.Contains(mdJoined, "go") || !strings.Contains(mdJoined, "fmt.Println(1)") {
		t.Fatalf("expected ruled code block in markdown output, got:\n%s", mdJoined)
	}
}

func TestTUI_PiZentui_EditorWorkingLineAndFooter(t *testing.T) {
	m := createTestMainUIModel(t, nil)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	_, _ = m.Update(TurnStartMsg{AgentID: PrimaryAgentID, Turn: 0})
	_, _ = m.Update(ToolCallStartMsg{AgentID: PrimaryAgentID, CallID: "c1", ToolName: "bash", ArgsRaw: `{"command":"go test"}`})
	_, _ = m.Update(TokenUsageMsg{InputTokens: 1200, OutputTokens: 350, TotalTokens: 1550})

	view := m.View()
	if !strings.Contains(view, "Simmering…") || !strings.Contains(view, "(bash)") {
		t.Fatalf("expected working line embedded in opencode editor border, got:\n%s", view)
	}
	if !strings.Contains(view, "ctx: 1.6k/128.0k") || !strings.Contains(view, "↑1.2k ↓350") {
		t.Fatalf("expected starship footer metrics, got:\n%s", view)
	}

	// Test /zentui preset minimalist
	m.inputArea.SetValue("/zentui preset minimalist")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := m.Config().EditorStyle; got != "minimalist" {
		t.Fatalf("expected minimalist editor style after /zentui preset, got %q", got)
	}
	if got := m.Config().FooterStyle; got != "hidden" {
		t.Fatalf("expected hidden footer style after /zentui preset minimalist, got %q", got)
	}
}

func TestTUI_SubagentDataFlowAndTabs(t *testing.T) {
	m := createTestMainUIModel(t, nil)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 35})

	_, _ = m.Update(TurnStartMsg{AgentID: PrimaryAgentID, Turn: 0})
	_, _ = m.Update(ToolCallStartMsg{
		AgentID:  PrimaryAgentID,
		CallID:   "call_sub_1",
		ToolName: "subagent",
		ArgsRaw:  `{"tasks":[{"agent":"pro1","task":"argue for speed"},{"agent":"con1","task":"argue for craft"}]}`,
	})
	_, _ = m.Update(AgentFlowMsg{
		ParentID:  PrimaryAgentID,
		ChildID:   "sub_pro1_1",
		Role:      "pro1",
		Direction: "dispatch",
		Payload:   "argue for speed",
	})
	_, _ = m.Update(AgentStreamDeltaMsg{AgentID: "sub_pro1_1", DeltaText: "Speed wins!"})
	_, _ = m.Update(AgentFlowMsg{
		ParentID:  PrimaryAgentID,
		ChildID:   "sub_pro1_1",
		Role:      "pro1",
		Direction: "return",
		Payload:   "Speed wins!",
		Duration:  400 * time.Millisecond,
	})

	view := m.View()
	if !strings.Contains(view, PrimaryAgentID+" ──▶") || !strings.Contains(view, "pro1") || !strings.Contains(view, "Speed wins!") {
		t.Fatalf("expected subagent data flow in main view, got:\n%s", view)
	}
	if !strings.Contains(view, "2:pro1") {
		t.Fatalf("expected subagent tab 2:pro1 in tab bar, got:\n%s", view)
	}
}

func TestTUI_LoginLogoutAndModelSelector(t *testing.T) {
	tmp := t.TempDir()
	store := auth.NewStoreWithPath(filepath.Join(tmp, "auth.json"))
	m := createTestMainUIModel(t, nil)
	m.SetAuthStore(store)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 35})

	// 1. /login qoder -> prompts for key -> Enter saves to auth.db
	m.inputArea.SetValue("/login qoder")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.overlay.Kind != OverlayLoginKey || m.overlay.TargetProv != "qoder" {
		t.Fatalf("expected OverlayLoginKey for qoder, got %+v", m.overlay)
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("pt-secret-key")})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if k, _, _, ok := store.ResolveCredential("qoder"); !ok || k != "pt-secret-key" {
		t.Fatalf("expected qoder key saved in auth.db, got (%q, %v)", k, ok)
	}

	// 2. /models -> opens Model Selector showing authenticated qoder models
	var switchedTo auth.ModelEntry
	var persisted bool
	m.SetModelChangeHandler(func(entry auth.ModelEntry, persist bool) error {
		switchedTo = entry
		persisted = persist
		return nil
	})
	m.inputArea.SetValue("/models")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.overlay.Kind != OverlayModelSelect {
		t.Fatalf("expected OverlayModelSelect, got %v", m.overlay.Kind)
	}
	ovView := m.View()
	if !strings.Contains(ovView, "Select Model") || !strings.Contains(ovView, "qoder/auto") {
		t.Fatalf("expected Select Model overlay with qoder/auto, got:\n%s", ovView)
	}
	// Ctrl+S saves as default model
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if switchedTo.FullName != "qoder/auto" || m.header.ModelName != "auto" || !persisted {
		t.Fatalf("expected model saved as default qoder/auto, got %+v / %q / persisted=%v", switchedTo, m.header.ModelName, persisted)
	}

	// 3. /logout qoder -> removes credential
	m.inputArea.SetValue("/logout qoder")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if _, _, _, ok := store.ResolveCredential("qoder"); ok {
		t.Fatalf("expected qoder credential removed after /logout")
	}
}

func TestTUI_SlashCommandCompletion(t *testing.T) {
	m := createTestMainUIModel(t, nil)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 35})

	// Typing "/" shows completion menu
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	view := m.View()
	if !strings.Contains(view, "/models") || !strings.Contains(view, "/login") || !strings.Contains(view, "/zentui") {
		t.Fatalf("expected slash command completion menu in view, got:\n%s", view)
	}

	// Typing "lo" filters to /login and /logout
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("lo")})
	view2 := m.View()
	if !strings.Contains(view2, "/login") || !strings.Contains(view2, "/logout") || strings.Contains(view2, "/zentui") {
		t.Fatalf("expected filtered slash commands (/login, /logout), got:\n%s", view2)
	}

	// Down selects /logout, Tab completes "/logout "
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if got := m.InputArea().Value(); got != "/logout " {
		t.Fatalf("expected Tab to complete '/logout ', got %q", got)
	}
}

func TestTUI_AskUserQuestionDialog(t *testing.T) {
	m := createTestMainUIModel(t, nil)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 110, Height: 40})

	respCh := make(chan AskUserResult, 1)
	_, _ = m.Update(AskUserQuestionMsg{
		ResponseCh: respCh,
		Questions: []AskUserQuestion{
			{
				Question: "Which auth method?",
				Header:   "Auth",
				Options: []AskUserOption{
					{Label: "JWT", Description: "Stateless token", Preview: "Authorization: Bearer ..."},
					{Label: "Session", Description: "Cookie store"},
				},
			},
			{
				Question:    "Which extras?",
				Header:      "Extras",
				MultiSelect: true,
				Options: []AskUserOption{
					{Label: "Metrics", Description: "Prometheus"},
					{Label: "Tracing", Description: "OpenTelemetry"},
				},
			},
		},
	})

	view := m.View()
	if !strings.Contains(view, "ask_user_question") || !strings.Contains(view, "Which auth method?") || !strings.Contains(view, "preview") {
		t.Fatalf("expected ask_user_question dialog with preview in view, got:\n%s", view)
	}

	// Pick option 1 ("JWT") -> advances to tab 1 ("Extras")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	// Check option 0 ("Metrics") in multi-select, then navigate to Next and press Enter -> Submit tab
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	// Submit tab: Enter submits
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	res := <-respCh
	if res.Cancelled || len(res.Answers) != 2 {
		t.Fatalf("expected 2 non-cancelled answers, got %+v", res)
	}
	if res.Answers[0].Answer != "JWT" || len(res.Answers[1].Selected) != 1 || res.Answers[1].Selected[0] != "Metrics" {
		t.Fatalf("unexpected answers: %+v", res.Answers)
	}
}

func TestTUI_ResumeCommandAndOverlay(t *testing.T) {
	m := createTestMainUIModel(t, nil)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 35})

	var resumedID string
	m.SetSessionHandlers(
		func() ([]storage.SessionSummary, error) {
			return []storage.SessionSummary{
				{SessionID: "sess_1001", MessageCount: 4, FirstPrompt: "build http cache", UpdatedAt: time.Now()},
				{SessionID: "sess_1002", MessageCount: 2, FirstPrompt: "fix login bug", UpdatedAt: time.Now().Add(-time.Hour)},
			}, nil
		},
		func(sessionID string) ([]ai.Message, error) {
			resumedID = sessionID
			return []ai.Message{
				ai.NewUserMessage("build http cache"),
				ai.NewAssistantMessage("Here is the cache middleware plan.", ""),
			}, nil
		},
	)

	m.inputArea.SetValue("/resume")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.overlay.Kind != OverlayResumeSelect {
		t.Fatalf("expected OverlayResumeSelect, got %v", m.overlay.Kind)
	}
	view := m.View()
	if !strings.Contains(view, "Resume Conversation") || !strings.Contains(view, "sess_1001") || !strings.Contains(view, "build http cache") {
		t.Fatalf("expected Resume Conversation overlay with sess_1001, got:\n%s", view)
	}

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if resumedID != "sess_1001" || m.header.SessionID != "sess_1001" {
		t.Fatalf("expected resumed sess_1001, got %q / %q", resumedID, m.header.SessionID)
	}
	resumedView := m.View()
	if !strings.Contains(resumedView, "build http cache") || !strings.Contains(resumedView, "Here is the cache middleware plan.") {
		t.Fatalf("expected restored conversation rendered in transcript, got:\n%s", resumedView)
	}
}

func TestTUI_ResetSubagentViewsOnNextPrompt(t *testing.T) {
	var submitted []string
	m := createTestMainUIModel(t, func(prompt string) {
		submitted = append(submitted, prompt)
	})
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 35})

	// Simulate previous turn with subagent tab, DAG tree, and active DAG view
	_, _ = m.Update(AgentFlowMsg{
		ParentID:  PrimaryAgentID,
		ChildID:   "sub_impl_1",
		Role:      "implementer",
		Direction: "dispatch",
		Payload:   "scan repo",
	})
	_, _ = m.Update(AgentFlowMsg{
		ParentID:  PrimaryAgentID,
		ChildID:   "sub_impl_1",
		Role:      "implementer",
		Direction: "return",
		Payload:   "done",
	})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})

	if len(m.tabIDs) != 2 || m.activeTabID != "sub_impl_1" || !m.showDAG || len(m.workflowTree.nodes) == 0 {
		t.Fatalf("precondition failed: tabs=%v active=%s showDAG=%v nodes=%d", m.tabIDs, m.activeTabID, m.showDAG, len(m.workflowTree.nodes))
	}

	// Next conversation turn starts via Enter
	m.inputArea.SetValue("next question")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if len(submitted) != 1 || submitted[0] != "next question" {
		t.Fatalf("expected prompt submitted, got %v", submitted)
	}
	if m.activeTabID != PrimaryAgentID {
		t.Fatalf("expected activeTabID reset to %s, got %s", PrimaryAgentID, m.activeTabID)
	}
	if len(m.tabIDs) != 1 || m.tabIDs[0] != PrimaryAgentID {
		t.Fatalf("expected tabIDs reset to [%s], got %v", PrimaryAgentID, m.tabIDs)
	}
	if _, exists := m.agentBuffers["sub_impl_1"]; exists {
		t.Fatalf("expected subagent buffer sub_impl_1 to be removed")
	}
	if m.showDAG {
		t.Fatalf("expected showDAG reset to false")
	}
	if len(m.workflowTree.nodes) != 0 || len(m.tray.order) != 0 {
		t.Fatalf("expected workflowTree and tray reset, got nodes=%d tray=%d", len(m.workflowTree.nodes), len(m.tray.order))
	}
	view := m.View()
	if strings.Contains(view, "2:implementer") || strings.Contains(view, "Tab switch") {
		t.Fatalf("expected subagent tab bar cleared in View, got:\n%s", view)
	}
}
