package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FooterState 保存 Starship 风格底部状态栏所需的真实上下文信息
type FooterState struct {
	mu            sync.RWMutex
	refreshing    atomic.Bool
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

// RefreshGit 异步刷新当前目录的 Git 分支与工作区脏状态（带 3 秒节流，绝不阻塞 UI 线程）
func (f *FooterState) RefreshGit() {
	f.mu.RLock()
	last := f.LastGitCheck
	cwd := f.Cwd
	f.mu.RUnlock()

	if !last.IsZero() && time.Since(last) < 3*time.Second {
		return
	}
	if cwd == "" || !f.refreshing.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer f.refreshing.Store(false)
		branch := ""
		dirty := false
		if out, err := exec.Command("git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
			branch = strings.TrimSpace(string(out))
			if stOut, stErr := exec.Command("git", "-C", cwd, "status", "--porcelain", "-uno").Output(); stErr == nil {
				dirty = len(strings.TrimSpace(string(stOut))) > 0
			}
		}
		f.mu.Lock()
		f.GitBranch = branch
		f.GitDirty = dirty
		f.LastGitCheck = time.Now()
		f.mu.Unlock()
	}()
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

	f.mu.RLock()
	gitBranch := f.GitBranch
	gitDirty := f.GitDirty
	f.mu.RUnlock()

	if gitBranch != "" {
		branchGlyph := "git:"
		if cfg.Icons {
			branchGlyph = " "
		}
		branchPart := t.Fg(t.Muted, "on ") + t.BoldFg(t.SyntaxKeyword, branchGlyph+gitBranch)
		if gitDirty {
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
