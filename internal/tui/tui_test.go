package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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
		{step: 3, keyMsg: tea.KeyMsg{Type: tea.KeyTab}, want: "main"},
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
				// 仅有默认的 "main" Tab
			},
			inputKey: tea.KeyMsg{Type: tea.KeyTab},
			wantTab:  "main",
		},
		{
			name: "single_tab_prev",
			setupModel: func(m *MainUIModel) {
				// 仅有默认的 "main" Tab
			},
			inputKey: tea.KeyMsg{Type: tea.KeyShiftTab},
			wantTab:  "main",
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
			wantBodyH: 30,
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
		AgentID:   "main",
		DeltaText: lines.String(),
	})

	mainBuf := m.AgentBuffer("main")
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
