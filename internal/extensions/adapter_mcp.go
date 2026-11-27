package extensions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gp/internal/ai"
	"gp/internal/mcp"
)

const mcpDiskCacheTTL = 24 * time.Hour

type mcpDiskCache struct {
	ConfigHash string      `json:"config_hash"`
	UpdatedAt  time.Time   `json:"updated_at"`
	Actions    []ActionDef `json:"actions"`
}

// mcpAdapter 封装既有 internal/mcp.Client：
// - 使用独立进程级 procCtx 管理子进程生命周期（单次请求取消绝不误杀长连接进程）
// - 区分永久 Close()（置 closed=true 杜绝并发重试泄漏孤儿进程）与临时 Disconnect()（供禁用/空闲回收）
// - 磁盘缓存指纹包含全部治理字段（DefaultRisk / Timeout / Cwd / Env），且加载时动态重算 Risk/Timeout
// - 无锁 running atomic.Bool 供 ListStatus() 查询，避免锁护航卡死 TUI
type mcpAdapter struct {
	spec       *ExtensionSpec
	cacheDir   string
	running    atomic.Bool
	mu         sync.Mutex
	closed     bool
	client     *mcp.Client
	procCancel context.CancelFunc
	lastUsed   time.Time
	inFlight   int
	cached     []ActionDef
}

func newMCPAdapter(spec *ExtensionSpec, cacheDir string) Adapter {
	a := &mcpAdapter{
		spec:     spec,
		cacheDir: cacheDir,
	}
	a.loadDiskCache()
	return a
}

func (a *mcpAdapter) Type() string { return "mcp" }

func (a *mcpAdapter) configFingerprint() string {
	h := sha256.New()
	_, _ = io.WriteString(h, a.spec.Adapter.Command+"\x00")
	_, _ = io.WriteString(h, strings.Join(a.spec.Adapter.Args, "\x00")+"\x00")
	_, _ = io.WriteString(h, strings.Join(a.spec.Adapter.IncludeTools, ",")+"\x00")
	_, _ = io.WriteString(h, strings.Join(a.spec.Adapter.ExcludeTools, ",")+"\x00")
	_, _ = io.WriteString(h, string(a.spec.EffectiveDefaultRisk())+"\x00")
	_, _ = io.WriteString(h, a.spec.EffectiveTimeout("").String()+"\x00")
	_, _ = io.WriteString(h, a.spec.Adapter.Cwd+"\x00")
	if len(a.spec.Adapter.Env) > 0 {
		keys := make([]string, 0, len(a.spec.Adapter.Env))
		for k := range a.spec.Adapter.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			_, _ = io.WriteString(h, k+"="+a.spec.Adapter.Env[k]+"\x00")
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (a *mcpAdapter) cachePath() string {
	if a.cacheDir == "" {
		return ""
	}
	return filepath.Join(a.cacheDir, "ext-mcp-"+sanitizeFilename(a.spec.Name)+".json")
}

func (a *mcpAdapter) loadDiskCache() {
	p := a.cachePath()
	if p == "" {
		return
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var dc mcpDiskCache
	if err := json.Unmarshal(raw, &dc); err != nil {
		return
	}
	if dc.ConfigHash != a.configFingerprint() || time.Since(dc.UpdatedAt) > mcpDiskCacheTTL || len(dc.Actions) == 0 {
		return
	}
	// 始终用当前清单的治理策略动态覆写缓存动作的 Risk/Timeout，杜绝旧缓存绕过高危门禁
	curRisk := a.spec.EffectiveDefaultRisk()
	curTimeout := a.spec.EffectiveTimeout("")
	for i := range dc.Actions {
		dc.Actions[i].Risk = curRisk
		dc.Actions[i].Timeout = curTimeout
	}
	a.cached = dc.Actions
}

func (a *mcpAdapter) saveDiskCache(defs []ActionDef) {
	p := a.cachePath()
	if p == "" || len(defs) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	dc := mcpDiskCache{
		ConfigHash: a.configFingerprint(),
		UpdatedAt:  time.Now(),
		Actions:    defs,
	}
	if raw, err := json.Marshal(dc); err == nil {
		_ = os.WriteFile(p, raw, 0o600)
	}
}

func (a *mcpAdapter) closeClientLocked() error {
	var err error
	if a.client != nil {
		err = a.client.Close()
		a.client = nil
	}
	if a.procCancel != nil {
		a.procCancel()
		a.procCancel = nil
	}
	a.running.Store(false)
	return err
}

// ensureConnectedLocked 使用独立进程级 procCtx 启动子进程，再用 reqCtx 完成握手
func (a *mcpAdapter) ensureConnectedLocked(reqCtx context.Context, fallbackCwd string) (*mcp.Client, error) {
	if a.closed {
		return nil, fmt.Errorf("mcp adapter %q is permanently closed", a.spec.Name)
	}
	if a.client != nil {
		a.lastUsed = time.Now()
		return a.client, nil
	}
	dir := a.spec.Adapter.Cwd
	if dir == "" {
		dir = fallbackCwd
	}
	if dir == "" {
		dir = a.spec.WorkspaceCwd
	}
	procCtx, procCancel := context.WithCancel(context.Background())
	c, err := mcp.NewStdioClientWithDir(procCtx, a.spec.Adapter.Command, a.spec.Adapter.Args, a.spec.Adapter.Env, dir)
	if err != nil {
		procCancel()
		return nil, err
	}
	c.ServerName = a.spec.Name
	initCtx, initCancel := context.WithTimeout(reqCtx, 10*time.Second)
	defer initCancel()
	if _, err := c.Initialize(initCtx); err != nil {
		_ = c.Close()
		procCancel()
		return nil, err
	}
	a.client = c
	a.procCancel = procCancel
	a.lastUsed = time.Now()
	a.running.Store(true)
	return c, nil
}

// DiscoverActions 优先使用有效指纹的磁盘缓存避免启动拉进程；无缓存时懒启动发现并写回缓存
func (a *mcpAdapter) DiscoverActions(ctx context.Context) ([]ActionDef, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, fmt.Errorf("mcp adapter %q is closed", a.spec.Name)
	}
	if len(a.cached) > 0 {
		out := make([]ActionDef, len(a.cached))
		copy(out, a.cached)
		return out, nil
	}
	c, err := a.ensureConnectedLocked(ctx, "")
	if err != nil {
		return nil, err
	}
	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	list, err := c.ListTools(listCtx)
	if err != nil {
		// 仅在底层传输断开时重置连接，避免 RPC 业务报错或超时误杀健康子进程
		if isMCPTransportBroken(err) && a.client == c {
			_ = a.closeClientLocked()
		}
		return nil, err
	}

	includeSet := toLowerSet(a.spec.Adapter.IncludeTools)
	excludeSet := toLowerSet(a.spec.Adapter.ExcludeTools)

	defs := make([]ActionDef, 0, len(list))
	for _, t := range list {
		low := strings.ToLower(t.Name)
		if len(includeSet) > 0 && !includeSet[low] {
			continue
		}
		if excludeSet[low] {
			continue
		}
		schema := ai.SanitizeToolSchema(t.InputSchema)
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		defs = append(defs, ActionDef{
			Name:        t.Name,
			Description: t.Description,
			Risk:        a.spec.EffectiveDefaultRisk(),
			Timeout:     a.spec.EffectiveTimeout(""),
			InputSchema: schema,
		})
	}
	a.cached = defs
	a.saveDiskCache(defs)
	out := make([]ActionDef, len(defs))
	copy(out, defs)
	return out, nil
}

func isMCPTransportBroken(err error) bool {
	if err == nil {
		return false
	}
	var rpcErr *mcp.RPCError
	if errors.As(err, &rpcErr) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "closed") || strings.Contains(msg, "broken pipe") || strings.Contains(msg, "eof")
}

func (a *mcpAdapter) Invoke(ctx context.Context, req InvokeRequest) (*InvokeResult, error) {
	start := time.Now()
	rawArgs, err := json.Marshal(req.Args)
	if err != nil {
		return &InvokeResult{
			IsError:   true,
			ErrorCode: ErrInvalidArgs,
			Content:   formatCodedError(ErrInvalidArgs, fmt.Sprintf("marshal mcp args: %v", err)),
			Duration:  time.Since(start),
		}, nil
	}
	if len(rawArgs) == 0 || string(rawArgs) == "null" {
		rawArgs = json.RawMessage(`{}`)
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = a.spec.EffectiveTimeout("")
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 仅对只读动作在底层管道意外断开时允许自动重连重试一次；绝不重复执行有副作用的 write/destructive 动作
	maxAttempts := 1
	if req.ActionRisk == RiskRead {
		maxAttempts = 2
	}

	var res *mcp.CallToolResult
	var callErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		a.mu.Lock()
		if a.closed {
			a.mu.Unlock()
			return &InvokeResult{
				IsError:   true,
				ErrorCode: ErrAdapterOffline,
				Content:   formatCodedError(ErrAdapterOffline, fmt.Sprintf("mcp server %q has been closed/reloaded", a.spec.Name)),
				Duration:  time.Since(start),
			}, nil
		}
		c, connErr := a.ensureConnectedLocked(cctx, req.Cwd)
		if connErr != nil {
			a.mu.Unlock()
			if errors.Is(cctx.Err(), context.DeadlineExceeded) {
				return &InvokeResult{
					IsError:   true,
					ErrorCode: ErrTimeout,
					Content:   formatCodedError(ErrTimeout, fmt.Sprintf("mcp server %q startup timed out after %s", a.spec.Name, timeout)),
					Duration:  time.Since(start),
				}, nil
			}
			return &InvokeResult{
				IsError:   true,
				ErrorCode: ErrAdapterOffline,
				Content:   formatCodedError(ErrAdapterOffline, fmt.Sprintf("mcp server %q failed to start: %v", a.spec.Name, connErr)),
				Duration:  time.Since(start),
			}, nil
		}
		a.inFlight++
		a.mu.Unlock()

		res, callErr = c.CallTool(cctx, req.Action, rawArgs)

		a.mu.Lock()
		a.inFlight--
		if callErr == nil {
			a.lastUsed = time.Now()
			a.mu.Unlock()
			break
		}
		// 仅在适配器未永久关闭且底层传输已死时立即清理死连接（无需等 inFlight==0，避免并发断连复用已死 client）
		shouldRetry := !a.closed && cctx.Err() == nil && isMCPTransportBroken(callErr)
		if shouldRetry && a.client == c {
			_ = a.closeClientLocked()
		}
		a.mu.Unlock()

		if !shouldRetry {
			break
		}
	}

	dur := time.Since(start)
	if callErr != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return &InvokeResult{
				IsError:   true,
				ErrorCode: ErrTimeout,
				Content:   formatCodedError(ErrTimeout, fmt.Sprintf("mcp call %s.%s timed out after %s", a.spec.Name, req.Action, timeout)),
				Duration:  dur,
			}, nil
		}
		return &InvokeResult{
			IsError:   true,
			ErrorCode: ErrUpstream,
			Content:   formatCodedError(ErrUpstream, fmt.Sprintf("mcp call %s.%s failed: %v", a.spec.Name, req.Action, callErr)),
			Duration:  dur,
		}, nil
	}

	var parts []string
	for _, c := range res.Content {
		if c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	text := strings.Join(parts, "\n")
	if res.IsError {
		return &InvokeResult{
			IsError:   true,
			ErrorCode: ErrUpstream,
			Content:   formatCodedError(ErrUpstream, text),
			Duration:  dur,
			RawBytes:  len(text),
		}, nil
	}
	return &InvokeResult{
		Content:  text,
		Duration: dur,
		RawBytes: len(text),
	}, nil
}

// ReapIfIdle 空闲超过 idle_ttl 且无在途调用时临时断连回收 MCP 子进程（不置 closed，后续调用可再次懒启动）
func (a *mcpAdapter) ReapIfIdle(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client == nil || a.inFlight > 0 {
		return false
	}
	if now.Sub(a.lastUsed) >= a.spec.EffectiveIdleTTL() {
		_ = a.closeClientLocked()
		return true
	}
	return false
}

// Disconnect 临时断开 MCP 子进程（供 SetEnabled(false) 使用，重新启用后仍可按需懒启动）
func (a *mcpAdapter) Disconnect() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closeClientLocked()
}

// IsRunning 无锁返回当前子进程运行态，避免 ListStatus() 在慢握手期间阻塞 TUI
func (a *mcpAdapter) IsRunning() bool {
	return a.running.Load()
}

func (a *mcpAdapter) HealthCheck(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := a.ensureConnectedLocked(cctx, "")
	return err
}

// Close 永久销毁适配器并终止子进程（供 ReloadWithOptions / CloseAll 使用，杜绝并发重试泄漏孤儿进程）
func (a *mcpAdapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	return a.closeClientLocked()
}

func toLowerSet(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, it := range items {
		if k := strings.ToLower(strings.TrimSpace(it)); k != "" {
			out[k] = true
		}
	}
	return out
}
