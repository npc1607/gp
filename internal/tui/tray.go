package tui

import (
	"fmt"
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

// Reset 清空上一轮的子协程托盘状态
func (m *ActiveTrayModel) Reset() {
	m.coroutines = make(map[string]*ActiveCoroutineItem)
	m.order = nil
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

// Render 渲染任务进度指示行
func (m *ActiveTrayModel) Render(width int) string {
	if width < 1 {
		width = 1
	}

	if len(m.order) == 0 {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#262626")).Render("  (No active background coroutines)")
	}

	id := m.order[0]
	item := m.coroutines[id]

	desc := item.TaskDesc
	if desc == "" {
		desc = "Simmering..."
	}
	elapsedStr := fmt.Sprintf("%.1fs", item.Elapsed.Seconds())

	star := lipgloss.NewStyle().Foreground(lipgloss.Color("#4ade80")).Bold(true).Render("*")
	var line string
	if item.Role != "" {
		line = fmt.Sprintf(" %s %s %s · %s",
			star,
			lipgloss.NewStyle().Foreground(lipgloss.Color("#38bdf8")).Bold(true).Render(fmt.Sprintf("[%s]", item.Role)),
			lipgloss.NewStyle().Foreground(lipgloss.Color("#737373")).Render(desc),
			lipgloss.NewStyle().Foreground(lipgloss.Color("#737373")).Render(elapsedStr),
		)
	} else {
		line = fmt.Sprintf(" %s %s · %s",
			star,
			lipgloss.NewStyle().Foreground(lipgloss.Color("#737373")).Render(desc),
			lipgloss.NewStyle().Foreground(lipgloss.Color("#737373")).Render(elapsedStr),
		)
	}

	return lipgloss.NewStyle().MaxWidth(width).Render(line)
}
