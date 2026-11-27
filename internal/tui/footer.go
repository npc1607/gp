package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	StatusSideLeft  = "left"
	StatusSideRight = "right"
)

// FooterSnapshot 为 StatusSegment.Render 提供无锁只读的底部状态栏完整快照
type FooterSnapshot struct {
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

	// 系统内部上下文引擎指标（由 ContextMetricsMsg / CompactionDoneMsg 实时更新）
	MetricsReady         bool
	TurnIndex            int
	StrategyMode         string
	EpochID              int
	EpochMaskedCount     int
	LedgerVersion        int
	LedgerActiveCnt      int
	LedgerSuperseded     int
	WorkingSetTokens     int64
	WorkingSetEntries    int
	GraphMatchedEntities int
	ContextBuildMs       float64
	RetrievalBuildMs     float64
	LastInputTokens      int64
	CacheReadTokens      int64
	CacheWriteTokens     int64
	TotalRecallCalls     int
	TotalRestoreCalls    int
	TotalSearchCalls     int
	CompactionCount      int
}

// StatusSegment 定义可注册到底部 Statusline 的独立指标段
type StatusSegment struct {
	ID          string
	Description string
	Side        string // StatusSideLeft | StatusSideRight
	Priority    int    // 越高越重要；终端宽度不足时按优先级从低到高自动隐藏
	Order       int    // 同侧展示顺序（越小越靠左）
	Enabled     bool
	Render      func(snap FooterSnapshot, t Theme, cfg UIConfig) string
}

// FooterState 保存 Starship 风格底部状态栏所需的真实上下文信息与可注册 Segment 目录
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

	// 内部上下文引擎实时状态
	MetricsReady         bool
	TurnIndex            int
	StrategyMode         string
	EpochID              int
	EpochMaskedCount     int
	LedgerVersion        int
	LedgerActiveCnt      int
	LedgerSuperseded     int
	WorkingSetTokens     int64
	WorkingSetEntries    int
	GraphMatchedEntities int
	ContextBuildMs       float64
	RetrievalBuildMs     float64
	LastInputTokens      int64
	CacheReadTokens      int64
	CacheWriteTokens     int64
	TotalRecallCalls     int
	TotalRestoreCalls    int
	TotalSearchCalls     int
	CompactionCount      int

	segments []StatusSegment
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
	fs.registerBuiltinSegments()
	fs.RefreshGit()
	return fs
}

// registerBuiltinSegments 注册默认工作区段与系统内部上下文核心指标段
func (f *FooterState) registerBuiltinSegments() {
	builtins := []StatusSegment{
		// ── 左侧：工作区与运行环境 ──────────────────────────────────────
		{
			ID:          "dir",
			Description: "当前工作目录名称",
			Side:        StatusSideLeft,
			Priority:    100,
			Order:       10,
			Enabled:     true,
			Render: func(snap FooterSnapshot, t Theme, _ UIConfig) string {
				dirName := filepath.Base(snap.Cwd)
				if dirName == "" || dirName == "." || dirName == "/" {
					dirName = "gp"
				}
				return t.BoldFg(t.Accent, dirName)
			},
		},
		{
			ID:          "git",
			Description: "Git 分支与工作区脏状态",
			Side:        StatusSideLeft,
			Priority:    85,
			Order:       20,
			Enabled:     true,
			Render: func(snap FooterSnapshot, t Theme, cfg UIConfig) string {
				if snap.GitBranch == "" {
					return ""
				}
				branchGlyph := "git:"
				if cfg.Icons {
					branchGlyph = " "
				}
				part := t.Fg(t.Muted, "on ") + t.BoldFg(t.SyntaxKeyword, branchGlyph+snap.GitBranch)
				if snap.GitDirty {
					part += t.BoldFg(t.Warning, " *")
				}
				return part
			},
		},
		{
			ID:          "runtime",
			Description: "Go 运行时版本",
			Side:        StatusSideLeft,
			Priority:    40,
			Order:       30,
			Enabled:     true,
			Render: func(snap FooterSnapshot, t Theme, cfg UIConfig) string {
				if snap.Runtime == "" {
					return ""
				}
				goGlyph := ""
				if cfg.Icons {
					goGlyph = "\ue627 "
				}
				return t.Fg(t.Muted, "via ") + t.Fg(t.SyntaxOperator, goGlyph+snap.Runtime)
			},
		},

		// ── 右侧：系统内部上下文引擎核心指标 ───────────────────────────
		{
			ID:          "turn",
			Description: "当前会话上下文轮次 (T:N)",
			Side:        StatusSideRight,
			Priority:    68,
			Order:       8,
			Enabled:     true,
			Render: func(snap FooterSnapshot, t Theme, _ UIConfig) string {
				if !snap.MetricsReady || snap.TurnIndex <= 0 {
					return ""
				}
				return t.Fg(t.Muted, fmt.Sprintf("T:%d", snap.TurnIndex))
			},
		},
		{
			ID:          "cache",
			Description: "Prompt Cache 命中率 (cache:xx%)",
			Side:        StatusSideRight,
			Priority:    78,
			Order:       10,
			Enabled:     true,
			Render: func(snap FooterSnapshot, t Theme, _ UIConfig) string {
				if !snap.MetricsReady && snap.CacheReadTokens <= 0 && snap.CacheWriteTokens <= 0 {
					return ""
				}
				denom := snap.LastInputTokens
				if denom <= 0 {
					denom = snap.CacheReadTokens + snap.CacheWriteTokens
				}
				pct := 0.0
				if denom > 0 {
					pct = float64(snap.CacheReadTokens) / float64(denom) * 100
					if pct > 100 {
						pct = 100
					}
				}
				col := t.Dim
				if pct >= 60 {
					col = t.Success
				} else if pct >= 30 {
					col = t.Warning
				} else if snap.CacheReadTokens > 0 {
					col = t.Muted
				}
				return t.Fg(col, fmt.Sprintf("cache:%.0f%%", pct))
			},
		},
		{
			ID:          "epoch",
			Description: "Epoch 观测掩码代际与折叠数 (ep:N(-M))",
			Side:        StatusSideRight,
			Priority:    65,
			Order:       20,
			Enabled:     true,
			Render: func(snap FooterSnapshot, t Theme, _ UIConfig) string {
				if !snap.MetricsReady && snap.EpochID <= 0 && snap.EpochMaskedCount <= 0 {
					return ""
				}
				if snap.EpochMaskedCount > 0 {
					return t.Fg(t.SyntaxOperator, fmt.Sprintf("ep:%d(-%d)", snap.EpochID, snap.EpochMaskedCount))
				}
				return t.Fg(t.Muted, fmt.Sprintf("ep:%d", snap.EpochID))
			},
		},
		{
			ID:          "ledger",
			Description: "Context Ledger 账本版本与活跃条目 (L:vN(M))",
			Side:        StatusSideRight,
			Priority:    60,
			Order:       30,
			Enabled:     true,
			Render: func(snap FooterSnapshot, t Theme, _ UIConfig) string {
				if !snap.MetricsReady && snap.LedgerVersion <= 0 && snap.LedgerActiveCnt <= 0 {
					return ""
				}
				if snap.LedgerActiveCnt > 0 {
					return t.Fg(t.SyntaxKeyword, fmt.Sprintf("L:v%d(%d)", snap.LedgerVersion, snap.LedgerActiveCnt))
				}
				return t.Fg(t.Dim, fmt.Sprintf("L:v%d", snap.LedgerVersion))
			},
		},
		{
			ID:          "workingset",
			Description: "动态工作集注入 Token 规模 (ws:xx)",
			Side:        StatusSideRight,
			Priority:    55,
			Order:       40,
			Enabled:     true,
			Render: func(snap FooterSnapshot, t Theme, _ UIConfig) string {
				if snap.WorkingSetTokens <= 0 {
					return ""
				}
				return t.Fg(t.Accent, "ws:"+FormatTokenCompact(snap.WorkingSetTokens))
			},
		},
		{
			ID:          "recall",
			Description: "跨压缩边界主动召回/检索次数 (recall:N)",
			Side:        StatusSideRight,
			Priority:    50,
			Order:       50,
			Enabled:     true,
			Render: func(snap FooterSnapshot, t Theme, _ UIConfig) string {
				total := snap.TotalRecallCalls + snap.TotalSearchCalls + snap.TotalRestoreCalls
				if total <= 0 {
					return ""
				}
				return t.Fg(t.Warning, fmt.Sprintf("recall:%d", total))
			},
		},
		{
			ID:          "compaction",
			Description: "会话累计上下文压缩次数 (cmp:N)",
			Side:        StatusSideRight,
			Priority:    70,
			Order:       60,
			Enabled:     true,
			Render: func(snap FooterSnapshot, t Theme, _ UIConfig) string {
				if snap.CompactionCount <= 0 {
					return ""
				}
				return t.Fg(t.SyntaxKeyword, fmt.Sprintf("cmp:%d", snap.CompactionCount))
			},
		},
		{
			ID:          "build",
			Description: "最近一轮上下文装配耗时 (build:xxms)",
			Side:        StatusSideRight,
			Priority:    35,
			Order:       70,
			Enabled:     true,
			Render: func(snap FooterSnapshot, t Theme, _ UIConfig) string {
				if snap.ContextBuildMs <= 0 {
					return ""
				}
				if snap.ContextBuildMs < 10 {
					return t.Fg(t.Dim, fmt.Sprintf("build:%.1fms", snap.ContextBuildMs))
				}
				return t.Fg(t.Dim, fmt.Sprintf("build:%.0fms", snap.ContextBuildMs))
			},
		},

		// ── 右侧：经典上下文窗口 / Token / 费用 / 会话时长 ──────────────
		{
			ID:          "context",
			Description: "上下文窗口占用率 (ctx: used/window)",
			Side:        StatusSideRight,
			Priority:    100,
			Order:       80,
			Enabled:     true,
			Render: func(snap FooterSnapshot, t Theme, _ UIConfig) string {
				if snap.ContextWindow <= 0 {
					return ""
				}
				pct := float64(snap.ContextUsed) / float64(snap.ContextWindow) * 100
				ctxStr := fmt.Sprintf("ctx: %s/%s (%.1f%%)",
					FormatTokenCompact(snap.ContextUsed),
					FormatTokenCompact(snap.ContextWindow),
					pct,
				)
				ctxColor := t.Success
				if pct >= 80 {
					ctxColor = t.Error
				} else if pct >= 55 {
					ctxColor = t.Warning
				}
				return t.Fg(ctxColor, ctxStr)
			},
		},
		{
			ID:          "tokens",
			Description: "累计输入/输出 Token (↑in ↓out)",
			Side:        StatusSideRight,
			Priority:    90,
			Order:       90,
			Enabled:     true,
			Render: func(snap FooterSnapshot, t Theme, _ UIConfig) string {
				if snap.InputTokens <= 0 && snap.OutputTokens <= 0 {
					return ""
				}
				return t.Fg(t.Muted, fmt.Sprintf("↑%s ↓%s",
					FormatTokenCompact(snap.InputTokens),
					FormatTokenCompact(snap.OutputTokens),
				))
			},
		},
		{
			ID:          "cost",
			Description: "估算费用 ($0.00)",
			Side:        StatusSideRight,
			Priority:    45,
			Order:       100,
			Enabled:     true,
			Render: func(snap FooterSnapshot, t Theme, _ UIConfig) string {
				return t.Fg(t.Success, fmt.Sprintf("$%.2f", snap.CostUSD))
			},
		},
		{
			ID:          "duration",
			Description: "本次会话持续时长",
			Side:        StatusSideRight,
			Priority:    75,
			Order:       110,
			Enabled:     true,
			Render: func(snap FooterSnapshot, t Theme, _ UIConfig) string {
				if snap.SessionStart.IsZero() {
					return ""
				}
				return t.Fg(t.Dim, formatShortDuration(time.Since(snap.SessionStart)))
			},
		},
	}
	for _, seg := range builtins {
		f.RegisterSegment(seg)
	}
}

// RegisterSegment 注册或覆盖一个 Statusline 指标段（线程安全，供内置与插件扩展调用）
func (f *FooterState) RegisterSegment(seg StatusSegment) {
	if strings.TrimSpace(seg.ID) == "" || seg.Render == nil {
		return
	}
	if seg.Side != StatusSideLeft && seg.Side != StatusSideRight {
		seg.Side = StatusSideRight
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.segments {
		if f.segments[i].ID == seg.ID {
			f.segments[i] = seg
			return
		}
	}
	f.segments = append(f.segments, seg)
}

// UnregisterSegment 按 ID 注销一个 Statusline 指标段
func (f *FooterState) UnregisterSegment(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.segments {
		if f.segments[i].ID == id {
			f.segments = append(f.segments[:i], f.segments[i+1:]...)
			return true
		}
	}
	return false
}

// SetSegmentEnabled 开启或关闭指定 ID 的指标段
func (f *FooterState) SetSegmentEnabled(id string, enabled bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.segments {
		if f.segments[i].ID == id {
			f.segments[i].Enabled = enabled
			return true
		}
	}
	return false
}

// ToggleSegment 切换指定 ID 的指标段开关
func (f *FooterState) ToggleSegment(id string) (enabled bool, found bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.segments {
		if f.segments[i].ID == id {
			f.segments[i].Enabled = !f.segments[i].Enabled
			return f.segments[i].Enabled, true
		}
	}
	return false, false
}

// ResetSegments 恢复全部已注册指标段为默认开启状态
func (f *FooterState) ResetSegments() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.segments {
		f.segments[i].Enabled = true
	}
}

// ListSegments 返回当前已注册的全部指标段副本（按 Side + Order 排序）
func (f *FooterState) ListSegments() []StatusSegment {
	f.mu.RLock()
	out := make([]StatusSegment, len(f.segments))
	copy(out, f.segments)
	f.mu.RUnlock()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Side != out[j].Side {
			return out[i].Side == StatusSideLeft
		}
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ApplyContextMetrics 将 Agent 上下文引擎每轮产出的内部指标合并到 FooterState
func (f *FooterState) ApplyContextMetrics(m ContextMetricsMsg) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.MetricsReady = true
	f.TurnIndex = m.TurnIndex
	if m.StrategyMode != "" {
		f.StrategyMode = m.StrategyMode
	}
	f.EpochID = m.EpochID
	f.EpochMaskedCount = m.EpochMaskedCount
	f.LedgerVersion = m.LedgerVersion
	f.LedgerActiveCnt = m.LedgerActiveCnt
	f.LedgerSuperseded = m.LedgerSuperseded
	f.WorkingSetTokens = m.WorkingSetTokens
	f.WorkingSetEntries = m.WorkingSetEntries
	f.GraphMatchedEntities = m.GraphMatchedEntities
	f.ContextBuildMs = m.ContextBuildMs
	f.RetrievalBuildMs = m.RetrievalBuildMs
	if m.InputTokens > 0 {
		f.LastInputTokens = m.InputTokens
	}
	f.CacheReadTokens = m.CacheReadTokens
	f.CacheWriteTokens = m.CacheWriteTokens
	f.TotalRecallCalls += m.RecallCalls
	f.TotalRestoreCalls += m.RestoreCalls
	f.TotalSearchCalls += m.SearchCalls
}

// RecordCompaction 记录一次成功的上下文压缩事件
func (f *FooterState) RecordCompaction(afterTokens int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CompactionCount++
	if afterTokens >= 0 {
		f.ContextUsed = afterTokens
	}
}

func (f *FooterState) snapshot() (FooterSnapshot, []StatusSegment) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	snap := FooterSnapshot{
		Cwd:                  f.Cwd,
		GitBranch:            f.GitBranch,
		GitDirty:             f.GitDirty,
		Runtime:              f.Runtime,
		ContextUsed:          f.ContextUsed,
		ContextWindow:        f.ContextWindow,
		InputTokens:          f.InputTokens,
		OutputTokens:         f.OutputTokens,
		CostUSD:              f.CostUSD,
		SessionStart:         f.SessionStart,
		MetricsReady:         f.MetricsReady,
		TurnIndex:            f.TurnIndex,
		StrategyMode:         f.StrategyMode,
		EpochID:              f.EpochID,
		EpochMaskedCount:     f.EpochMaskedCount,
		LedgerVersion:        f.LedgerVersion,
		LedgerActiveCnt:      f.LedgerActiveCnt,
		LedgerSuperseded:     f.LedgerSuperseded,
		WorkingSetTokens:     f.WorkingSetTokens,
		WorkingSetEntries:    f.WorkingSetEntries,
		GraphMatchedEntities: f.GraphMatchedEntities,
		ContextBuildMs:       f.ContextBuildMs,
		RetrievalBuildMs:     f.RetrievalBuildMs,
		LastInputTokens:      f.LastInputTokens,
		CacheReadTokens:      f.CacheReadTokens,
		CacheWriteTokens:     f.CacheWriteTokens,
		TotalRecallCalls:     f.TotalRecallCalls,
		TotalRestoreCalls:    f.TotalRestoreCalls,
		TotalSearchCalls:     f.TotalSearchCalls,
		CompactionCount:      f.CompactionCount,
	}
	segs := make([]StatusSegment, len(f.segments))
	copy(segs, f.segments)
	return snap, segs
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

type renderedSegment struct {
	id       string
	side     string
	priority int
	order    int
	text     string
	width    int
	dropped  bool
}

// Render 渲染 pi-zentui Starship 风格底部状态栏（支持按 Priority 自适应宽度降级）
func (f *FooterState) Render(width int, t Theme, cfg UIConfig) string {
	if cfg.FooterStyle == "hidden" {
		return ""
	}
	if width < 20 {
		width = 20
	}

	snap, segs := f.snapshot()
	var active []renderedSegment
	for _, seg := range segs {
		if !seg.Enabled || seg.Render == nil {
			continue
		}
		txt := seg.Render(snap, t, cfg)
		if txt == "" {
			continue
		}
		active = append(active, renderedSegment{
			id:       seg.ID,
			side:     seg.Side,
			priority: seg.Priority,
			order:    seg.Order,
			text:     txt,
			width:    VisibleWidth(txt),
		})
	}

	// 同侧按 Order 排序，保证视觉顺序稳定
	sort.SliceStable(active, func(i, j int) bool {
		if active[i].side != active[j].side {
			return active[i].side == StatusSideLeft
		}
		if active[i].order != active[j].order {
			return active[i].order < active[j].order
		}
		return active[i].id < active[j].id
	})

	rightSep := t.Fg(t.BorderMuted, " │ ")
	rightSepW := VisibleWidth(rightSep)

	totalWidth := func() int {
		leftW, leftCnt := 0, 0
		rightW, rightCnt := 0, 0
		for _, s := range active {
			if s.dropped {
				continue
			}
			if s.side == StatusSideLeft {
				if leftCnt > 0 {
					leftW++
				}
				leftW += s.width
				leftCnt++
			} else {
				if rightCnt > 0 {
					rightW += rightSepW
				}
				rightW += s.width
				rightCnt++
			}
		}
		if leftCnt > 0 && rightCnt > 0 {
			return leftW + 2 + rightW
		}
		return leftW + rightW
	}

	// 宽度不足时，从低优先级到高优先级逐个优雅隐藏非核心指标，避免截断核心 ctx/tokens
	for totalWidth() > width {
		victim := -1
		for i := range active {
			if active[i].dropped {
				continue
			}
			if victim == -1 || active[i].priority < active[victim].priority {
				victim = i
			}
		}
		if victim == -1 {
			break
		}
		active[victim].dropped = true
	}

	var leftParts, rightParts []string
	for _, s := range active {
		if s.dropped {
			continue
		}
		if s.side == StatusSideLeft {
			leftParts = append(leftParts, s.text)
		} else {
			rightParts = append(rightParts, s.text)
		}
	}

	left := strings.Join(leftParts, " ")
	right := strings.Join(rightParts, rightSep)
	return composeLeftRight(left, right, width)
}
