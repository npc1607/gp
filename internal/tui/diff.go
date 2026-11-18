package tui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type diffRow struct {
	kind    string // "ctx" | "change" | "skip"
	oldNum  int
	oldText string
	oldChg  bool
	newNum  int
	newText string
	newChg  bool
}

// ComputeLineDiff 计算 oldText 与 newText 的行级 LCS diff 并对齐为并排视图行
func ComputeLineDiff(oldText, newText string) []diffRow {
	oldLines := strings.Split(strings.ReplaceAll(oldText, "\r\n", "\n"), "\n")
	newLines := strings.Split(strings.ReplaceAll(newText, "\r\n", "\n"), "\n")

	m, n := len(oldLines), len(newLines)
	// 对于超大文本做简单保护，避免 O(N^2) 内存爆炸
	if m*n > 250000 {
		var rows []diffRow
		maxLen := max(n, m)
		for i := range maxLen {
			var o, nw string
			var oNum, nNum int
			if i < m {
				o = oldLines[i]
				oNum = i + 1
			}
			if i < n {
				nw = newLines[i]
				nNum = i + 1
			}
			rows = append(rows, diffRow{
				kind:    "change",
				oldNum:  oNum,
				oldText: o,
				oldChg:  i < m && (i >= n || o != nw),
				newNum:  nNum,
				newText: nw,
				newChg:  i < n && (i >= m || o != nw),
			})
		}
		return rows
	}

	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	type op struct {
		kind byte // '=' | '-' | '+'
		num  int
		text string
	}
	var ops []op
	i, j := 0, 0
	for i < m && j < n {
		if oldLines[i] == newLines[j] {
			ops = append(ops, op{'=', i + 1, oldLines[i]})
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			ops = append(ops, op{'-', i + 1, oldLines[i]})
			i++
		} else {
			ops = append(ops, op{'+', j + 1, newLines[j]})
			j++
		}
	}
	for i < m {
		ops = append(ops, op{'-', i + 1, oldLines[i]})
		i++
	}
	for j < n {
		ops = append(ops, op{'+', j + 1, newLines[j]})
		j++
	}

	var rows []diffRow
	newCursor := 1
	k := 0
	for k < len(ops) {
		if ops[k].kind == '=' {
			rows = append(rows, diffRow{
				kind:    "ctx",
				oldNum:  ops[k].num,
				oldText: ops[k].text,
				newNum:  newCursor,
				newText: ops[k].text,
			})
			newCursor++
			k++
			continue
		}
		var removed, added []op
		for k < len(ops) && ops[k].kind == '-' {
			removed = append(removed, ops[k])
			k++
		}
		for k < len(ops) && ops[k].kind == '+' {
			added = append(added, ops[k])
			k++
		}
		pairs := max(len(added), len(removed))
		for p := range pairs {
			var r diffRow
			r.kind = "change"
			if p < len(removed) {
				r.oldNum = removed[p].num
				r.oldText = removed[p].text
				r.oldChg = true
			}
			if p < len(added) {
				r.newNum = added[p].num
				r.newText = added[p].text
				r.newChg = true
				newCursor = added[p].num + 1
			}
			rows = append(rows, r)
		}
	}
	return rows
}

// RenderSplitDiff 渲染 pi-tidy 风格的并排双栏 diff (old │ new) 或窄屏回退统一 diff
func RenderSplitDiff(oldText, newText string, width, maxLines int, split bool, splitMinWidth int, t Theme) []string {
	rows := ComputeLineDiff(oldText, newText)
	if len(rows) == 0 {
		return []string{t.Fg(t.Muted, " (no changes)")}
	}

	// 定位首个变更行，使折叠窗口锚定在改动附近
	firstChange := -1
	for i, r := range rows {
		if r.oldChg || r.newChg {
			firstChange = i
			break
		}
	}

	totalRows := len(rows)
	truncatedMore := 0
	if maxLines > 0 && len(rows) > maxLines {
		start := 0
		if firstChange >= 0 {
			start = max(firstChange-maxLines/3, 0)
			if start+maxLines > len(rows) {
				start = len(rows) - maxLines
			}
		}
		rows = rows[start : start+maxLines]
		truncatedMore = totalRows - maxLines
	}

	if !split || width < splitMinWidth {
		return renderUnifiedDiffRows(rows, width, truncatedMore, totalRows, t)
	}

	// 并排双栏: `│ ` / `┃ ` + num + ` ` + code + ` │ ` + `│ ` / `┃ ` + num + ` ` + code
	sep := t.Fg(t.BorderMuted, " │ ")
	sepW := 3
	paneW := (width - sepW) / 2
	if paneW < 12 {
		return renderUnifiedDiffRows(rows, width, truncatedMore, totalRows, t)
	}
	rightPaneW := width - sepW - paneW

	maxNum := 1
	for _, r := range rows {
		if r.oldNum > maxNum {
			maxNum = r.oldNum
		}
		if r.newNum > maxNum {
			maxNum = r.newNum
		}
	}
	numW := max(len(fmt.Sprintf("%d", maxNum)), 2)

	out := make([]string, 0, len(rows)+1)
	for _, r := range rows {
		leftCell := renderDiffPaneCell(r.oldNum, r.oldText, r.oldChg, false, paneW, numW, t)
		rightCell := renderDiffPaneCell(r.newNum, r.newText, r.newChg, true, rightPaneW, numW, t)
		out = append(out, leftCell+sep+rightCell)
	}
	if truncatedMore > 0 {
		out = append(out, t.Fg(t.Muted, fmt.Sprintf(" … (%d more lines, %d total, ctrl+o to expand)", truncatedMore, totalRows)))
	}
	return out
}

func renderDiffPaneCell(num int, text string, changed, isNew bool, paneW, numW int, t Theme) string {
	text = strings.ReplaceAll(text, "\t", "    ")
	numStr := strings.Repeat(" ", numW)
	if num > 0 {
		numStr = fmt.Sprintf("%*d", numW, num)
	}

	var bar string
	var bg lipgloss.Color
	var fg lipgloss.Color
	if changed {
		if isNew {
			bar = lipgloss.NewStyle().Foreground(t.AddedFg).Background(t.AddedBg).Bold(true).Render("┃")
			bg = t.AddedBg
			fg = t.AddedFg
		} else {
			bar = lipgloss.NewStyle().Foreground(t.RemovedFg).Background(t.RemovedBg).Bold(true).Render("┃")
			bg = t.RemovedBg
			fg = t.RemovedFg
		}
	} else {
		bar = t.Fg(t.BorderMuted, " ")
		fg = t.Muted
	}

	prefixW := 2 + numW + 1 // bar + space + num + space
	codeW := max(paneW-prefixW, 1)
	clipped := ClipToWidth(text, codeW, "…")
	padLen := max(codeW-VisibleWidth(clipped), 0)
	body := fmt.Sprintf(" %s %s%s", numStr, clipped, strings.Repeat(" ", padLen))
	if changed {
		return bar + lipgloss.NewStyle().Foreground(fg).Background(bg).Render(body)
	}
	return bar + t.Fg(fg, body)
}

func renderUnifiedDiffRows(rows []diffRow, width, truncatedMore, totalRows int, t Theme) []string {
	var out []string
	for _, r := range rows {
		if !r.oldChg && !r.newChg {
			line := fmt.Sprintf("  %3d %s", r.oldNum, strings.ReplaceAll(r.oldText, "\t", "    "))
			out = append(out, t.Fg(t.Muted, ClipToWidth(line, width, "…")))
			continue
		}
		if r.oldChg {
			line := fmt.Sprintf("- %3d %s", r.oldNum, strings.ReplaceAll(r.oldText, "\t", "    "))
			out = append(out, lipgloss.NewStyle().Foreground(t.RemovedFg).Background(t.RemovedBg).Render(PadToWidth(ClipToWidth(line, width, "…"), width)))
		}
		if r.newChg {
			line := fmt.Sprintf("+ %3d %s", r.newNum, strings.ReplaceAll(r.newText, "\t", "    "))
			out = append(out, lipgloss.NewStyle().Foreground(t.AddedFg).Background(t.AddedBg).Render(PadToWidth(ClipToWidth(line, width, "…"), width)))
		}
	}
	if truncatedMore > 0 {
		out = append(out, t.Fg(t.Muted, fmt.Sprintf(" … (%d more lines, %d total, ctrl+o to expand)", truncatedMore, totalRows)))
	}
	return out
}

// ── Bash Syntax Highlighting ────────────────────────────────────────────────

var bashOperators = map[string]bool{
	"&&": true, "||": true, "|": true, ";": true, ">": true, ">>": true, "<": true, "2>&1": true, "&": true,
}

// HighlightBashCommand 对 bash 命令行做轻量词法高亮（命令、参数 flag、字符串、变量、操作符）
func HighlightBashCommand(cmd string, t Theme) string {
	if cmd == "" {
		return ""
	}
	var out strings.Builder
	i := 0
	expectCmd := true
	n := len(cmd)

	for i < n {
		ch := cmd[i]
		// 空白
		if ch == ' ' || ch == '\t' {
			out.WriteByte(ch)
			i++
			continue
		}
		// 注释
		if ch == '#' && (i == 0 || cmd[i-1] == ' ') {
			out.WriteString(t.Fg(t.SyntaxComment, cmd[i:]))
			break
		}
		// 引号字符串
		if ch == '"' || ch == '\'' {
			quote := ch
			j := i + 1
			for j < n {
				if cmd[j] == '\\' && quote == '"' && j+1 < n {
					j += 2
					continue
				}
				if cmd[j] == quote {
					j++
					break
				}
				j++
			}
			out.WriteString(t.Fg(t.SyntaxString, cmd[i:j]))
			expectCmd = false
			i = j
			continue
		}
		// 读取连续 token
		j := i
		for j < n && cmd[j] != ' ' && cmd[j] != '\t' && cmd[j] != '"' && cmd[j] != '\'' {
			j++
		}
		tok := cmd[i:j]
		switch {
		case bashOperators[tok]:
			out.WriteString(t.Fg(t.SyntaxOperator, tok))
			expectCmd = true
		case strings.HasPrefix(tok, "$"):
			out.WriteString(t.Fg(t.SyntaxVariable, tok))
			expectCmd = false
		case expectCmd:
			if tok == "sudo" || tok == "env" || tok == "time" {
				out.WriteString(t.Fg(t.SyntaxKeyword, tok))
			} else {
				out.WriteString(t.BoldFg(t.SyntaxFunction, tok))
				expectCmd = false
			}
		case strings.HasPrefix(tok, "-"):
			out.WriteString(t.Fg(t.SyntaxKeyword, tok))
		case strings.Contains(tok, "="):
			parts := strings.SplitN(tok, "=", 2)
			out.WriteString(t.Fg(t.SyntaxVariable, parts[0]) + t.Fg(t.SyntaxOperator, "=") + t.Fg(t.Text, parts[1]))
		default:
			out.WriteString(t.Fg(t.Text, tok))
		}
		i = j
	}
	return out.String()
}

// ── Hex Color Swatches ──────────────────────────────────────────────────────

var hexColorRe = regexp.MustCompile(`#[0-9a-fA-F]{6}\b`)

// RenderHexSwatches 在正文中的 #rrggbb 颜色前添加对应颜色的 ● 指示点
func RenderHexSwatches(line string) string {
	return hexColorRe.ReplaceAllStringFunc(line, func(hex string) string {
		dot := lipgloss.NewStyle().Foreground(lipgloss.Color(hex)).Render("●")
		return dot + " " + hex
	})
}
