package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gp/internal/core/auth"
	"gp/internal/core/retrieval"
)

// ConfirmDestructiveFunc 高危动作（risk=destructive）二次确认回调；返回 true 表示放行
type ConfirmDestructiveFunc func(ctx context.Context, extName, actionName string, args map[string]any) (bool, error)

// ExtensionStatus 用于 `/ext` TUI 弹层与诊断展示的只读状态摘要
type ExtensionStatus struct {
	Name        string
	Description string
	AdapterType string
	Source      string
	Enabled     bool
	ActionCount int
	State       string // "ready" | "running" | "idle" | "disabled" | "error"
	LoadError   string
	FilePath    string
}

// CallContext 传入 Registry.Call 的调用方上下文
type CallContext struct {
	Cwd        string
	AgentID    string
	IsSubagent bool
	SessionID  string
}

type managedExtension struct {
	spec         *ExtensionSpec
	adapter      Adapter
	enabled      bool
	loadError    string
	actionsMu    sync.RWMutex
	actions      []ActionDef
	actionsReady bool
	bm25         *retrieval.BM25
}

// Registry 统一外部能力注册中心：管理发现、BM25 动作索引、执行治理、热重载与空闲进程回收
type Registry struct {
	mu               sync.RWMutex
	opts             LoadOptions
	cacheDir         string
	spillDir         string
	allowDestructive bool
	confirmFn        ConfirmDestructiveFunc
	authResolver     *AuthResolver
	exts             map[string]*managedExtension
	order            []string
	reapStop         chan struct{}
	reapOnce         sync.Once
}

// RegistryConfig 构造 Registry 所需的运行时依赖
type RegistryConfig struct {
	Load             LoadOptions
	AuthStore        *auth.Store
	CacheDir         string
	SpillDir         string
	AllowDestructive bool
	ConfirmFn        ConfirmDestructiveFunc
}

func NewRegistry(cfg RegistryConfig) *Registry {
	cacheDir := cfg.CacheDir
	if cacheDir == "" && cfg.Load.HomeDir != "" {
		cacheDir = filepath.Join(cfg.Load.HomeDir, ".gp", "cache")
	}
	spillDir := cfg.SpillDir
	if spillDir == "" && cfg.Load.HomeDir != "" {
		spillDir = filepath.Join(cfg.Load.HomeDir, ".gp", "tmp", "ext-outputs")
	}
	r := &Registry{
		opts:             cfg.Load,
		cacheDir:         cacheDir,
		spillDir:         spillDir,
		allowDestructive: cfg.AllowDestructive,
		confirmFn:        cfg.ConfirmFn,
		authResolver:     &AuthResolver{AuthStore: cfg.AuthStore},
		exts:             make(map[string]*managedExtension),
		reapStop:         make(chan struct{}),
	}
	r.Reload()
	go r.reapLoop()
	return r
}

func (r *Registry) SetConfirmHandler(fn ConfirmDestructiveFunc) {
	r.mu.Lock()
	r.confirmFn = fn
	r.mu.Unlock()
}

func (r *Registry) SetAllowDestructive(allow bool) {
	r.mu.Lock()
	r.allowDestructive = allow
	r.mu.Unlock()
}

// Reload 使用当前 opts 重新扫描目录与配置，热更新扩展并安全关闭旧适配器
func (r *Registry) Reload() {
	r.mu.RLock()
	opts := r.opts
	r.mu.RUnlock()
	r.ReloadWithOptions(opts)
}

// ReloadWithOptions 使用最新的 LoadOptions（含最新 DisabledExtensions / BridgeLegacyMCP）热重载全部扩展
func (r *Registry) ReloadWithOptions(opts LoadOptions) {
	loaded := DiscoverExtensions(opts)

	r.mu.Lock()
	r.opts = opts
	oldExts := r.exts
	nextExts := make(map[string]*managedExtension, len(loaded))
	nextOrder := make([]string, 0, len(loaded))

	for _, item := range loaded {
		if item.Spec == nil {
			continue
		}
		me := &managedExtension{
			spec:      item.Spec,
			enabled:   item.Enabled,
			loadError: item.LoadError,
		}
		if item.LoadError == "" {
			me.adapter = r.buildAdapter(item.Spec)
			if item.Spec.Adapter.Type != "mcp" && item.Spec.Adapter.Type != "openapi" {
				if defs, err := me.adapter.DiscoverActions(context.Background()); err == nil {
					me.setActions(defs)
				}
			} else if ma, ok := me.adapter.(*mcpAdapter); ok && len(ma.cached) > 0 {
				me.setActions(ma.cached)
			}
		}
		nextExts[item.Spec.Name] = me
		nextOrder = append(nextOrder, item.Spec.Name)
	}
	r.exts = nextExts
	r.order = nextOrder
	r.mu.Unlock()

	// 锁外关闭旧适配器，避免阻塞 TUI 渲染与其他并发读取
	for _, old := range oldExts {
		if old != nil && old.adapter != nil {
			_ = old.adapter.Close()
		}
	}
}

func (r *Registry) buildAdapter(spec *ExtensionSpec) Adapter {
	switch spec.Adapter.Type {
	case "guide":
		return newGuideAdapter(spec)
	case "cli":
		return newCLIAdapter(spec)
	case "script":
		return newScriptAdapter(spec)
	case "http":
		return newHTTPAdapter(spec)
	case "openapi":
		return newOpenAPIAdapter(spec)
	case "mcp":
		return newMCPAdapter(spec, r.cacheDir)
	default:
		return newGuideAdapter(spec)
	}
}

func (me *managedExtension) setActions(defs []ActionDef) {
	me.actionsMu.Lock()
	defer me.actionsMu.Unlock()
	me.actions = defs
	me.actionsReady = true
	idx := retrieval.NewBM25()
	triggers := ""
	if me.spec != nil && len(me.spec.Triggers) > 0 {
		triggers = " " + strings.Join(me.spec.Triggers, " ")
	}
	for _, d := range defs {
		doc := d.Name + " " + d.Description + " " + strings.Join(d.Tags, " ") + triggers + " " + string(d.InputSchema)
		idx.AddDoc(doc)
	}
	me.bm25 = idx
}

func (me *managedExtension) ensureActions(ctx context.Context) ([]ActionDef, error) {
	me.actionsMu.RLock()
	if me.actionsReady {
		out := make([]ActionDef, len(me.actions))
		copy(out, me.actions)
		me.actionsMu.RUnlock()
		return out, nil
	}
	me.actionsMu.RUnlock()

	if me.adapter == nil {
		return nil, nil
	}
	defs, err := me.adapter.DiscoverActions(ctx)
	if err != nil {
		return nil, err
	}
	me.setActions(defs)
	return defs, nil
}

// RenderDirectoryPrompt 输出确定性排序的 Stage 0 极简能力目录（保护 Prompt Cache 前缀）
func (r *Registry) RenderDirectoryPrompt() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	const maxDirBytes = 7500 // 软上限约 ~2000 tokens
	var lines []string
	totalBytes := 0
	omitted := 0
	for _, name := range r.order {
		me := r.exts[name]
		if me == nil || !me.enabled || me.loadError != "" {
			continue
		}
		me.actionsMu.RLock()
		acts := me.actions
		me.actionsMu.RUnlock()

		var summary string
		switch {
		case me.spec.Adapter.Type == "guide":
			summary = "guide-only"
		case len(acts) == 0 && (me.spec.Adapter.Type == "mcp" || me.spec.Adapter.Type == "openapi"):
			summary = "lazy-indexed; call ext_describe"
		case len(acts) <= 6:
			names := make([]string, 0, len(acts))
			for _, a := range acts {
				names = append(names, a.Name)
			}
			summary = "actions: " + strings.Join(names, ", ")
		default:
			summary = fmt.Sprintf("%d actions indexed; call ext_describe with query", len(acts))
		}
		line := fmt.Sprintf("- %s [%s]: %s (%s)", me.spec.Name, me.spec.Adapter.Type, me.spec.Description, summary)
		if totalBytes+len(line) > maxDirBytes {
			omitted++
			continue
		}
		lines = append(lines, line)
		totalBytes += len(line) + 1
	}
	if len(lines) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[Available Extensions]\n")
	for _, l := range lines {
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	if omitted > 0 {
		fmt.Fprintf(&sb, "- ... (%d more extensions omitted for token budget; run /ext to inspect)\n", omitted)
	}
	sb.WriteString("Workflow: Call `ext_describe(extension, query?)` before using an unfamiliar extension to inspect its guide and action schemas, then invoke via `ext_call(extension, action, args)`.")
	return sb.String()
}

// Describe 按需返回扩展的 GUIDE.md 与经 BM25 检索/裁剪后的动作 Schema
func (r *Registry) Describe(ctx context.Context, extName, query string, includeGuide bool, limit int) (string, bool) {
	return r.DescribeForCaller(ctx, extName, query, includeGuide, limit, false)
}

// DescribeForCaller 带子 Agent 权限校验的按需描述接口
func (r *Registry) DescribeForCaller(ctx context.Context, extName, query string, includeGuide bool, limit int, isSubagent bool) (string, bool) {
	if limit <= 0 {
		limit = 8
	}
	if limit > 20 {
		limit = 20
	}

	r.mu.RLock()
	me, ok := r.exts[strings.TrimSpace(extName)]
	var enabled bool
	var loadErr string
	if ok && me != nil {
		enabled = me.enabled
		loadErr = me.loadError
	}
	r.mu.RUnlock()

	if !ok || me == nil {
		return formatCodedError(ErrNotFound, fmt.Sprintf("extension %q not found", extName)), true
	}
	if loadErr != "" {
		return formatCodedError(ErrAdapterOffline, fmt.Sprintf("extension %q failed to load: %s", extName, loadErr)), true
	}
	if !enabled {
		return formatCodedError(ErrDisabled, fmt.Sprintf("extension %q is disabled (enable via /ext)", extName)), true
	}
	if isSubagent && me.spec.EffectiveSubagentPolicy() == "deny" {
		return formatCodedError(ErrDenied, fmt.Sprintf("subagent is not permitted to inspect extension %q (subagent_policy=deny)", extName)), true
	}

	defs, err := me.ensureActions(ctx)
	if err != nil {
		scrubber := NewSecretScrubber(me.spec.ResolvedEnvSecrets...)
		return scrubber.Scrub(formatCodedError(ErrAdapterOffline, fmt.Sprintf("discover actions for %q failed: %v", extName, err))), true
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "# Extension: %s (%s)\n%s\n", me.spec.Name, me.spec.Adapter.Type, me.spec.Description)
	if len(me.spec.Triggers) > 0 {
		fmt.Fprintf(&sb, "Triggers: %s\n", strings.Join(me.spec.Triggers, ", "))
	}

	if includeGuide && strings.TrimSpace(me.spec.GuideText) != "" {
		guide := me.spec.GuideText
		const maxGuideBytes = 8192
		if len(guide) > maxGuideBytes {
			fullPath := me.spec.FilePath
			if me.spec.Guide != "" && me.spec.Dir != "" {
				fullPath = filepath.Join(me.spec.Dir, me.spec.Guide)
			}
			guide = truncateUTF8Bytes(guide, 6144) + fmt.Sprintf("\n\n... [guide truncated; full file at %s]", fullPath)
		}
		sb.WriteString("\n## Guide\n")
		sb.WriteString(guide)
		sb.WriteByte('\n')
	}

	if len(defs) > 0 {
		selected := defs
		q := strings.TrimSpace(query)
		if q != "" || len(defs) > 10 {
			selected = me.searchActions(q, limit)
		}
		fmt.Fprintf(&sb, "\n## Actions (showing %d of %d)\n", len(selected), len(defs))
		for _, act := range selected {
			fmt.Fprintf(&sb, "### %s (risk=%s)\n%s\nSchema: %s\n\n", act.Name, act.Risk, act.Description, string(act.InputSchema))
		}
		if len(selected) < len(defs) {
			sb.WriteString("Tip: Pass a specific `query` to `ext_describe` to search other actions in this extension.\n")
		}
	}
	return strings.TrimSpace(sb.String()), false
}

func (me *managedExtension) searchActions(query string, limit int) []ActionDef {
	me.actionsMu.RLock()
	defer me.actionsMu.RUnlock()
	if len(me.actions) == 0 {
		return nil
	}
	if query == "" || me.bm25 == nil {
		if len(me.actions) <= limit {
			return append([]ActionDef(nil), me.actions...)
		}
		return append([]ActionDef(nil), me.actions[:limit]...)
	}
	scores := me.bm25.Score(query)
	type scored struct {
		idx   int
		score float64
	}
	items := make([]scored, 0, len(me.actions))
	lowQ := strings.ToLower(query)
	for i, a := range me.actions {
		sc := 0.0
		if i < len(scores) {
			sc = scores[i]
		}
		if strings.Contains(strings.ToLower(a.Name), lowQ) {
			sc += 5.0
		} else if strings.Contains(strings.ToLower(a.Description), lowQ) {
			sc += 2.0
		}
		items = append(items, scored{idx: i, score: sc})
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].score > items[j].score
	})
	n := min(limit, len(items))
	out := make([]ActionDef, 0, n)
	for i := 0; i < n; i++ {
		if items[i].score <= 0 && i > 0 {
			break
		}
		out = append(out, me.actions[items[i].idx])
	}
	return out
}

// Call 执行完整的安全治理调用管线：定位 -> 子 Agent 策略 -> 参数校验 -> 高危门禁 -> 鉴权脱敏 -> 执行 -> 预算裁剪
func (r *Registry) Call(ctx context.Context, extName, actionName string, args map[string]any, caller CallContext) *InvokeResult {
	start := time.Now()
	r.mu.RLock()
	me, ok := r.exts[strings.TrimSpace(extName)]
	var enabled bool
	var loadErr string
	if ok && me != nil {
		enabled = me.enabled
		loadErr = me.loadError
	}
	allowDestructive := r.allowDestructive
	confirmFn := r.confirmFn
	r.mu.RUnlock()

	if !ok || me == nil {
		return &InvokeResult{
			IsError:   true,
			ErrorCode: ErrNotFound,
			Content:   formatCodedError(ErrNotFound, fmt.Sprintf("extension %q not found", extName)),
		}
	}
	adapterType := me.spec.Adapter.Type
	if loadErr != "" {
		return &InvokeResult{
			IsError:     true,
			ErrorCode:   ErrAdapterOffline,
			AdapterType: adapterType,
			Content:     formatCodedError(ErrAdapterOffline, fmt.Sprintf("extension %q failed to load: %s", extName, loadErr)),
		}
	}
	if !enabled {
		return &InvokeResult{
			IsError:     true,
			ErrorCode:   ErrDisabled,
			AdapterType: adapterType,
			Content:     formatCodedError(ErrDisabled, fmt.Sprintf("extension %q is disabled", extName)),
		}
	}

	// Step 0: 子 Agent deny 策略前置到 ensureActions 之前，防止被拒子 Agent 触发 MCP 启动或远端 SpecURL 拉取
	if caller.IsSubagent && me.spec.EffectiveSubagentPolicy() == "deny" {
		return &InvokeResult{
			IsError:     true,
			ErrorCode:   ErrDenied,
			AdapterType: adapterType,
			Content:     formatCodedError(ErrDenied, fmt.Sprintf("subagent is not permitted to invoke extension %q (subagent_policy=deny)", extName)),
		}
	}

	defs, err := me.ensureActions(ctx)
	if err != nil {
		scrubber := NewSecretScrubber(me.spec.ResolvedEnvSecrets...)
		return &InvokeResult{
			IsError:     true,
			ErrorCode:   ErrAdapterOffline,
			AdapterType: adapterType,
			Content:     scrubber.Scrub(formatCodedError(ErrAdapterOffline, fmt.Sprintf("discover actions for %q failed: %v", extName, err))),
		}
	}
	var targetAction *ActionDef
	for i := range defs {
		if defs[i].Name == actionName {
			targetAction = &defs[i]
			break
		}
	}
	if targetAction == nil {
		return &InvokeResult{
			IsError:     true,
			ErrorCode:   ErrNotFound,
			AdapterType: adapterType,
			Content:     formatCodedError(ErrNotFound, fmt.Sprintf("action %q not found in extension %q (call ext_describe to inspect available actions)", actionName, extName)),
		}
	}

	// Step 1: 子 Agent 权限收敛校验
	if caller.IsSubagent {
		pol := me.spec.EffectiveSubagentPolicy()
		if pol == "deny" || (pol == "read_only" && targetAction.Risk != RiskRead) {
			return &InvokeResult{
				IsError:     true,
				ErrorCode:   ErrDenied,
				AdapterType: adapterType,
				Risk:        targetAction.Risk,
				Content:     formatCodedError(ErrDenied, fmt.Sprintf("subagent is not permitted to invoke %s.%s (risk=%s, subagent_policy=%s)", extName, actionName, targetAction.Risk, pol)),
			}
		}
	}

	// Step 2: 参数默认值填充与必填/类型/枚举校验
	normalizedArgs, valErr := validateAndFillArgs(args, targetAction.InputSchema)
	if valErr != nil {
		return &InvokeResult{
			IsError:     true,
			ErrorCode:   ErrInvalidArgs,
			AdapterType: adapterType,
			Risk:        targetAction.Risk,
			Content:     formatCodedError(ErrInvalidArgs, fmt.Sprintf("%v\nExpected schema: %s", valErr, string(targetAction.InputSchema))),
		}
	}

	// Step 3: 高危动作门禁（位于鉴权解析之前，避免未授权操作触发 cmd:// 外部命令或加载密钥）
	if targetAction.Risk == RiskDestructive && !allowDestructive {
		if confirmFn == nil {
			return &InvokeResult{
				IsError:     true,
				ErrorCode:   ErrDenied,
				AdapterType: adapterType,
				Risk:        targetAction.Risk,
				Content:     formatCodedError(ErrDenied, fmt.Sprintf("destructive action %s.%s requires interactive confirmation or extension_allow_destructive=true", extName, actionName)),
			}
		}
		allowed, cErr := confirmFn(ctx, extName, actionName, normalizedArgs)
		if cErr != nil || !allowed {
			return &InvokeResult{
				IsError:     true,
				ErrorCode:   ErrDenied,
				AdapterType: adapterType,
				Risk:        targetAction.Risk,
				Content:     formatCodedError(ErrDenied, fmt.Sprintf("destructive action %s.%s denied by user", extName, actionName)),
			}
		}
	}

	// Step 4: 鉴权解析 + 注册密钥脱敏器（覆盖原文/URL编码/Base64及敏感环境变量）
	resolvedAuth, authErr := r.authResolver.Resolve(ctx, me.spec.Auth)
	if authErr != nil {
		scrubber := NewSecretScrubber(me.spec.ResolvedEnvSecrets...)
		return &InvokeResult{
			IsError:     true,
			ErrorCode:   ErrAuthMissing,
			AdapterType: adapterType,
			Risk:        targetAction.Risk,
			Content:     scrubber.Scrub(formatCodedError(ErrAuthMissing, authErr.Error())),
		}
	}
	scrubber := NewSecretScrubberForAuth(resolvedAuth, me.spec.ResolvedEnvSecrets...)

	// Step 5: 执行底层适配器
	res, err := me.adapter.Invoke(ctx, InvokeRequest{
		Action:     actionName,
		ActionRisk: targetAction.Risk,
		Args:       normalizedArgs,
		Cwd:        caller.Cwd,
		AgentID:    caller.AgentID,
		Auth:       resolvedAuth,
		Timeout:    targetAction.Timeout,
	})
	if err != nil {
		res = &InvokeResult{
			IsError:   true,
			ErrorCode: ErrUpstream,
			Content:   formatCodedError(ErrUpstream, err.Error()),
			Duration:  time.Since(start),
		}
	}

	// Step 6: 脱敏 + 输出预算裁剪与落盘（正常与报错输出均受预算约束，防止巨型错误页打爆上下文）
	res.AdapterType = adapterType
	res.Risk = targetAction.Risk
	res.Content = scrubber.Scrub(res.Content)
	if res.Duration == 0 {
		res.Duration = time.Since(start)
	}
	maxBytes := me.spec.EffectiveOutputMaxBytes()
	maxLines := me.spec.EffectiveOutputMaxLines()
	if res.IsError {
		maxBytes = min(maxBytes, 8192)
		maxLines = min(maxLines, 120)
	}
	spillBase := r.spillDir
	if caller.SessionID != "" && spillBase != "" {
		spillBase = filepath.Join(spillBase, sanitizeFilename(caller.SessionID))
	}
	trimmed, truncated, spillPath := ApplyOutputBudget(
		res.Content,
		maxBytes,
		maxLines,
		spillBase,
		extName,
		actionName,
	)
	res.Content = trimmed
	res.Truncated = truncated
	res.SpillPath = spillPath
	return res
}

func validateAndFillArgs(raw map[string]any, schemaBytes json.RawMessage) (map[string]any, error) {
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		out[k] = v
	}
	if len(schemaBytes) == 0 {
		return out, nil
	}
	var schema struct {
		Properties map[string]struct {
			Type    any   `json:"type"` // 兼容单类型 "string" 与联合类型 ["string", "null"]
			Default any   `json:"default"`
			Enum    []any `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		return out, nil
	}
	for name, prop := range schema.Properties {
		if _, exists := out[name]; !exists && prop.Default != nil {
			out[name] = prop.Default
		}
	}
	for _, reqKey := range schema.Required {
		v, exists := out[reqKey]
		propTypes := extractSchemaTypes(schema.Properties[reqKey].Type)
		allowsNull := containsString(propTypes, "null")
		if !exists || (v == nil && !allowsNull) {
			return nil, fmt.Errorf("missing required parameter %q", reqKey)
		}
		if v != nil && formatArgScalar(v) == "" && len(propTypes) == 1 && propTypes[0] == "string" {
			return nil, fmt.Errorf("missing required parameter %q", reqKey)
		}
	}
	for name, prop := range schema.Properties {
		v, exists := out[name]
		if !exists {
			continue
		}
		propTypes := extractSchemaTypes(prop.Type)
		if v == nil {
			if len(propTypes) > 0 && !containsString(propTypes, "null") {
				return nil, fmt.Errorf("parameter %q cannot be null (expects %s)", name, strings.Join(propTypes, "|"))
			}
			continue
		}
		if err := validateParamTypes(name, v, propTypes); err != nil {
			return nil, err
		}
		if len(prop.Enum) > 0 {
			matched := false
			vStr := formatArgScalar(v)
			for _, allowed := range prop.Enum {
				if vStr == formatArgScalar(allowed) {
					matched = true
					break
				}
			}
			if !matched {
				return nil, fmt.Errorf("parameter %q value %v is not in allowed enum %v", name, v, prop.Enum)
			}
		}
	}
	return out, nil
}

func extractSchemaTypes(raw any) []string {
	switch t := raw.(type) {
	case string:
		if s := strings.ToLower(strings.TrimSpace(t)); s != "" {
			return []string{s}
		}
	case []any:
		var out []string
		for _, it := range t {
			if s, ok := it.(string); ok {
				if low := strings.ToLower(strings.TrimSpace(s)); low != "" {
					out = append(out, low)
				}
			}
		}
		return out
	}
	return nil
}

func containsString(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}

func validateParamTypes(name string, v any, wantTypes []string) error {
	if len(wantTypes) == 0 || containsString(wantTypes, "any") {
		return nil
	}
	var lastErr error
	for _, wt := range wantTypes {
		if wt == "null" {
			continue
		}
		if err := validateParamType(name, v, wt); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("parameter %q expects %s, got %T", name, strings.Join(wantTypes, "|"), v)
}

func validateParamType(name string, v any, wantType string) error {
	switch strings.ToLower(strings.TrimSpace(wantType)) {
	case "", "any":
		return nil
	case "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("parameter %q expects string, got %T", name, v)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("parameter %q expects boolean, got %T", name, v)
		}
	case "integer":
		switch n := v.(type) {
		case int, int64, int32:
			return nil
		case json.Number:
			if _, err := n.Int64(); err == nil {
				return nil
			}
			if f, err := n.Float64(); err == nil && math.Trunc(f) == f {
				return nil
			}
			return fmt.Errorf("parameter %q expects integer, got %s", name, n.String())
		case float64:
			if math.Trunc(n) != n {
				return fmt.Errorf("parameter %q expects integer, got %v", name, n)
			}
		default:
			return fmt.Errorf("parameter %q expects integer, got %T", name, v)
		}
	case "number":
		switch n := v.(type) {
		case int, int64, int32, float64, float32:
			return nil
		case json.Number:
			if _, err := n.Float64(); err != nil {
				return fmt.Errorf("parameter %q expects number, got %s", name, n.String())
			}
		default:
			return fmt.Errorf("parameter %q expects number, got %T", name, v)
		}
	case "array":
		if _, ok := v.([]any); !ok {
			return fmt.Errorf("parameter %q expects array, got %T", name, v)
		}
	case "object":
		if _, ok := v.(map[string]any); !ok {
			return fmt.Errorf("parameter %q expects object, got %T", name, v)
		}
	}
	return nil
}

// SetEnabled 实时启用或禁用某个扩展（对 MCP 仅做临时 Disconnect 而非永久 Close，便于重新启用后继续懒启动）
func (r *Registry) SetEnabled(name string, enabled bool) bool {
	var toDisconnect Adapter
	r.mu.Lock()
	me, ok := r.exts[name]
	if ok && me != nil {
		if me.loadError != "" && enabled {
			r.mu.Unlock()
			return false
		}
		me.enabled = enabled
		r.opts.DisabledExtensions = updateDisabledNames(r.opts.DisabledExtensions, name, enabled)
		if !enabled && me.adapter != nil {
			toDisconnect = me.adapter
		}
	}
	r.mu.Unlock()
	if toDisconnect != nil {
		if ma, isMCP := toDisconnect.(*mcpAdapter); isMCP {
			_ = ma.Disconnect()
		} else {
			_ = toDisconnect.Close()
		}
	}
	return ok
}

func updateDisabledNames(list []string, name string, enable bool) []string {
	target := strings.ToLower(strings.TrimSpace(name))
	var out []string
	for _, it := range list {
		if strings.ToLower(strings.TrimSpace(it)) == target {
			continue
		}
		out = append(out, it)
	}
	if !enable && target != "" {
		out = append(out, strings.TrimSpace(name))
	}
	return out
}

// HealthCheck 执行单个扩展连通性探测（带超时保护与敏感信息脱敏）
func (r *Registry) HealthCheck(ctx context.Context, name string) error {
	r.mu.RLock()
	me, ok := r.exts[name]
	var loadErr string
	if ok && me != nil {
		loadErr = me.loadError
	}
	r.mu.RUnlock()
	if !ok || me == nil {
		return fmt.Errorf("extension %q not found", name)
	}
	if loadErr != "" {
		return fmt.Errorf("manifest error: %s", loadErr)
	}
	if me.adapter == nil {
		return nil
	}
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := me.adapter.HealthCheck(hctx); err != nil {
		scrubber := NewSecretScrubber(me.spec.ResolvedEnvSecrets...)
		return fmt.Errorf("%s", scrubber.Scrub(err.Error()))
	}
	return nil
}

// ListStatus 返回全部已发现扩展的只读状态列表（按 name 升序）
func (r *Registry) ListStatus() []ExtensionStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ExtensionStatus, 0, len(r.order))
	for _, name := range r.order {
		me := r.exts[name]
		if me == nil {
			continue
		}
		me.actionsMu.RLock()
		actCnt := len(me.actions)
		me.actionsMu.RUnlock()

		state := "ready"
		if me.loadError != "" {
			state = "error"
		} else if !me.enabled {
			state = "disabled"
		} else if ma, ok := me.adapter.(*mcpAdapter); ok {
			if ma.IsRunning() {
				state = "running"
			} else {
				state = "idle"
			}
		}
		out = append(out, ExtensionStatus{
			Name:        me.spec.Name,
			Description: me.spec.Description,
			AdapterType: me.spec.Adapter.Type,
			Source:      me.spec.Source,
			Enabled:     me.enabled,
			ActionCount: actCnt,
			State:       state,
			LoadError:   me.loadError,
			FilePath:    me.spec.FilePath,
		})
	}
	return out
}

// EnabledCount 返回当前已启用且加载健康的扩展总数
func (r *Registry) EnabledCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, me := range r.exts {
		if me != nil && me.enabled && me.loadError == "" {
			n++
		}
	}
	return n
}

func (r *Registry) reapLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-r.reapStop:
			return
		case now := <-ticker.C:
			// 锁内仅收集待巡检的 mcpAdapter 切片，锁外执行可能阻塞的 Close()
			r.mu.RLock()
			var mcps []*mcpAdapter
			for _, me := range r.exts {
				if me != nil {
					if ma, ok := me.adapter.(*mcpAdapter); ok {
						mcps = append(mcps, ma)
					}
				}
			}
			r.mu.RUnlock()
			for _, ma := range mcps {
				ma.ReapIfIdle(now)
			}
		}
	}
}

// CloseAll 停止空闲巡检并关闭所有外部子进程（在 gp 退出时 defer 调用）
func (r *Registry) CloseAll() {
	r.reapOnce.Do(func() {
		close(r.reapStop)
	})
	r.mu.Lock()
	exts := r.exts
	r.exts = make(map[string]*managedExtension)
	r.order = nil
	r.mu.Unlock()
	for _, me := range exts {
		if me != nil && me.adapter != nil {
			_ = me.adapter.Close()
		}
	}
}

var _ = os.TempDir
