package tui

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// HitKind 标识可点击折叠/展开的交互区域类型
type HitKind int

const (
	HitTurn HitKind = iota + 1
	HitGroup
	HitTool
	HitThinking
)

// HitRegion 记录渲染行号区间对应的交互目标
type HitRegion struct {
	StartLine int
	EndLine   int
	Kind      HitKind
	TurnIdx   int
	GroupIdx  int
	ToolIdx   int
	StepIdx   int
}

// TranscriptItem 标识对话记录中的顶层条目
type TranscriptItem struct {
	Kind    string // "user" | "turn" | "error" | "notice"
	User    *UserItem
	Turn    *TurnItem
	ErrItem *ErrorItem
	Notice  string
}

type UserItem struct {
	Text      string
	Timestamp time.Time
}

type ErrorItem struct {
	Message string
	Count   int
}

type ToolCallItem struct {
	CallID    string
	ToolName  string
	ArgsRaw   string
	Result    string
	IsError   bool
	Pending   bool
	Expanded  bool
	StartTime time.Time
	Duration  time.Duration
}

type ToolGroup struct {
	Open     bool
	Tools    []*ToolCallItem
	Thoughts []string // 归属于该工具组展示的思考摘要
}

type TurnStep struct {
	Kind      string // "thinking" | "text" | "group" | "tool_box"
	Thinking  string
	ThinkOpen bool
	ThinkDur  time.Duration
	Text      string
	Group     *ToolGroup
	Tool      *ToolCallItem
}

type TurnItem struct {
	ID         string
	StartedAt  time.Time
	EndedAt    time.Time
	Outcome    string // "running" | "completed" | "error" | "interrupted"
	Model      string
	Provider   string
	Folded     bool
	FoldManual bool // 用户手动切换过折叠状态时不再自动覆盖
	Steps      []*TurnStep
	FinalText  string
}

// AgentFlowEntry 记录主 Agent 与子 Agent 之间的一次派发与返回数据流
type AgentFlowEntry struct {
	ParentID  string
	ChildID   string
	Role      string
	TaskIn    string
	ResultOut string
	IsError   bool
	Pending   bool
	StartTime time.Time
	Duration  time.Duration
}

// Transcript 管理单个 Agent 的结构化对话历史与折叠状态
type Transcript struct {
	Items      []*TranscriptItem
	HitRegions []HitRegion
	Flows      []*AgentFlowEntry
	Model      string
	Provider   string
	AllOpen    bool
}

func NewTranscript(model, provider string) *Transcript {
	return &Transcript{
		Model:    model,
		Provider: provider,
	}
}

func (tr *Transcript) RecordAgentFlow(msg AgentFlowMsg) {
	if msg.Direction == "dispatch" {
		tr.Flows = append(tr.Flows, &AgentFlowEntry{
			ParentID:  msg.ParentID,
			ChildID:   msg.ChildID,
			Role:      msg.Role,
			TaskIn:    msg.Payload,
			Pending:   true,
			StartTime: time.Now(),
		})
		return
	}
	for _, v := range slices.Backward(tr.Flows) {
		if v.ChildID == msg.ChildID {
			v.Pending = false
			v.ResultOut = msg.Payload
			v.IsError = msg.IsError
			v.Duration = msg.Duration
			return
		}
	}
}

func (tr *Transcript) AddUserMessage(text string, ts time.Time) {
	if ts.IsZero() {
		ts = time.Now()
	}
	tr.Items = append(tr.Items, &TranscriptItem{
		Kind: "user",
		User: &UserItem{Text: text, Timestamp: ts},
	})
}

func (tr *Transcript) AddNotice(msg string) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return
	}
	tr.Items = append(tr.Items, &TranscriptItem{
		Kind:   "notice",
		Notice: msg,
	})
}

func (tr *Transcript) AddError(msg string) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return
	}
	if n := len(tr.Items); n > 0 && tr.Items[n-1].Kind == "error" && tr.Items[n-1].ErrItem.Message == msg {
		tr.Items[n-1].ErrItem.Count++
		return
	}
	tr.Items = append(tr.Items, &TranscriptItem{
		Kind:    "error",
		ErrItem: &ErrorItem{Message: msg, Count: 1},
	})
}

func (tr *Transcript) ActiveTurn(cfg UIConfig) *TurnItem {
	if n := len(tr.Items); n > 0 && tr.Items[n-1].Kind == "turn" && tr.Items[n-1].Turn.Outcome == "running" {
		return tr.Items[n-1].Turn
	}
	return tr.StartTurn(cfg)
}

func (tr *Transcript) StartTurn(cfg UIConfig) *TurnItem {
	now := time.Now()
	// 防御性收口：确保同一时刻最多只有一个 running Turn，避免任何遗留 Turn 持续计时
	for _, it := range tr.Items {
		if it.Kind == "turn" && it.Turn != nil && it.Turn.Outcome == "running" {
			it.Turn.Outcome = "completed"
			if it.Turn.EndedAt.IsZero() {
				it.Turn.EndedAt = now
			}
		}
	}
	turn := &TurnItem{
		ID:        fmt.Sprintf("turn_%d", len(tr.Items)+1),
		StartedAt: now,
		Outcome:   "running",
		Model:     tr.Model,
		Provider:  tr.Provider,
		Folded:    cfg.CollapseTurns == "live",
	}
	tr.Items = append(tr.Items, &TranscriptItem{
		Kind: "turn",
		Turn: turn,
	})
	return turn
}

func (tr *Transcript) AppendThinkingDelta(delta string, cfg UIConfig) {
	turn := tr.ActiveTurn(cfg)
	if len(turn.Steps) > 0 && turn.Steps[len(turn.Steps)-1].Kind == "thinking" {
		turn.Steps[len(turn.Steps)-1].Thinking += delta
		return
	}
	turn.Steps = append(turn.Steps, &TurnStep{
		Kind:     "thinking",
		Thinking: delta,
	})
}

func (tr *Transcript) AppendTextDelta(delta string, cfg UIConfig) {
	turn := tr.ActiveTurn(cfg)
	if len(turn.Steps) > 0 && turn.Steps[len(turn.Steps)-1].Kind == "text" {
		turn.Steps[len(turn.Steps)-1].Text += delta
		return
	}
	turn.Steps = append(turn.Steps, &TurnStep{
		Kind: "text",
		Text: delta,
	})
}

func (tr *Transcript) StartToolCall(callID, toolName, argsRaw string, cfg UIConfig) {
	turn := tr.ActiveTurn(cfg)
	tc := &ToolCallItem{
		CallID:    callID,
		ToolName:  toolName,
		ArgsRaw:   argsRaw,
		Pending:   true,
		StartTime: time.Now(),
	}
	if !cfg.GroupTools || cfg.IsAlwaysShownTool(toolName) {
		turn.Steps = append(turn.Steps, &TurnStep{
			Kind: "tool_box",
			Tool: tc,
		})
		return
	}
	if len(turn.Steps) > 0 && turn.Steps[len(turn.Steps)-1].Kind == "group" {
		turn.Steps[len(turn.Steps)-1].Group.Tools = append(turn.Steps[len(turn.Steps)-1].Group.Tools, tc)
		return
	}
	turn.Steps = append(turn.Steps, &TurnStep{
		Kind: "group",
		Group: &ToolGroup{
			Open:  cfg.GroupOpen || tr.AllOpen,
			Tools: []*ToolCallItem{tc},
		},
	})
}

func (tr *Transcript) FinishToolCall(callID, toolName, argsRaw, result string, isErr bool, dur time.Duration) {
	for _, v := range slices.Backward(tr.Items) {
		if v.Kind != "turn" {
			continue
		}
		turn := v.Turn
		for _, st := range slices.Backward(turn.Steps) {

			if st.Kind == "tool_box" && st.Tool != nil && (st.Tool.CallID == callID || (callID == "" && st.Tool.ToolName == toolName && st.Tool.Pending)) {
				st.Tool.Pending = false
				st.Tool.Result = result
				st.Tool.IsError = isErr
				st.Tool.Duration = dur
				if argsRaw != "" {
					st.Tool.ArgsRaw = argsRaw
				}
				return
			}
			if st.Kind == "group" && st.Group != nil {
				for _, tc := range slices.Backward(st.Group.Tools) {

					if tc.CallID == callID || (callID == "" && tc.ToolName == toolName && tc.Pending) {
						tc.Pending = false
						tc.Result = result
						tc.IsError = isErr
						tc.Duration = dur
						if argsRaw != "" {
							tc.ArgsRaw = argsRaw
						}
						return
					}
				}
			}
		}
	}
}

func (tr *Transcript) FinishTurn(outcome string, cfg UIConfig) {
	now := time.Now()
	for _, v := range slices.Backward(tr.Items) {
		if v.Kind != "turn" || v.Turn == nil {
			continue
		}
		turn := v.Turn
		if turn.Outcome != "running" {
			continue
		}
		turn.Outcome = outcome
		turn.EndedAt = now
		// 提取最后一个非空 text step 作为 FinalText（仅当已结束且存在回复时）
		for _, v := range slices.Backward(turn.Steps) {
			if v.Kind == "text" && strings.TrimSpace(v.Text) != "" {
				turn.FinalText = v.Text
				break
			}
		}
		if !turn.FoldManual {
			switch cfg.CollapseTurns {
			case "afterRun", "live":
				turn.Folded = turn.hasProcess()
			case "off":
				turn.Folded = false
			}
		}
	}
}

func (t *TurnItem) hasProcess() bool {
	if t.Outcome == "error" || t.Outcome == "interrupted" {
		return true
	}
	textCount := 0
	for _, st := range t.Steps {
		switch st.Kind {
		case "thinking":
			if strings.TrimSpace(st.Thinking) != "" {
				return true
			}
		case "group", "tool_box":
			return true
		case "text":
			if strings.TrimSpace(st.Text) != "" {
				textCount++
				if textCount > 1 {
					return true
				}
			}
		}
	}
	return false
}

// ToggleAllExpanded 对应 Ctrl+O：切换最近/所有工具与思考的展开状态
func (tr *Transcript) ToggleAllExpanded() {
	tr.AllOpen = !tr.AllOpen
	for _, it := range tr.Items {
		if it.Kind != "turn" {
			continue
		}
		turn := it.Turn
		if turn.hasProcess() {
			turn.Folded = !tr.AllOpen
			turn.FoldManual = true
		}
		for _, st := range turn.Steps {
			if st.Kind == "group" && st.Group != nil {
				st.Group.Open = tr.AllOpen
				for _, tc := range st.Group.Tools {
					tc.Expanded = tr.AllOpen
				}
			} else if st.Kind == "tool_box" && st.Tool != nil {
				st.Tool.Expanded = tr.AllOpen
			}
		}
	}
}

// ToggleLatestThinking 对应 Ctrl+T：切换最近一条思考块的展开状态
func (tr *Transcript) ToggleLatestThinking() {
	for _, v := range slices.Backward(tr.Items) {
		if v.Kind != "turn" {
			continue
		}
		turn := v.Turn
		for _, v := range slices.Backward(turn.Steps) {
			if v.Kind == "thinking" {
				v.ThinkOpen = !v.ThinkOpen
				if turn.Folded && v.ThinkOpen {
					turn.Folded = false
					turn.FoldManual = true
				}
				return
			}
		}
	}
}

// HandleClick 根据点击行号命中折叠/展开目标
func (tr *Transcript) HandleClick(lineIdx int) bool {
	// 从内层往外层匹配（Tool -> Group -> Thinking -> Turn）
	for _, hr := range slices.Backward(tr.HitRegions) {

		if lineIdx < hr.StartLine || lineIdx > hr.EndLine {
			continue
		}
		if hr.TurnIdx < 0 || hr.TurnIdx >= len(tr.Items) || tr.Items[hr.TurnIdx].Kind != "turn" {
			continue
		}
		turn := tr.Items[hr.TurnIdx].Turn
		switch hr.Kind {
		case HitTool:
			if hr.StepIdx >= 0 && hr.StepIdx < len(turn.Steps) {
				st := turn.Steps[hr.StepIdx]
				if st.Kind == "group" && st.Group != nil && hr.ToolIdx >= 0 && hr.ToolIdx < len(st.Group.Tools) {
					st.Group.Tools[hr.ToolIdx].Expanded = !st.Group.Tools[hr.ToolIdx].Expanded
					return true
				}
				if st.Kind == "tool_box" && st.Tool != nil {
					st.Tool.Expanded = !st.Tool.Expanded
					return true
				}
			}
		case HitGroup:
			if hr.StepIdx >= 0 && hr.StepIdx < len(turn.Steps) && turn.Steps[hr.StepIdx].Group != nil {
				turn.Steps[hr.StepIdx].Group.Open = !turn.Steps[hr.StepIdx].Group.Open
				return true
			}
		case HitThinking:
			if hr.StepIdx >= 0 && hr.StepIdx < len(turn.Steps) {
				turn.Steps[hr.StepIdx].ThinkOpen = !turn.Steps[hr.StepIdx].ThinkOpen
				return true
			}
		case HitTurn:
			turn.Folded = !turn.Folded
			turn.FoldManual = true
			return true
		}
	}
	return false
}

// Render 将整份 Transcript 渲染为带样式的终端行，并重建 HitRegions
func (tr *Transcript) Render(width int, t Theme, cfg UIConfig, spinnerGlyph string) string {
	if width < 20 {
		width = 20
	}
	tr.HitRegions = tr.HitRegions[:0]
	var lines []string

	for idx, item := range tr.Items {
		switch item.Kind {
		case "user":
			lines = append(lines, renderUserItem(item.User, width, t, cfg)...)
			lines = append(lines, "")
		case "error":
			lines = append(lines, renderErrorItem(item.ErrItem, width, t, cfg)...)
			lines = append(lines, "")
		case "notice":
			lines = append(lines, " "+t.Fg(t.Success, item.Notice))
			lines = append(lines, "")
		case "turn":
			turnLines := tr.renderTurnItem(idx, item.Turn, len(lines), width, t, cfg, spinnerGlyph)
			lines = append(lines, turnLines...)
			lines = append(lines, "")
		}
	}
	return strings.Join(lines, "\n")
}

func renderUserItem(u *UserItem, width int, t Theme, cfg UIConfig) []string {
	userColor := t.MessageColor("user")
	timeStr := ""
	if cfg.UserMessageTime {
		timeStr = t.ItalicFg(t.Dim, FormatMessageTime(u.Timestamp))
	}
	icon := UserIcon(cfg.Icons)
	identity := t.BoldFg(userColor, icon)
	label, lead := MessageHeaderLabel(t, userColor, identity, timeStr, cfg.MessageHeaderStyle, width)

	switch cfg.UserMessageStyle {
	case "zentui-framed":
		rule := t.Fg(t.BorderMuted, strings.Repeat("─", width))
		rail := t.BoldFg(userColor, "│") + " "
		wrapped := wrapStyledLine(t.Fg(t.Text, u.Text), width-2)
		out := []string{rule, rail}
		for _, wl := range wrapped {
			out = append(out, rail+wl)
		}
		out = append(out, rail, rule)
		return out

	case "rail", "rules":
		topRule := LabeledRule(width, t, t.BorderMuted, label, cfg.UserMessageAlign, lead)
		botRule := t.Fg(t.BorderMuted, strings.Repeat("─", width))
		useRail := cfg.UserMessageStyle == "rail"
		rail := ""
		contentW := width
		if useRail {
			rail = t.BoldFg(userColor, "│") + " "
			contentW = width - 2
		}
		wrapped := wrapStyledLine(t.Fg(t.Text, u.Text), contentW)
		out := []string{topRule}
		if useRail {
			out = append(out, rail)
		}
		for _, wl := range wrapped {
			out = append(out, rail+wl)
		}
		if useRail {
			out = append(out, rail)
		}
		out = append(out, botRule)
		return out

	case "off":
		return wrapStyledLine(t.BoldFg(userColor, "> ")+t.Fg(t.Text, u.Text), width)

	default: // "frame" (pi-tidy rounded frame hugging the text)
		textLines := wrapStyledLine(t.Fg(t.Text, u.Text), width-4)
		maxW := VisibleWidth(label) + 4
		for _, tl := range textLines {
			if w := VisibleWidth(tl) + 4; w > maxW {
				maxW = w
			}
		}
		if maxW > width {
			maxW = width
		}
		if maxW < 12 {
			maxW = 12
		}
		padded := make([]string, len(textLines))
		for i, tl := range textLines {
			padded[i] = " " + tl
		}
		return DrawFrame(padded, maxW, t, userColor, label, cfg.UserMessageAlign, lead)
	}
}

func renderErrorItem(e *ErrorItem, width int, t Theme, cfg UIConfig) []string {
	title := t.BoldFg(t.Error, "✗ error")
	if e.Count > 1 {
		title = t.BoldFg(t.Error, fmt.Sprintf("✗ error ×%d", e.Count))
	}
	body := wrapStyledLine(t.Fg(t.Error, e.Message), width-4)
	for i := range body {
		body[i] = " " + body[i]
	}
	return DrawFrame(body, width, t, t.Error, title, "left", 1)
}

func (tr *Transcript) renderTurnItem(turnIdx int, turn *TurnItem, baseLine int, width int, t Theme, cfg UIConfig, spinnerGlyph string) []string {
	var out []string
	hasProc := turn.hasProcess()
	assistantColor := t.MessageColor("assistant")

	// 找到最后一个非空文本步骤作为 final reply 候选
	finalStepIdx := -1
	if turn.Outcome != "running" {
		for j, v := range slices.Backward(turn.Steps) {
			if v.Kind == "text" && strings.TrimSpace(v.Text) != "" {
				finalStepIdx = j
				break
			}
		}
	}

	if hasProc && cfg.CollapseTurns != "off" {
		sumLine := tr.formatTurnSummary(turn, width, t, cfg, spinnerGlyph)
		tr.HitRegions = append(tr.HitRegions, HitRegion{
			StartLine: baseLine + len(out),
			EndLine:   baseLine + len(out),
			Kind:      HitTurn,
			TurnIdx:   turnIdx,
		})
		out = append(out, sumLine)

		if turn.Folded {
			// 折叠态仅保留最终回复（或失败提示）
			if finalStepIdx >= 0 {
				out = append(out, renderFinalReply(turn.Steps[finalStepIdx].Text, turn, width, t, cfg, false)...)
			}
			return out
		}
	}

	// 展开态：若有流程，左侧绘制贯穿到 final reply 边框的 rail (`│`)
	railActive := hasProc && cfg.CollapseTurns != "off"
	railPrefix := ""
	innerW := width
	if railActive {
		railPrefix = " " + t.Fg(assistantColor, "│")
		innerW = width - 2
	}

	for stepIdx, st := range turn.Steps {
		if stepIdx == finalStepIdx && cfg.FinalReplyStyle != "off" {
			replyLines := renderFinalReply(st.Text, turn, width, t, cfg, railActive)
			out = append(out, replyLines...)
			continue
		}

		switch st.Kind {
		case "thinking":
			if !cfg.ShowThinking || strings.TrimSpace(st.Thinking) == "" {
				continue
			}
			startL := baseLine + len(out)
			thinkLines := renderThinkingStep(st, innerW, t, cfg, turn.Outcome == "running", spinnerGlyph)
			for _, tl := range thinkLines {
				if railActive {
					out = append(out, railPrefix+tl)
				} else {
					out = append(out, tl)
				}
			}
			tr.HitRegions = append(tr.HitRegions, HitRegion{
				StartLine: startL,
				EndLine:   baseLine + len(out) - 1,
				Kind:      HitThinking,
				TurnIdx:   turnIdx,
				StepIdx:   stepIdx,
			})

		case "text":
			if strings.TrimSpace(st.Text) == "" {
				continue
			}
			mdLines := RenderMarkdownWithCodeBlocks(st.Text, innerW-1, t, cfg, assistantColor, false)
			for _, ml := range mdLines {
				if railActive {
					out = append(out, railPrefix+" "+ml)
				} else {
					out = append(out, " "+ml)
				}
			}

		case "group":
			gLines := tr.renderToolGroup(turnIdx, stepIdx, st.Group, baseLine+len(out), innerW, t, cfg, spinnerGlyph, railPrefix)
			out = append(out, gLines...)

		case "tool_box":
			startL := baseLine + len(out)
			boxW := innerW
			if railActive {
				boxW = innerW - 1
			}
			bLines := tr.renderToolBox(st.Tool, boxW, t, cfg, spinnerGlyph)
			for _, bl := range bLines {
				if railActive {
					out = append(out, railPrefix+" "+bl)
				} else {
					out = append(out, bl)
				}
			}
			tr.HitRegions = append(tr.HitRegions, HitRegion{
				StartLine: startL,
				EndLine:   baseLine + len(out) - 1,
				Kind:      HitTool,
				TurnIdx:   turnIdx,
				StepIdx:   stepIdx,
			})
		}
	}

	return out
}

func renderFinalReply(text string, turn *TurnItem, width int, t Theme, cfg UIConfig, joinRail bool) []string {
	assistantColor := t.MessageColor("assistant")
	if cfg.FinalReplyStyle == "off" {
		return RenderMarkdownWithCodeBlocks(text, width, t, cfg, assistantColor, false)
	}
	pIcon := ProviderIcon(turn.Provider, turn.Model, cfg.Icons)
	modelLabel := turn.Model
	if modelLabel == "" {
		modelLabel = "agent"
	}
	identity := modelLabel
	if pIcon != "" {
		identity = pIcon + " " + modelLabel
	}
	identityStyled := t.BoldFg(assistantColor, identity)
	timeStr := t.ItalicFg(t.Dim, FormatMessageTime(turn.EndedAt))
	label, lead := MessageHeaderLabel(t, assistantColor, identityStyled, timeStr, cfg.MessageHeaderStyle, width)

	if cfg.FinalReplyStyle == "rail" || cfg.FinalReplyStyle == "rules" {
		topRule := LabeledRule(width, t, assistantColor, label, "left", lead)
		botRule := t.Fg(assistantColor, strings.Repeat("─", width))
		useRail := cfg.FinalReplyStyle == "rail"
		contentW := width
		rail := ""
		if useRail {
			rail = t.Fg(assistantColor, "│") + " "
			contentW = width - 2
		}
		mdLines := RenderMarkdownWithCodeBlocks(text, contentW, t, cfg, assistantColor, false)
		out := []string{topRule}
		for _, ml := range mdLines {
			out = append(out, rail+ml)
		}
		out = append(out, botRule)
		return out
	}

	// "frame": 全宽圆角框，内置 markdown + 分割线代码块
	innerW := width - 2
	mdLines := RenderMarkdownWithCodeBlocks(text, innerW-2, t, cfg, assistantColor, true)
	padded := make([]string, len(mdLines))
	for i, ml := range mdLines {
		padded[i] = " " + ClipToWidth(ml, innerW-1, "…")
	}
	framed := DrawFrame(padded, width, t, assistantColor, label, "left", lead)
	if joinRail && len(framed) > 0 {
		// 将左上角 ╭ 替换为 ├ 以承接上方展开态的 rail
		framed[0] = strings.Replace(framed[0], "╭", "├", 1)
	}
	return framed
}

func (tr *Transcript) formatTurnSummary(turn *TurnItem, width int, t Theme, cfg UIConfig, spinnerGlyph string) string {
	assistantColor := t.MessageColor("assistant")
	marker := "▸"
	if !turn.Folded {
		marker = "▾"
	}
	total := time.Since(turn.StartedAt)
	if !turn.EndedAt.IsZero() {
		total = turn.EndedAt.Sub(turn.StartedAt)
	}
	verb := "took"
	if turn.Outcome == "running" {
		verb = "running"
		if spinnerGlyph != "" {
			marker = spinnerGlyph
		}
	}

	var parts []string
	parts = append(parts, t.Fg(assistantColor, fmt.Sprintf("%s %s %s", marker, verb, formatShortDuration(total))))

	var toolCount, failedCount int
	var toolOrder []string
	toolCounts := make(map[string]int)
	var totalToolDur time.Duration
	thoughtCount := 0

	for _, st := range turn.Steps {
		switch st.Kind {
		case "thinking":
			if strings.TrimSpace(st.Thinking) != "" {
				thoughtCount++
			}
		case "tool_box":
			if st.Tool != nil {
				toolCount++
				totalToolDur += st.Tool.Duration
				if st.Tool.IsError {
					failedCount++
				}
				if toolCounts[st.Tool.ToolName] == 0 {
					toolOrder = append(toolOrder, st.Tool.ToolName)
				}
				toolCounts[st.Tool.ToolName]++
			}
		case "group":
			if st.Group != nil {
				for _, tc := range st.Group.Tools {
					toolCount++
					totalToolDur += tc.Duration
					if tc.IsError {
						failedCount++
					}
					if toolCounts[tc.ToolName] == 0 {
						toolOrder = append(toolOrder, tc.ToolName)
					}
					toolCounts[tc.ToolName]++
				}
			}
		}
	}

	if failedCount > 0 {
		parts = append(parts, t.Fg(t.Error, fmt.Sprintf("%d failed", failedCount)))
	}
	llmDur := max(total-totalToolDur, 0)
	pIcon := ProviderIcon(turn.Provider, turn.Model, cfg.Icons)
	if pIcon != "" {
		parts = append(parts, t.Fg(t.Muted, fmt.Sprintf("%s LLM %s", pIcon, formatShortDuration(llmDur))))
	} else {
		parts = append(parts, t.Fg(t.Muted, fmt.Sprintf("LLM %s", formatShortDuration(llmDur))))
	}
	if totalToolDur > 0 {
		parts = append(parts, t.Fg(t.Muted, fmt.Sprintf("tools %s", formatShortDuration(totalToolDur))))
	}
	if turn.Folded && toolCount > 0 {
		callWord := "calls"
		if toolCount == 1 {
			callWord = "call"
		}
		parts = append(parts, t.Fg(t.Accent, fmt.Sprintf("%d %s", toolCount, callWord)))
		var bd []string
		for _, name := range toolOrder {
			ic := ToolKindIcon(name, cfg.Icons)
			if ic != "" {
				bd = append(bd, t.Fg(t.Muted, fmt.Sprintf("%s %s×%d", ic, name, toolCounts[name])))
			} else {
				bd = append(bd, t.Fg(t.Muted, fmt.Sprintf("%s×%d", name, toolCounts[name])))
			}
		}
		parts = append(parts, strings.Join(bd, "  "))
	}
	if turn.Folded && thoughtCount > 0 {
		ic := ThinkingIcon(cfg.Icons)
		label := fmt.Sprintf("%d thoughts", thoughtCount)
		if thoughtCount == 1 {
			label = "1 thought"
		}
		if ic != "" {
			label = ic + " " + label
		}
		parts = append(parts, t.Fg(t.Thinking, label))
	}

	sep := t.Fg(t.Muted, " · ")
	return ClipToWidth(" "+strings.Join(parts, sep), width, "…")
}

func renderThinkingStep(st *TurnStep, width int, t Theme, cfg UIConfig, running bool, spinnerGlyph string) []string {
	ic := ThinkingIcon(cfg.Icons)
	if st.ThinkOpen {
		label := t.BoldFg(t.Thinking, "thinking")
		if ic != "" {
			label = t.Fg(t.Thinking, ic) + " " + label
		}
		body := wrapStyledLine(t.ItalicFg(t.Muted, strings.TrimSpace(st.Thinking)), width-4)
		for i := range body {
			body[i] = " " + body[i]
		}
		return DrawFrame(body, width, t, t.Thinking, label, "left", 1)
	}
	// 折叠单行：截取最新思考文本的一行摘要
	flat := strings.Join(strings.Fields(st.Thinking), " ")
	glyph := ic
	if running && spinnerGlyph != "" {
		glyph = spinnerGlyph
	}
	head := t.Fg(t.Thinking, "thinking")
	if glyph != "" {
		head = t.Fg(t.Thinking, glyph) + " " + head
	}
	budget := max(width-VisibleWidth(head)-4, 8)
	preview := ClipToWidth(flat, budget, "…")
	return []string{" " + head + "  " + t.ItalicFg(t.Muted, preview)}
}

func (tr *Transcript) renderToolGroup(turnIdx, stepIdx int, g *ToolGroup, baseLine int, width int, t Theme, cfg UIConfig, spinnerGlyph, outerRail string) []string {
	var out []string
	glyph := "▸"
	if g.Open {
		glyph = "▾"
	}

	var failedCount, runningCount int
	var totalDur time.Duration
	var order []string
	counts := make(map[string]int)
	for _, tc := range g.Tools {
		if tc.Pending {
			runningCount++
		} else if tc.IsError {
			failedCount++
		}
		totalDur += tc.Duration
		if counts[tc.ToolName] == 0 {
			order = append(order, tc.ToolName)
		}
		counts[tc.ToolName]++
	}

	callWord := "calls"
	if len(g.Tools) == 1 {
		callWord = "call"
	}
	var parts []string
	parts = append(parts, t.Fg(t.Accent, glyph)+" "+t.Fg(t.Accent, fmt.Sprintf("%d %s", len(g.Tools), callWord)))
	var bd []string
	for _, name := range order {
		ic := ToolKindIcon(name, cfg.Icons)
		if ic != "" {
			bd = append(bd, t.Fg(t.Muted, fmt.Sprintf("%s %s×%d", ic, name, counts[name])))
		} else {
			bd = append(bd, t.Fg(t.Muted, fmt.Sprintf("%s×%d", name, counts[name])))
		}
	}
	if len(bd) > 0 {
		parts = append(parts, strings.Join(bd, "  "))
	}
	if failedCount > 0 {
		color := t.Error
		if g.Open {
			color = t.Muted
		}
		parts = append(parts, t.Fg(color, fmt.Sprintf("%d failed", failedCount)))
	}
	if runningCount > 0 {
		sp := spinnerGlyph
		if sp == "" {
			sp = "◐"
		}
		parts = append(parts, t.Fg(t.Warning, fmt.Sprintf("%s %d running", sp, runningCount)))
	} else if totalDur > 0 {
		parts = append(parts, t.Fg(t.Muted, formatShortDuration(totalDur)))
	}

	headerLine := ClipToWidth(" "+strings.Join(parts, t.Fg(t.Muted, " · ")), width, "…")
	tr.HitRegions = append(tr.HitRegions, HitRegion{
		StartLine: baseLine,
		EndLine:   baseLine,
		Kind:      HitGroup,
		TurnIdx:   turnIdx,
		StepIdx:   stepIdx,
	})
	out = append(out, outerRail+headerLine)

	gutter := " " + t.Fg(t.BorderMuted, "│") + " "
	for toolIdx, tc := range g.Tools {
		if !g.Open && !tc.Pending {
			continue
		}
		rowStart := baseLine + len(out)
		row := formatToolRow(tc, width-3, t, cfg, spinnerGlyph)
		out = append(out, outerRail+gutter+row)
		if tc.Expanded {
			boxLines := tr.renderToolBox(tc, width-4, t, cfg, spinnerGlyph)
			for _, bl := range boxLines {
				out = append(out, outerRail+gutter+bl)
			}
		}
		tr.HitRegions = append(tr.HitRegions, HitRegion{
			StartLine: rowStart,
			EndLine:   baseLine + len(out) - 1,
			Kind:      HitTool,
			TurnIdx:   turnIdx,
			StepIdx:   stepIdx,
			ToolIdx:   toolIdx,
		})
	}
	return out
}

func formatToolRow(tc *ToolCallItem, width int, t Theme, cfg UIConfig, spinnerGlyph string) string {
	var statusGlyph string
	switch {
	case tc.Pending:
		sp := spinnerGlyph
		if sp == "" {
			sp = "⋯"
		}
		statusGlyph = t.Fg(t.Warning, sp)
	case tc.IsError:
		statusGlyph = t.Fg(t.Error, "✗")
	default:
		statusGlyph = t.Fg(t.Success, "✓")
	}

	kindIcon := ToolKindIcon(tc.ToolName, cfg.Icons)
	namePart := t.BoldFg(t.ToolBorderColor(tc.ToolName, tc.Pending, tc.IsError), tc.ToolName)
	if kindIcon != "" {
		namePart = t.Fg(t.ToolBorderColor(tc.ToolName, tc.Pending, tc.IsError), kindIcon) + " " + namePart
	}

	argSummary := extractToolArgSummary(tc.ToolName, tc.ArgsRaw)
	if tc.ToolName == "bash" && cfg.HighlightBash && argSummary != "" {
		argSummary = HighlightBashCommand(argSummary, t)
	} else if argSummary != "" {
		argSummary = t.Fg(t.Text, argSummary)
	}

	var tailParts []string
	if !tc.Pending {
		if note := summarizeToolResult(tc.Result, tc.IsError); note != "" {
			if tc.IsError {
				tailParts = append(tailParts, t.Fg(t.Error, note))
			} else {
				tailParts = append(tailParts, t.Fg(t.Muted, note))
			}
		}
		if tc.Duration > 0 {
			tailParts = append(tailParts, t.Fg(t.Muted, formatShortDuration(tc.Duration)))
		}
	} else {
		tailParts = append(tailParts, t.Fg(t.Warning, formatShortDuration(time.Since(tc.StartTime))))
	}
	tail := strings.Join(tailParts, t.Fg(t.Muted, " · "))

	left := statusGlyph + "  " + namePart
	if argSummary != "" {
		left += " " + argSummary
	}
	if tail == "" {
		return ClipToWidth(left, width, "…")
	}
	tailW := VisibleWidth(tail)
	availLeft := width - tailW - 2
	if availLeft < 10 {
		return ClipToWidth(left+"  "+tail, width, "…")
	}
	clippedLeft := ClipToWidth(left, availLeft, "…")
	gap := max(width-VisibleWidth(clippedLeft)-tailW, 1)
	return clippedLeft + strings.Repeat(" ", gap) + tail
}

func (tr *Transcript) renderToolBox(tc *ToolCallItem, width int, t Theme, cfg UIConfig, spinnerGlyph string) []string {
	if width < 16 {
		width = 16
	}
	borderColor := t.ToolBorderColor(tc.ToolName, tc.Pending, tc.IsError)
	kindIcon := ToolKindIcon(tc.ToolName, cfg.Icons)
	argsMap := parseArgsMap(tc.ArgsRaw)
	filePath, _ := argsMap["path"].(string)
	if filePath == "" {
		filePath, _ = argsMap["file_path"].(string)
	}

	var labelParts []string
	if kindIcon != "" {
		labelParts = append(labelParts, t.Fg(borderColor, kindIcon))
	}
	labelParts = append(labelParts, t.BoldFg(borderColor, tc.ToolName))
	if filePath != "" {
		if fi := FileIcon(filePath, cfg.Icons); fi != "" {
			labelParts = append(labelParts, fi)
		}
		labelParts = append(labelParts, t.Fg(t.Text, filePath))
		if tc.ToolName == "edit" || tc.ToolName == "write" {
			labelParts = append(labelParts, t.Fg(t.Muted, CopyChip(cfg.Icons)))
		}
	}
	if tc.Pending {
		sp := spinnerGlyph
		if sp == "" {
			sp = "⋯"
		}
		labelParts = append(labelParts, t.Fg(t.Warning, sp))
	} else if tc.Duration > 0 {
		labelParts = append(labelParts, t.ItalicFg(t.Dim, formatShortDuration(tc.Duration)))
	}
	topLabel := strings.Join(labelParts, " ")

	innerW := width - 4
	var body []string

	switch tc.ToolName {
	case "edit":
		oldStr, _ := argsMap["old_string"].(string)
		newStr, _ := argsMap["new_string"].(string)
		if oldStr == "" {
			oldStr, _ = argsMap["oldText"].(string)
		}
		if newStr == "" {
			newStr, _ = argsMap["newText"].(string)
		}
		if oldStr != "" || newStr != "" {
			maxDiff := cfg.DiffLines
			if tc.Expanded {
				maxDiff = 0
			}
			body = RenderSplitDiff(oldStr, newStr, innerW, maxDiff, cfg.SplitDiff, cfg.SplitDiffMinWidth, t)
		} else {
			body = renderToolResultBody(tc.Result, innerW, 10, tc.Expanded, t)
		}

	case "write":
		content, _ := argsMap["content"].(string)
		if content != "" {
			maxW := cfg.WriteLines
			if tc.Expanded {
				maxW = 0
			}
			body = renderWritePreview(content, innerW, maxW, t)
		} else {
			body = renderToolResultBody(tc.Result, innerW, 10, tc.Expanded, t)
		}

	case "bash":
		cmdStr, _ := argsMap["command"].(string)
		if cmdStr != "" {
			if cfg.HighlightBash {
				body = append(body, "$ "+HighlightBashCommand(ClipToWidth(cmdStr, innerW-2, "…"), t))
			} else {
				body = append(body, t.Fg(t.Text, "$ "+ClipToWidth(cmdStr, innerW-2, "…")))
			}
		}
		if tc.Result != "" {
			maxR := cfg.BashResultLines
			if tc.Expanded || tc.IsError {
				maxR = 0
			}
			body = append(body, renderToolResultBody(tc.Result, innerW, maxR, tc.Expanded || tc.IsError, t)...)
		}

	case "subagent":
		body = tr.renderSubagentFlowBody(tc, argsMap, innerW, t, spinnerGlyph)

	default:
		if arg := extractToolArgSummary(tc.ToolName, tc.ArgsRaw); arg != "" {
			body = append(body, t.Fg(t.Muted, ClipToWidth(arg, innerW, "…")))
		}
		if tc.Result != "" {
			maxR := 5
			if tc.Expanded {
				maxR = 0
			}
			body = append(body, renderToolResultBody(tc.Result, innerW, maxR, tc.Expanded, t)...)
		}
	}

	if len(body) == 0 {
		if tc.Pending {
			body = []string{t.Fg(t.Muted, "running...")}
		} else {
			body = []string{t.Fg(t.Muted, "(no output)")}
		}
	}
	for i := range body {
		body[i] = " " + body[i]
	}
	return DrawFrame(body, width, t, borderColor, topLabel, "left", 1)
}

func (tr *Transcript) renderSubagentFlowBody(tc *ToolCallItem, argsMap map[string]any, width int, t Theme, spinnerGlyph string) []string {
	var out []string
	rawTasks, _ := argsMap["tasks"].([]any)
	type taskSpec struct {
		role string
		task string
	}
	var specs []taskSpec
	for _, rt := range rawTasks {
		if m, ok := rt.(map[string]any); ok {
			ag, _ := m["agent"].(string)
			tk, _ := m["task"].(string)
			specs = append(specs, taskSpec{role: ag, task: tk})
		}
	}

	// 将实时 Flows 匹配到本次 subagent 调用的任务（按角色与时间顺序）
	used := make(map[int]bool)
	for _, sp := range specs {
		var matched *AgentFlowEntry
		for i, fl := range tr.Flows {
			if used[i] || fl.Role != sp.role {
				continue
			}
			if !tc.StartTime.IsZero() && fl.StartTime.Before(tc.StartTime.Add(-200*time.Millisecond)) {
				continue
			}
			matched = fl
			used[i] = true
			break
		}

		statusGlyph := t.Fg(t.Warning, spinnerGlyph)
		durStr := ""
		if matched != nil {
			if matched.Pending {
				if spinnerGlyph == "" {
					statusGlyph = t.Fg(t.Warning, "⋯")
				}
				durStr = formatShortDuration(time.Since(matched.StartTime))
			} else if matched.IsError {
				statusGlyph = t.Fg(t.Error, "✗")
				durStr = formatShortDuration(matched.Duration)
			} else {
				statusGlyph = t.Fg(t.Success, "✓")
				durStr = formatShortDuration(matched.Duration)
			}
		} else if !tc.Pending {
			statusGlyph = t.Fg(t.Success, "✓")
		}

		parentLabel := PrimaryAgentID
		if matched != nil && matched.ParentID != "" {
			parentLabel = matched.ParentID
		}
		head := fmt.Sprintf("%s %s %s",
			statusGlyph,
			t.Fg(t.Muted, parentLabel+" ──▶"),
			t.BoldFg(t.Accent, sp.role),
		)
		if durStr != "" {
			head += " " + t.Fg(t.Muted, durStr)
		}
		out = append(out, ClipToWidth(head, width, "…"))

		inFlat := strings.Join(strings.Fields(sp.task), " ")
		if inFlat != "" {
			out = append(out, t.Fg(t.Muted, "  in : ")+t.Fg(t.Dim, ClipToWidth(inFlat, width-7, "…")))
		}
		if matched != nil && strings.TrimSpace(matched.ResultOut) != "" {
			outFlat := strings.Join(strings.Fields(matched.ResultOut), " ")
			out = append(out, t.Fg(t.Success, "  out: ")+t.Fg(t.Text, ClipToWidth(outFlat, width-7, "…")))
		} else if matched != nil && matched.Pending {
			out = append(out, t.Fg(t.Warning, "  out: ")+t.ItalicFg(t.Dim, "streaming… (Tab to inspect)"))
		}
	}

	if len(out) == 0 && tc.Result != "" {
		return renderToolResultBody(tc.Result, width, 10, tc.Expanded, t)
	}
	return out
}

func renderWritePreview(content string, width, maxLines int, t Theme) []string {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	total := len(lines)
	shown := lines
	more := 0
	if maxLines > 0 && total > maxLines {
		shown = lines[:maxLines]
		more = total - maxLines
	}
	out := make([]string, 0, len(shown)+1)
	for i, l := range shown {
		num := t.Fg(t.Dim, fmt.Sprintf("%3d ", i+1))
		out = append(out, num+t.Fg(t.Text, ClipToWidth(strings.ReplaceAll(l, "\t", "    "), width-5, "…")))
	}
	if more > 0 {
		out = append(out, t.Fg(t.Muted, fmt.Sprintf("(+%d more lines, ctrl+o to expand)", more)))
	}
	return out
}

func renderToolResultBody(result string, width, maxLines int, expanded bool, t Theme) []string {
	if result == "" {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(result, "\r\n", "\n"), "\n")
	total := len(lines)
	shown := lines
	more := 0
	if !expanded && maxLines > 0 && total > maxLines {
		shown = lines[:maxLines]
		more = total - maxLines
	}
	out := make([]string, 0, len(shown)+1)
	for _, l := range shown {
		out = append(out, t.Fg(t.Muted, ClipToWidth(strings.ReplaceAll(l, "\t", "    "), width, "…")))
	}
	if more > 0 {
		out = append(out, t.Fg(t.Muted, fmt.Sprintf("(+%d more lines, ctrl+o to expand)", more)))
	}
	return out
}

func parseArgsMap(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(raw), &m)
	return m
}

func extractToolArgSummary(toolName, raw string) string {
	m := parseArgsMap(raw)
	if len(m) == 0 {
		return ""
	}
	switch toolName {
	case "bash":
		if cmd, ok := m["command"].(string); ok {
			return strings.Join(strings.Fields(cmd), " ")
		}
	case "read", "edit", "write":
		if p, ok := m["path"].(string); ok {
			return p
		}
		if p, ok := m["file_path"].(string); ok {
			return p
		}
	case "grep", "find":
		if q, ok := m["pattern"].(string); ok {
			return q
		}
		if q, ok := m["query"].(string); ok {
			return q
		}
	case "subagent":
		if tasks, ok := m["tasks"].([]any); ok && len(tasks) > 0 {
			var roles []string
			for _, rt := range tasks {
				if tm, ok := rt.(map[string]any); ok {
					if ag, ok := tm["agent"].(string); ok && ag != "" {
						roles = append(roles, ag)
					}
				}
			}
			if len(roles) > 0 {
				return fmt.Sprintf("%d tasks: %s", len(roles), strings.Join(roles, ", "))
			}
			return fmt.Sprintf("%d tasks", len(tasks))
		}
	}
	for _, v := range m {
		if s, ok := v.(string); ok && s != "" {
			return strings.Join(strings.Fields(s), " ")
		}
	}
	return ""
}

func summarizeToolResult(res string, isErr bool) string {
	trimmed := strings.TrimSpace(res)
	if trimmed == "" {
		if isErr {
			return "failed"
		}
		return ""
	}
	if isErr {
		firstLine, _, _ := strings.Cut(trimmed, "\n")
		return ClipToWidth(firstLine, 24, "…")
	}
	lines := strings.Count(trimmed, "\n") + 1
	if lines == 1 {
		return "1 line"
	}
	return fmt.Sprintf("%d lines", lines)
}

func formatShortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	mins := int(d.Minutes())
	secs := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm%02ds", mins, secs)
}
