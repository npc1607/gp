package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	styleTrayHeader = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	styleTrayRole   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	styleTrayDesc   = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	styleTrayTime   = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	styleTrayMuted  = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
)

// ActiveCoroutineItem 描述单个活跃协程的运行状态
type ActiveCoroutineItem struct {
	AgentID  string
	Role     string
	TaskDesc string
	Elapsed  time.Duration
}

// ActiveTrayModel 屏幕底端悬浮渲染活跃子协程状态摘要
type ActiveTrayModel struct {
	coroutines map[string]*ActiveCoroutineItem
	order      []string
	spinner    spinner.Model
}

// NewActiveTrayModel 创建动态任务托盘模型
func NewActiveTrayModel() *ActiveTrayModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	return &ActiveTrayModel{
		coroutines: make(map[string]*ActiveCoroutineItem),
		spinner:    sp,
	}
}

// UpdateCoroutine 添加、更新或移除协程状态
func (m *ActiveTrayModel) UpdateCoroutine(msg ActiveCoroutineMsg) {
	if msg.Active {
		item, exists := m.coroutines[msg.AgentID]
		if !exists {
			item = &ActiveCoroutineItem{AgentID: msg.AgentID}
			m.coroutines[msg.AgentID] = item
			m.order = append(m.order, msg.AgentID)
		}
		if msg.Role != "" {
			item.Role = msg.Role
		}
		if msg.TaskDesc != "" {
			item.TaskDesc = msg.TaskDesc
		}
		if msg.Elapsed > 0 {
			item.Elapsed = msg.Elapsed
		}
	} else {
		delete(m.coroutines, msg.AgentID)
		newOrder := make([]string, 0, len(m.order))
		for _, id := range m.order {
			if id != msg.AgentID {
				newOrder = append(newOrder, id)
			}
		}
		m.order = newOrder
	}
}

// UpdateSpinner 驱动 Spinner 逐帧旋转
func (m *ActiveTrayModel) UpdateSpinner(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	m.spinner, cmd = m.spinner.Update(msg)
	return cmd
}

// Render 渲染多任务动态悬浮托盘
func (m *ActiveTrayModel) Render(width int) string {
	if width < 1 {
		width = 1
	}

	var lines []string
	lines = append(lines, styleTrayHeader.Render("⚡ 动态任务泳道 (Active Coroutine Tray):"))

	if len(m.order) == 0 {
		lines = append(lines, styleTrayMuted.Render("  (暂无活跃子协程 / No active background coroutines)"))
	} else {
		spView := m.spinner.View()
		if spView == "" {
			spView = "⠋"
		}
		const maxVisible = 4
		visibleCount := 0
		for _, id := range m.order {
			if visibleCount >= maxVisible {
				remaining := len(m.order) - maxVisible
				lines = append(lines, styleTrayMuted.Render(fmt.Sprintf("  ... (另有 %d 个子任务在后台运行)", remaining)))
				break
			}
			item := m.coroutines[id]
			role := item.Role
			if role == "" {
				role = item.AgentID
			}
			elapsedStr := fmt.Sprintf("%.1fs", item.Elapsed.Seconds())
			line := fmt.Sprintf("  %s %s %s %s",
				spView,
				styleTrayRole.Render(fmt.Sprintf("[%s]", role)),
				styleTrayDesc.Render(item.TaskDesc),
				styleTrayTime.Render(elapsedStr),
			)
			lines = append(lines, line)
			visibleCount++
		}
	}

	content := strings.Join(lines, "\n")
	return lipgloss.NewStyle().MaxWidth(width).Render(content)
}
