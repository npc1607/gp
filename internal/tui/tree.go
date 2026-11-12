package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/lipgloss"
)

var (
	styleTitle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("86"))
	styleRunning  = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	styleBlocked  = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	styleRepair   = lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Bold(true)
	styleDone     = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	styleApproval = lipgloss.NewStyle().Foreground(lipgloss.Color("198")).Bold(true)
	styleFailed   = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	styleTreeLine = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

// DAGNodeItem 表示 DAG 树上的单个任务节点
type DAGNodeItem struct {
	ID          string
	Role        string
	Status      string
	Attempt     int
	MaxAttempts int
	DependsOn   []string
	Output      string
	ErrorMsg    string
}

// WorkflowTreeModel 左侧 35% DAG 拓扑树展示组件
type WorkflowTreeModel struct {
	nodes        map[string]*DAGNodeItem
	nodeOrder    []string
	workflowName string
	startTime    time.Time
	spinner      spinner.Model
}

// NewWorkflowTreeModel 创建 DAG 工作流树模型
func NewWorkflowTreeModel() *WorkflowTreeModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	return &WorkflowTreeModel{
		nodes:        make(map[string]*DAGNodeItem),
		workflowName: "DAG 任务拓扑",
		startTime:    time.Now(),
		spinner:      sp,
	}
}

// SetWorkflowName 设置工作流名称
func (m *WorkflowTreeModel) SetWorkflowName(name string) {
	m.workflowName = name
}

// AddOrUpdateNode 插入或更新 DAG 节点状态
func (m *WorkflowTreeModel) AddOrUpdateNode(msg DAGNodeStatusMsg) {
	node, exists := m.nodes[msg.NodeID]
	if !exists {
		node = &DAGNodeItem{
			ID: msg.NodeID,
		}
		m.nodes[msg.NodeID] = node
		m.nodeOrder = append(m.nodeOrder, msg.NodeID)
	}
	if msg.Role != "" {
		node.Role = msg.Role
	}
	if msg.Status != "" {
		node.Status = msg.Status
	}
	if msg.Attempt > 0 {
		node.Attempt = msg.Attempt
	}
	if msg.MaxAttempts > 0 {
		node.MaxAttempts = msg.MaxAttempts
	}
	if msg.Output != "" {
		node.Output = msg.Output
	}
	if msg.ErrorMsg != "" {
		node.ErrorMsg = msg.ErrorMsg
	}
}

// UpdateNodeStatus 根据 ID 更新节点状态
func (m *WorkflowTreeModel) UpdateNodeStatus(id string, status string) {
	if node, exists := m.nodes[id]; exists {
		node.Status = status
	}
}

// renderBadge 渲染符合 ANSI 256 色彩的 Lipgloss 状态徽标
func (m *WorkflowTreeModel) renderBadge(node *DAGNodeItem) string {
	switch strings.ToUpper(strings.TrimSpace(node.Status)) {
	case "RUNNING":
		spView := m.spinner.View()
		if spView == "" {
			spView = "⠋"
		}
		return styleRunning.Render(fmt.Sprintf("%s [RUNNING]", spView))
	case "BLOCKED", "WAITING":
		return styleBlocked.Render("⏸ [BLOCKED]")
	case "REPAIR":
		if node.MaxAttempts > 0 {
			return styleRepair.Render(fmt.Sprintf("↻ [REPAIR: %d/%d]", node.Attempt, node.MaxAttempts))
		}
		return styleRepair.Render("↻ [REPAIR]")
	case "DONE", "SUCCESS":
		return styleDone.Render("✓ [DONE]")
	case "APPROVAL", "APPROVAL REQUIRED":
		return styleApproval.Render("⚠ [APPROVAL]")
	case "FAILED", "ERROR":
		return styleFailed.Render("✗ [FAILED]")
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Render(fmt.Sprintf("[%s]", node.Status))
	}
}

// Render 渲染左侧 DAG 树视图
func (m *WorkflowTreeModel) Render(width, height int) string {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}

	var lines []string
	elapsed := time.Since(m.startTime).Round(100 * time.Millisecond)
	title := m.workflowName
	if title == "" {
		title = "DAG 任务拓扑"
	}
	lines = append(lines, styleTitle.Render(fmt.Sprintf("▾ [%s] (%s)", title, elapsed)))

	if len(m.nodeOrder) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color("243")).Render("  (等待 DAG 任务规划 / Waiting for DAG plan...)"))
	} else {
		for i, id := range m.nodeOrder {
			node := m.nodes[id]
			connector := "├─ "
			if i == len(m.nodeOrder)-1 && node.ErrorMsg == "" {
				connector = "└─ "
			}
			badge := m.renderBadge(node)
			displayName := node.Role
			if displayName == "" {
				displayName = node.ID
			} else if displayName != node.ID {
				displayName = fmt.Sprintf("%s (%s)", node.Role, node.ID)
			}
			lines = append(lines, fmt.Sprintf("%s%s %s", styleTreeLine.Render(connector), badge, displayName))

			if node.ErrorMsg != "" {
				errConn := "└─ "
				if i < len(m.nodeOrder)-1 {
					errConn = "│  └─ "
				}
				lines = append(lines, styleFailed.Render(fmt.Sprintf("%s✗ %s", errConn, node.ErrorMsg)))
			}
		}
	}

	if len(lines) > height {
		lines = lines[:height]
	}

	content := strings.Join(lines, "\n")
	paneStyle := lipgloss.NewStyle().
		Width(width).
		Height(height).
		MaxHeight(height).
		MaxWidth(width)

	return paneStyle.Render(content)
}
