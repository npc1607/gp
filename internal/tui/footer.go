package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// FooterState 保存 Starship 风格底部状态栏所需的真实上下文信息
type FooterState struct {
	Cwd           string
	GitBranch     string
	GitDirty      bool
	Runtime       string
	ContextUsed   int64
	ContextWindow int64
	InputTokens   int64
	OutputTokens  int64
	CostUSD       float64
	SessionStart  time.Time
	LastGitCheck  time.Time
}

func NewFooterState(cwd string) *FooterState {
	if cwd == "" {
		if d, err := os.Getwd(); err == nil {
			cwd = d
		}
	}
	fs := &FooterState{
		Cwd:           cwd,
		Runtime:       runtime.Version(),
		ContextWindow: 128000,
		SessionStart:  time.Now(),
	}
	fs.RefreshGit()
	return fs
}

// RefreshGit 刷新当前目录的 Git 分支与工作区脏状态（带 3 秒节流保护）
func (f *FooterState) RefreshGit() {
	if !f.LastGitCheck.IsZero() && time.Since(f.LastGitCheck) < 3*time.Second {
		return
	}
	f.LastGitCheck = time.Now()
	if f.Cwd == "" {
		return
	}
	cmd := exec.Command("git", "-C", f.Cwd, "rev-parse", "--abbrev-ref", "HEAD")
	if out, err := cmd.Output(); err == nil {
		f.GitBranch = strings.TrimSpace(string(out))
	} else {
		f.GitBranch = ""
		f.GitDirty = false
		return
	}
	stCmd := exec.Command("git", "-C", f.Cwd, "status", "--porcelain", "-uno")
	if out, err := stCmd.Output(); err == nil {
		f.GitDirty = len(strings.TrimSpace(string(out))) > 0
	}
}

// Render 渲染 pi-zentui Starship 风格底部状态栏
func (f *FooterState) Render(width int, t Theme, cfg UIConfig) string {
	if cfg.FooterStyle == "hidden" {
		return ""
	}
	if width < 20 {
		width = 20
	}

	dirName := filepath.Base(f.Cwd)
	if dirName == "" || dirName == "." || dirName == "/" {
		dirName = "gp"
	}

	var leftParts []string
	leftParts = append(leftParts, t.BoldFg(t.Accent, dirName))

	if f.GitBranch != "" {
		branchGlyph := "git:"
		if cfg.Icons {
			branchGlyph = " "
		}
		branchPart := t.Fg(t.Muted, "on ") + t.BoldFg(t.SyntaxKeyword, branchGlyph+f.GitBranch)
		if f.GitDirty {
			branchPart += t.BoldFg(t.Warning, " *")
		}
		leftParts = append(leftParts, branchPart)
	}

	if f.Runtime != "" {
		goGlyph := ""
		if cfg.Icons {
			goGlyph = "\ue627 "
		}
		leftParts = append(leftParts, t.Fg(t.Muted, "via ")+t.Fg(t.SyntaxOperator, goGlyph+f.Runtime))
	}
	left := strings.Join(leftParts, " ")

	var rightParts []string
	if f.ContextWindow > 0 {
		pct := float64(f.ContextUsed) / float64(f.ContextWindow) * 100
		ctxStr := fmt.Sprintf("ctx: %s/%s (%.1f%%)",
			FormatTokenCompact(f.ContextUsed),
			FormatTokenCompact(f.ContextWindow),
			pct,
		)
		ctxColor := t.Success
		if pct >= 80 {
			ctxColor = t.Error
		} else if pct >= 55 {
			ctxColor = t.Warning
		}
		rightParts = append(rightParts, t.Fg(ctxColor, ctxStr))
	}
	if f.InputTokens > 0 || f.OutputTokens > 0 {
		rightParts = append(rightParts, t.Fg(t.Muted, fmt.Sprintf("↑%s ↓%s",
			FormatTokenCompact(f.InputTokens),
			FormatTokenCompact(f.OutputTokens),
		)))
	}
	rightParts = append(rightParts, t.Fg(t.Success, fmt.Sprintf("$%.2f", f.CostUSD)))
	if !f.SessionStart.IsZero() {
		rightParts = append(rightParts, t.Fg(t.Dim, formatShortDuration(time.Since(f.SessionStart))))
	}
	right := strings.Join(rightParts, t.Fg(t.BorderMuted, " │ "))

	return composeLeftRight(left, right, width)
}
