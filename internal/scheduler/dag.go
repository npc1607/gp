package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

var (
	ErrCyclicDependency      = errors.New("cyclic dependency detected in DAG")
	ErrCircuitBreakerTripped = errors.New("circuit breaker tripped: repair loop exceeded maximum attempts (2)")
	ErrDependencyFailed      = errors.New("upstream dependency failed")
	ErrNodeNotFound          = errors.New("task node not found")
	ErrDuplicateNodeID       = errors.New("duplicate task node ID")
)

// TaskNodeStatus 任务节点生命周期状态
type TaskNodeStatus string

const (
	TaskNodePending   TaskNodeStatus = "pending"
	TaskNodeRunning   TaskNodeStatus = "running"
	TaskNodeCompleted TaskNodeStatus = "completed"
	TaskNodeFailed    TaskNodeStatus = "failed"
	TaskNodeRepairing TaskNodeStatus = "repairing"
)

// NodeRunner 节点执行回调函数
type NodeRunner func(ctx context.Context, node *TaskNode, mergedInput string) (string, error)

// TaskNode DAG 调度图中的任务节点
type TaskNode struct {
	ID           string
	AgentRole    string // scout, planner, implementer, verifier, reviewer
	TaskPrompt   string
	DependsOn    []string
	Status       TaskNodeStatus
	Output       string
	Error        error
	CustomRunner NodeRunner
	Attempt      int
	doneChan     chan struct{}
	mu           sync.RWMutex
}

// GetStatus 线程安全获取节点状态
func (n *TaskNode) GetStatus() TaskNodeStatus {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.Status
}

// GetOutput 线程安全获取节点产出
func (n *TaskNode) GetOutput() string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.Output
}

// GetError 线程安全获取节点错误
func (n *TaskNode) GetError() error {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.Error
}

// DAGScheduler 拓扑依赖图执行引擎与自愈闭环调度器
type DAGScheduler struct {
	nodes             map[string]*TaskNode
	nodeOrder         []string
	blackboard        *Blackboard
	governor          *Governor
	defaultRunner     NodeRunner
	maxRepairAttempts int // 默认 2 次 ("两轮原则熔断器")
	mu                sync.RWMutex
}

// NewDAGScheduler 创建 DAG 调度器实例
func NewDAGScheduler(gov *Governor, bb *Blackboard) *DAGScheduler {
	if gov == nil {
		gov = NewGovernor(10, 100000)
	}
	if bb == nil {
		bb = NewBlackboard()
	}
	return &DAGScheduler{
		nodes:             make(map[string]*TaskNode),
		nodeOrder:         make([]string, 0),
		blackboard:        bb,
		governor:          gov,
		maxRepairAttempts: 2,
	}
}

// AddNode 注册任务节点至 DAG 图中
func (s *DAGScheduler) AddNode(node *TaskNode) error {
	if node == nil {
		return errors.New("cannot add nil node")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.nodes[node.ID]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicateNodeID, node.ID)
	}
	s.nodes[node.ID] = node
	s.nodeOrder = append(s.nodeOrder, node.ID)
	return nil
}

// SetDefaultRunner 配置所有节点的默认执行器
func (s *DAGScheduler) SetDefaultRunner(runner NodeRunner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.defaultRunner = runner
}

// SetMaxRepairAttempts 设置自愈回路最大尝试次数 (熔断阈值)
func (s *DAGScheduler) SetMaxRepairAttempts(max int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxRepairAttempts = max
}

// GetNode 获取指定 ID 的节点
func (s *DAGScheduler) GetNode(id string) (*TaskNode, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.nodes[id]
	return n, ok
}

// Validate 使用 Kahn 算法检验拓扑依赖图是否有环及依赖完整性
func (s *DAGScheduler) Validate() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	inDegree := make(map[string]int)
	graph := make(map[string][]string)

	for id := range s.nodes {
		inDegree[id] = 0
	}

	for id, node := range s.nodes {
		for _, dep := range node.DependsOn {
			if _, exists := s.nodes[dep]; !exists {
				return fmt.Errorf("dependency %q for node %q: %w", dep, id, ErrNodeNotFound)
			}
			inDegree[id]++
			graph[dep] = append(graph[dep], id)
		}
	}

	queue := make([]string, 0, len(s.nodes))
	for id, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, id)
		}
	}

	visited := 0
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		visited++

		for _, next := range graph[curr] {
			inDegree[next]--
			if inDegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}

	if visited < len(s.nodes) {
		return ErrCyclicDependency
	}

	return nil
}

// Run 启动拓扑依赖调度引擎，各节点基于 doneChan 自动并发唤醒与自愈流转
func (s *DAGScheduler) Run(ctx context.Context) error {
	if err := s.Validate(); err != nil {
		return err
	}

	s.mu.RLock()
	nodesList := make([]*TaskNode, 0, len(s.nodes))
	for _, id := range s.nodeOrder {
		node := s.nodes[id]
		node.mu.Lock()
		node.doneChan = make(chan struct{})
		node.Status = TaskNodePending
		node.Error = nil
		node.Output = ""
		node.Attempt = 0
		node.mu.Unlock()
		nodesList = append(nodesList, node)
	}
	s.mu.RUnlock()

	var firstErr error
	var errMu sync.Mutex
	setFirstErr := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		defer errMu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}

	var wg sync.WaitGroup
	for _, n := range nodesList {
		wg.Add(1)
		go func(node *TaskNode) {
			defer wg.Done()
			defer func() {
				select {
				case <-node.doneChan:
				default:
					close(node.doneChan)
				}
			}()

			// 1. 等待全部上游依赖完成
			for _, depID := range node.DependsOn {
				s.mu.RLock()
				dep := s.nodes[depID]
				s.mu.RUnlock()

				select {
				case <-ctx.Done():
					node.mu.Lock()
					node.Status = TaskNodeFailed
					node.Error = ctx.Err()
					node.mu.Unlock()
					setFirstErr(ctx.Err())
					return
				case <-dep.doneChan:
					dep.mu.RLock()
					depStatus := dep.Status
					depErr := dep.Error
					dep.mu.RUnlock()

					if depStatus == TaskNodeFailed || depErr != nil {
						node.mu.Lock()
						node.Status = TaskNodeFailed
						node.Error = ErrDependencyFailed
						node.mu.Unlock()
						setFirstErr(ErrDependencyFailed)
						return
					}
				}
			}

			if err := ctx.Err(); err != nil {
				node.mu.Lock()
				node.Status = TaskNodeFailed
				node.Error = err
				node.mu.Unlock()
				setFirstErr(err)
				return
			}

			// 2. 从黑板组装依赖节点的输入上下文
			mergedInput := s.blackboard.BuildInputForTask(node.DependsOn)

			runner := node.CustomRunner
			if runner == nil {
				s.mu.RLock()
				runner = s.defaultRunner
				s.mu.RUnlock()
			}
			if runner == nil {
				runner = func(c context.Context, n *TaskNode, in string) (string, error) {
					return fmt.Sprintf("completed %s", n.ID), nil
				}
			}

			// 3. 区分角色执行：Verifier 支持自愈回路与熔断控制
			if strings.EqualFold(node.AgentRole, "verifier") {
				for {
					node.mu.Lock()
					node.Status = TaskNodeRunning
					node.mu.Unlock()

					var out string
					var runErr error
					execErr := s.governor.ExecuteSubagent(ctx, func(execCtx context.Context) error {
						var e error
						out, e = runner(execCtx, node, mergedInput)
						return e
					})
					if execErr != nil {
						runErr = execErr
					}

					if ctx.Err() != nil {
						node.mu.Lock()
						node.Status = TaskNodeFailed
						node.Error = ctx.Err()
						node.mu.Unlock()
						setFirstErr(ctx.Err())
						return
					}

					if runErr == nil {
						// Verifier 验证通过
						node.mu.Lock()
						node.Status = TaskNodeCompleted
						node.Output = out
						node.Error = nil
						node.mu.Unlock()
						s.blackboard.Set(node.ID, out)
						return
					}

					// Verifier 报告失败，触发自愈重试判断
					node.mu.Lock()
					node.Attempt++
					currentAttempt := node.Attempt
					node.mu.Unlock()

					s.mu.RLock()
					maxAttempts := s.maxRepairAttempts
					s.mu.RUnlock()

					// 两轮原则熔断器：超过两轮失败自动终止并触发人工介入
					if currentAttempt >= maxAttempts {
						node.mu.Lock()
						node.Status = TaskNodeFailed
						node.Error = ErrCircuitBreakerTripped
						node.mu.Unlock()
						setFirstErr(ErrCircuitBreakerTripped)
						return
					}

					// 寻找上游负责代码实现的 implementer 节点
					s.mu.RLock()
					var implementer *TaskNode
					for _, depID := range node.DependsOn {
						if dep, ok := s.nodes[depID]; ok && strings.EqualFold(dep.AgentRole, "implementer") {
							implementer = dep
							break
						}
					}
					if implementer == nil && len(node.DependsOn) > 0 {
						implementer = s.nodes[node.DependsOn[0]]
					}
					s.mu.RUnlock()

					if implementer == nil {
						node.mu.Lock()
						node.Status = TaskNodeFailed
						node.Error = runErr
						node.mu.Unlock()
						setFirstErr(runErr)
						return
					}

					// 重新激活 Implementer 进行自愈修复
					implementer.mu.Lock()
					implementer.Status = TaskNodeRepairing
					implementer.mu.Unlock()

					repairPrompt := fmt.Sprintf("\n[Verifier 测试失败反馈 (第 %d 次修复)]:\n%v\n请针对上述错误进行自愈修复。", currentAttempt, runErr)
					impInput := implementer.TaskPrompt + repairPrompt

					impRunner := implementer.CustomRunner
					if impRunner == nil {
						s.mu.RLock()
						impRunner = s.defaultRunner
						s.mu.RUnlock()
					}
					if impRunner == nil {
						impRunner = func(c context.Context, n *TaskNode, in string) (string, error) {
							return fmt.Sprintf("repaired output for %s", n.ID), nil
						}
					}

					var repairedOut string
					impErr := s.governor.ExecuteSubagent(ctx, func(execCtx context.Context) error {
						var e error
						repairedOut, e = impRunner(execCtx, implementer, impInput)
						return e
					})

					if impErr != nil {
						implementer.mu.Lock()
						implementer.Status = TaskNodeFailed
						implementer.Error = impErr
						implementer.mu.Unlock()

						node.mu.Lock()
						node.Status = TaskNodeFailed
						node.Error = fmt.Errorf("implementer repair failed: %w", impErr)
						node.mu.Unlock()
						setFirstErr(node.Error)
						return
					}

					implementer.mu.Lock()
					implementer.Status = TaskNodeCompleted
					implementer.Output = repairedOut
					implementer.mu.Unlock()
					s.blackboard.Set(implementer.ID, repairedOut)

					// 重新组装输入上下文，继续下一轮 Verifier 验证
					mergedInput = s.blackboard.BuildInputForTask(node.DependsOn)
				}
			} else {
				// 普通非 Verifier 节点执行
				node.mu.Lock()
				node.Status = TaskNodeRunning
				node.mu.Unlock()

				var out string
				var runErr error
				execErr := s.governor.ExecuteSubagent(ctx, func(execCtx context.Context) error {
					var e error
					out, e = runner(execCtx, node, mergedInput)
					return e
				})
				if execErr != nil {
					runErr = execErr
				}

				if runErr != nil {
					node.mu.Lock()
					node.Status = TaskNodeFailed
					node.Error = runErr
					node.mu.Unlock()
					setFirstErr(runErr)
					return
				}

				node.mu.Lock()
				node.Status = TaskNodeCompleted
				node.Output = out
				node.Error = nil
				node.mu.Unlock()
				s.blackboard.Set(node.ID, out)
			}
		}(n)
	}

	wg.Wait()

	errMu.Lock()
	defer errMu.Unlock()
	return firstErr
}
