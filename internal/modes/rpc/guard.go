package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gp/internal/agent"
	"gp/internal/ai"
	"gp/internal/tools"
)

// riskVerdict represents the structured decision returned by the security review subagent.
type riskVerdict struct {
	RiskLevel          string `json:"risk_level"` // "high" | "medium" | "low"
	Allow              bool   `json:"allow"`
	CanApprove         bool   `json:"can_approve"`
	Reason             string `json:"reason"`
	RequiredPermission string `json:"required_permission"`
}

type cachedVerdict struct {
	verdict   riskVerdict
	expiresAt time.Time
}

// auditCache caches security audit verdicts across turns to avoid redundant subagent calls.
type auditCache struct {
	mu    sync.Mutex
	items map[string]cachedVerdict
	ttl   time.Duration
}

func newAuditCache(ttl time.Duration) *auditCache {
	return &auditCache{
		items: make(map[string]cachedVerdict),
		ttl:   ttl,
	}
}

func (c *auditCache) get(key string) (*riskVerdict, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(item.expiresAt) {
		delete(c.items, key)
		return nil, false
	}
	v := item.verdict
	return &v, true
}

func (c *auditCache) set(key string, v *riskVerdict) {
	if c == nil || v == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = cachedVerdict{
		verdict:   *v,
		expiresAt: time.Now().Add(c.ttl),
	}
}

// riskGuard evaluates potentially dangerous tool invocations using a dedicated security subagent.
type riskGuard struct {
	prov            ai.Provider
	modelName       string
	userPrompt      string
	workDir         string
	agentDirs       []string
	policy          ExecutionPolicy
	caller          CallerContext
	sessionID       string
	reqID           string
	emit            func(Event)
	cache           *auditCache
	approvedCache   *auditCache
	requestApproval func(ctx context.Context, toolName, reason string, rawArgs json.RawMessage) (bool, error)
}

func (g *riskGuard) scopePrefix() string {
	who := strings.TrimSpace(g.caller.SenderID)
	if who == "" {
		who = "anon"
	}
	role := "member"
	if g.caller.IsBoss {
		role = "boss"
	}
	return fmt.Sprintf("%s:%s:%s:%s", who, role, strings.TrimSpace(g.workDir), strings.TrimSpace(g.sessionID))
}

func wrapRegistryWithRiskGuard(reg *tools.Registry, guard *riskGuard) *tools.Registry {
	if reg == nil || guard == nil {
		return reg
	}
	guarded := tools.NewRegistry()
	for _, t := range reg.List() {
		if isSensitiveTool(t.Name()) {
			_ = guarded.Register(&guardedTool{inner: t, guard: guard})
		} else {
			_ = guarded.Register(t)
		}
	}
	return guarded
}

func isSensitiveTool(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch lower {
	case "subagent", "ask_peer_agent", "restore_observation", "ask_user_question", "feishu_send_message", "mcp__feishu__feishu_send_message":
		return false
	case "bash", "write", "edit", "read", "grep", "find", "ls", "codemode":
		return true
	}
	// Any read-only discovery/read MCP tool passes; all mutating/remote/exec tools go through audit.
	if strings.Contains(lower, "read") || strings.Contains(lower, "resolve") || strings.Contains(lower, "list") || strings.Contains(lower, "get") || strings.Contains(lower, "search") {
		if !strings.Contains(lower, "delete") && !strings.Contains(lower, "remove") && !strings.Contains(lower, "update") && !strings.Contains(lower, "edit") {
			return false
		}
	}
	return true
}

type guardedTool struct {
	inner tools.Tool
	guard *riskGuard
}

func (g *guardedTool) Name() string                 { return g.inner.Name() }
func (g *guardedTool) Description() string          { return g.inner.Description() }
func (g *guardedTool) InputSchema() json.RawMessage { return g.inner.InputSchema() }

func (g *guardedTool) Execute(ctx *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
	workDir := g.guard.workDir
	if workDir == "" && ctx != nil {
		workDir = ctx.Cwd
	}

	if g.guard.shouldSkipCheck(g.inner.Name(), rawArgs, workDir) {
		return g.inner.Execute(ctx, rawArgs)
	}

	var verdict *riskVerdict
	var err error
	if staticV, matched := g.guard.staticVerdict(g.inner.Name(), rawArgs, workDir); matched {
		verdict = staticV
	} else {
		if g.guard.caller.AllowApproval && g.guard.approvedCache != nil {
			if apprKey := buildApprovalCacheKey(g.guard.scopePrefix(), g.inner.Name(), rawArgs, workDir); apprKey != "" {
				if _, ok := g.guard.approvedCache.get(apprKey); ok {
					return g.inner.Execute(ctx, rawArgs)
				}
			}
		}
		verdict, err = g.guard.assess(ctx.Context, g.inner.Name(), rawArgs, workDir)
		if err != nil {
			return nil, fmt.Errorf("安全审计子 Agent 评估失败，基于安全原则拒绝执行 %s 操作 (%v)；禁止执行高风险操作", g.inner.Name(), err)
		}
	}

	if !verdict.Allow || strings.EqualFold(verdict.RiskLevel, "high") {
		if verdict.CanApprove && g.guard.caller.AllowApproval && g.guard.requestApproval != nil {
			apprKey := buildApprovalCacheKey(g.guard.scopePrefix(), g.inner.Name(), rawArgs, workDir)
			if apprKey != "" && g.guard.approvedCache != nil {
				if _, ok := g.guard.approvedCache.get(apprKey); ok {
					return g.inner.Execute(ctx, rawArgs)
				}
			}
			approved, apprErr := g.guard.requestApproval(ctx.Context, g.inner.Name(), verdict.Reason, rawArgs)
			if apprErr == nil && approved {
				if apprKey != "" && g.guard.approvedCache != nil {
					g.guard.approvedCache.set(apprKey, &riskVerdict{Allow: true})
				}
				return g.inner.Execute(ctx, rawArgs)
			}
			if apprErr != nil {
				return nil, fmt.Errorf("SECURITY_BLOCKED（危险程度：%s）: %s。禁止执行高风险操作（%v）。",
					verdict.RiskLevel, verdict.Reason, apprErr)
			}
			return nil, fmt.Errorf("SECURITY_BLOCKED（危险程度：%s）: %s。禁止执行高风险操作（管理员已拒绝），严禁尝试绕过。",
				verdict.RiskLevel, verdict.Reason)
		}

		return nil, fmt.Errorf("SECURITY_BLOCKED（危险程度：%s）: %s。禁止执行高风险操作，请向用户明确反馈该操作风险过高、未予执行，严禁尝试绕过。",
			verdict.RiskLevel, verdict.Reason)
	}

	return g.inner.Execute(ctx, rawArgs)
}

// isAgentSensitiveFile reports whether p points to a sensitive credential/config/database file
// inside an agent directory (e.g., auth.json, settings.json, gp.db, .env, *.key, *.pem).
func isAgentSensitiveFile(p string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(p)))
	switch base {
	case "auth.json", "settings.json", "gp.db", "gp.db-wal", "gp.db-shm", "config.toml":
		return true
	}
	if strings.HasPrefix(base, ".env") || strings.HasSuffix(base, ".key") || strings.HasSuffix(base, ".pem") {
		return true
	}
	return false
}

// isAgentDirPath reports whether p points inside a trusted agent directory
// (~/.gp, ~/.agents, or configured extra agentDirs/skill_dirs) and is NOT a sensitive credential/config file.
func isAgentDirPath(p string, extraDirs []string) bool {
	p = strings.TrimSpace(p)
	if p == "" || isAgentSensitiveFile(p) {
		return false
	}
	evalTarget := evalPathOrClean(p)
	if isAgentSensitiveFile(evalTarget) {
		return false
	}
	roots := make([]string, 0, len(extraDirs)+2)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots,
			filepath.Join(home, ".gp"),
			filepath.Join(home, ".agents"),
		)
	}
	for _, d := range extraDirs {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		roots = append(roots, resolveTargetPath(d, ""))
	}
	for _, root := range roots {
		evalRoot := evalPathOrClean(root)
		rel, err := filepath.Rel(evalRoot, evalTarget)
		if err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))) {
			return true
		}
	}
	return false
}

// isBashMutatingCommand reports whether a shell command appears to write, edit, or delete files.
func isBashMutatingCommand(cmd string) bool {
	lower := strings.ToLower(cmd)
	if strings.Contains(cmd, ">") || strings.Contains(lower, "tee ") {
		return true
	}
	segments, ok := splitShellSegments(cmd)
	if !ok {
		fields := splitCommandFields(cmd)
		if len(fields) == 0 {
			return false
		}
		return isMutatingBin(fields)
	}
	for _, seg := range segments {
		fields := splitCommandFields(seg.cmd)
		if len(fields) > 0 && isMutatingBin(fields) {
			return true
		}
	}
	return false
}

func isMutatingBin(fields []string) bool {
	bin := strings.ToLower(filepath.Base(fields[0]))
	switch bin {
	case "rm", "mv", "cp", "touch", "mkdir", "rmdir", "chmod", "chown", "ln", "unlink", "truncate", "dd", "install", "tee":
		return true
	case "sed":
		return !isReadOnlySed(fields)
	case "find":
		for _, f := range fields[1:] {
			if f == "-delete" || f == "-exec" || f == "-execdir" {
				return true
			}
		}
	}
	return false
}

// isTempPath reports whether p points strictly inside a standard temporary directory.
func isTempPath(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	clean := filepath.Clean(p)
	if strings.Contains(clean, "..") {
		return false
	}

	prefixes := []string{"/tmp/", "/private/tmp/", "/var/folders/"}
	if tmpDir := strings.TrimSpace(os.TempDir()); tmpDir != "" && tmpDir != "/" {
		prefixes = append(prefixes, strings.TrimRight(filepath.Clean(tmpDir), "/")+"/")
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(clean, prefix) {
			return true
		}
	}
	return false
}

// resolveTargetPath expands ~ and resolves relative paths against workDir.
func resolveTargetPath(raw, workDir string) string {
	p := strings.TrimSpace(raw)
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if p == "~" {
				p = home
			} else {
				p = filepath.Join(home, p[2:])
			}
		}
	}
	if !filepath.IsAbs(p) && workDir != "" {
		p = filepath.Join(workDir, p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return abs
}

// isWithinWorkDir reports whether targetPath resolves inside workDir (or equals workDir).
func isWithinWorkDir(targetPath, workDir string) bool {
	if workDir == "" || targetPath == "" {
		return true
	}
	absWork, err := filepath.Abs(workDir)
	if err != nil {
		absWork = filepath.Clean(workDir)
	}
	absTarget := resolveTargetPath(targetPath, absWork)
	rel, err := filepath.Rel(absWork, absTarget)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// evalPathOrClean resolves symlinks for existing paths (or nearest existing parent)
// to prevent symlink escape attacks.
func evalPathOrClean(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	dir := filepath.Dir(p)
	base := filepath.Base(p)
	if dir != p {
		if resolvedDir, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolvedDir, base)
		}
	}
	return filepath.Clean(p)
}

func isWithinAllowedBoundary(targetPath, workDir string, allowedDirs []string) bool {
	if len(allowedDirs) == 0 {
		absTarget := resolveTargetPath(targetPath, workDir)
		evalTarget := evalPathOrClean(absTarget)
		return isWithinWorkDir(evalTarget, evalPathOrClean(resolveTargetPath(".", workDir)))
	}
	absTarget := resolveTargetPath(targetPath, workDir)
	evalTarget := evalPathOrClean(absTarget)
	for _, dir := range allowedDirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		absAllowed := resolveTargetPath(dir, workDir)
		evalAllowed := evalPathOrClean(absAllowed)
		if isWithinWorkDir(evalTarget, evalAllowed) {
			return true
		}
	}
	return false
}

func matchesDeniedPath(targetPath, workDir string, deniedPaths []string) (string, bool) {
	if len(deniedPaths) == 0 || strings.TrimSpace(targetPath) == "" {
		return "", false
	}
	absTarget := resolveTargetPath(targetPath, workDir)
	evalTarget := evalPathOrClean(absTarget)
	base := filepath.Base(absTarget)

	for _, pat := range deniedPaths {
		p := strings.TrimSpace(pat)
		if p == "" {
			continue
		}
		// Glob or bare filename match (e.g. .env*, *.key, config.toml)
		if !strings.Contains(p, "/") {
			if ok, _ := filepath.Match(p, base); ok {
				return pat, true
			}
		}
		absPat := resolveTargetPath(p, workDir)
		evalPat := evalPathOrClean(absPat)
		if isWithinWorkDir(absTarget, absPat) || isWithinWorkDir(evalTarget, evalPat) {
			return pat, true
		}
		if ok, _ := filepath.Match(absPat, absTarget); ok {
			return pat, true
		}
	}
	return "", false
}

func extractToolPath(rawArgs json.RawMessage) string {
	var args struct {
		Path      string `json:"path"`
		FilePath  string `json:"file_path"`
		Dir       string `json:"dir"`
		Directory string `json:"directory"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return ""
	}
	for _, candidate := range []string{args.FilePath, args.Path, args.Dir, args.Directory} {
		if strings.TrimSpace(candidate) != "" {
			return strings.TrimSpace(candidate)
		}
	}
	return ""
}

// mcpCapabilityDecision checks kind=="mcp" capability rules for an mcp__* tool.
// Returns (hasMCPRules, allowed).
func (g *riskGuard) mcpCapabilityDecision(lowerTool string) (bool, bool) {
	hasMCPRule := false
	for _, c := range g.policy.Capabilities {
		if !strings.EqualFold(strings.TrimSpace(c.Kind), "mcp") {
			continue
		}
		hasMCPRule = true
		for _, a := range c.Allow {
			pat := strings.ToLower(strings.TrimSpace(a))
			if pat == "" {
				continue
			}
			if strings.HasSuffix(pat, "*") {
				if strings.HasPrefix(lowerTool, strings.TrimSuffix(pat, "*")) {
					return true, true
				}
			} else if lowerTool == pat {
				return true, true
			}
		}
	}
	return hasMCPRule, false
}

func (g *riskGuard) allowAgentDirFastPass() bool {
	return g.effectiveBashMode() == "full"
}

// shouldSkipCheck fast-passes strictly read-only commands inside workDir and operations confined to temporary directories.
func (g *riskGuard) shouldSkipCheck(toolName string, rawArgs json.RawMessage, workDir string) bool {
	lower := strings.ToLower(strings.TrimSpace(toolName))

	if strings.HasPrefix(lower, "mcp__") {
		if hasRule, allowed := g.mcpCapabilityDecision(lower); hasRule {
			return allowed
		}
	}

	if g.effectiveBashMode() == "disabled" && lower == "bash" {
		return false
	}
	if g.effectiveBashMode() == "scoped" && lower == "bash" {
		return g.matchesScopedBash(rawArgs, workDir)
	}
	if g.effectiveBashMode() == "full" && lower == "bash" && len(g.policy.Capabilities) > 0 {
		if g.matchesScopedBash(rawArgs, workDir) {
			return true
		}
	}
	if g.policy.AllowWrite != nil && !*g.policy.AllowWrite && (lower == "write" || lower == "edit") {
		return false
	}

	// Read-only discovery tools: fast-pass only if targeting allowed boundary, temp directory,
	// or (in full mode) trusted agent directories, and never matching denied_paths.
	if lower == "read" || lower == "grep" || lower == "find" || lower == "ls" {
		target := extractToolPath(rawArgs)
		if target == "" {
			target = "."
		}
		absTarget := resolveTargetPath(target, workDir)
		if _, denied := matchesDeniedPath(absTarget, workDir, g.policy.DeniedPaths); denied {
			return false
		}
		if g.allowAgentDirFastPass() && isAgentDirPath(absTarget, g.agentDirs) {
			return true
		}
		return isTempPath(absTarget) || isWithinAllowedBoundary(absTarget, workDir, g.policy.AllowedDirs)
	}

	// Write/edit operations targeting temporary directories pass directly (unless denied).
	if lower == "write" || lower == "edit" {
		target := extractToolPath(rawArgs)
		if target != "" {
			absTarget := resolveTargetPath(target, workDir)
			if _, denied := matchesDeniedPath(absTarget, workDir, g.policy.DeniedPaths); denied {
				return false
			}
			if isTempPath(absTarget) {
				return true
			}
		}
		return false
	}

	if lower != "bash" {
		return false
	}
	if len(g.policy.DeniedPaths) > 0 || len(g.policy.AllowedDirs) > 0 {
		// When role-level path policies are active, inspect bash tokens against deniedPaths & allowedDirs too.
		var bArgs struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(rawArgs, &bArgs); err == nil {
			for _, tok := range splitCommandFields(bArgs.Command) {
				cleanTok := strings.Trim(tok, `"'(),;`)
				if _, denied := matchesDeniedPath(cleanTok, workDir, g.policy.DeniedPaths); denied {
					return false
				}
			}
		}
	}

	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return false
	}
	cmd := strings.TrimSpace(args.Command)
	if cmd == "" {
		return true
	}

	// Strip harmless stderr redirects before analyzing
	cmd = strings.ReplaceAll(cmd, "2>&1", "")
	cmd = strings.ReplaceAll(cmd, "2>/dev/null", "")
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return true
	}

	segments, ok := splitShellSegments(cmd)
	if !ok || len(segments) == 0 {
		return false
	}
	var allowedAgentDirs []string
	if g.allowAgentDirFastPass() {
		allowedAgentDirs = g.agentDirs
	}
	for _, seg := range segments {
		if !isSafeSingleCommand(seg.cmd, seg.isPipeSink, workDir, g.allowAgentDirFastPass(), allowedAgentDirs) {
			return false
		}
	}
	return true
}

type shellSegment struct {
	cmd        string
	isPipeSink bool
}

// splitShellSegments splits a shell command on unquoted |, &&, ||, and ; while
// rejecting unquoted redirections, subshells, and newlines.
func splitShellSegments(cmd string) ([]shellSegment, bool) {
	var segments []shellSegment
	var buf strings.Builder
	inSingle := false
	inDouble := false
	escaped := false
	nextPipeSink := false

	flush := func(isPipe bool) {
		part := strings.TrimSpace(buf.String())
		buf.Reset()
		if part != "" {
			segments = append(segments, shellSegment{cmd: part, isPipeSink: nextPipeSink})
		}
		nextPipeSink = isPipe
	}

	for i := 0; i < len(cmd); i++ {
		ch := cmd[i]
		if escaped {
			buf.WriteByte(ch)
			escaped = false
			continue
		}
		if ch == '\\' && !inSingle {
			buf.WriteByte(ch)
			escaped = true
			continue
		}
		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			buf.WriteByte(ch)
			continue
		}
		if ch == '"' && !inSingle {
			inDouble = !inDouble
			buf.WriteByte(ch)
			continue
		}
		if inSingle || inDouble {
			buf.WriteByte(ch)
			continue
		}

		// Outside quotes: check unsafe operators
		switch ch {
		case '\n', '\r', '>', '<', '`':
			return nil, false
		case '$':
			if i+1 < len(cmd) && cmd[i+1] == '(' {
				return nil, false
			}
			buf.WriteByte(ch)
		case ';':
			flush(false)
		case '&':
			if i+1 < len(cmd) && cmd[i+1] == '&' {
				i++
				flush(false)
			} else {
				// Backgrounding '&' goes through audit
				return nil, false
			}
		case '|':
			if i+1 < len(cmd) && cmd[i+1] == '|' {
				i++
				flush(false)
			} else {
				flush(true)
			}
		default:
			buf.WriteByte(ch)
		}
	}

	if inSingle || inDouble {
		return nil, false
	}
	flush(false)
	return segments, true
}

// splitCommandFields tokenizes a single command respecting single/double quotes and backslash escapes.
func splitCommandFields(cmd string) []string {
	var fields []string
	var buf strings.Builder
	inSingle := false
	inDouble := false
	escaped := false
	hasToken := false

	for i := 0; i < len(cmd); i++ {
		ch := cmd[i]
		if escaped {
			buf.WriteByte(ch)
			escaped = false
			hasToken = true
			continue
		}
		if ch == '\\' && !inSingle {
			escaped = true
			hasToken = true
			continue
		}
		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			hasToken = true
			continue
		}
		if ch == '"' && !inSingle {
			inDouble = !inDouble
			hasToken = true
			continue
		}
		if !inSingle && !inDouble && (ch == ' ' || ch == '\t') {
			if hasToken {
				fields = append(fields, buf.String())
				buf.Reset()
				hasToken = false
			}
			continue
		}
		buf.WriteByte(ch)
		hasToken = true
	}
	if hasToken {
		fields = append(fields, buf.String())
	}
	return fields
}

func isReadOnlySed(fields []string) bool {
	hasSilent := false
	for _, f := range fields[1:] {
		if f == "--in-place" || strings.HasPrefix(f, "--in-place=") || f == "-i" || strings.HasPrefix(f, "-i") {
			return false
		}
		if f == "-n" || f == "--quiet" || f == "--silent" {
			hasSilent = true
		}
	}
	return hasSilent
}

func isSafeSingleCommand(cmd string, isPipeSink bool, workDir string, allowAgentDirs bool, agentDirs []string) bool {
	fields := splitCommandFields(cmd)
	if len(fields) == 0 {
		return true
	}
	bin := fields[0]

	if isPipeSink {
		switch bin {
		case "head", "tail", "grep", "rg", "wc", "sort", "uniq", "cut", "tr":
			return true
		case "sed":
			return isReadOnlySed(fields)
		default:
			return false
		}
	}

	// Any path-like argument in a read-only command must stay inside workDir, temp dirs, or (in full mode) agent dirs.
	checkArgsWithinBoundary := func(args []string) bool {
		for _, f := range args {
			if strings.HasPrefix(f, "-") {
				continue
			}
			if strings.HasPrefix(f, "/") || strings.HasPrefix(f, "~/") || f == "~" || strings.Contains(f, "..") {
				abs := resolveTargetPath(f, workDir)
				if !isTempPath(abs) && !isWithinWorkDir(abs, workDir) && !(allowAgentDirs && isAgentDirPath(abs, agentDirs)) {
					return false
				}
			}
		}
		return true
	}

	// Read-only inspection commands
	switch bin {
	case "ls", "pwd", "cat", "head", "tail", "rg", "grep", "find", "wc", "which", "echo", "stat", "file", "date", "uname", "du", "df", "sort", "uniq":
		if bin == "find" {
			for _, f := range fields[1:] {
				if f == "-delete" || f == "-exec" || f == "-execdir" {
					return false
				}
			}
		}
		return checkArgsWithinBoundary(fields[1:])
	case "sed":
		if !isReadOnlySed(fields) {
			return false
		}
		return checkArgsWithinBoundary(fields[1:])
	case "cd":
		if len(fields) == 1 {
			return false
		}
		if len(fields) == 2 {
			abs := resolveTargetPath(fields[1], workDir)
			return isTempPath(abs) || isWithinWorkDir(abs, workDir) || (allowAgentDirs && isAgentDirPath(abs, agentDirs))
		}
		return false
	case "git":
		if len(fields) >= 2 {
			switch fields[1] {
			case "status", "log", "diff", "show", "branch", "rev-parse":
				for _, f := range fields[2:] {
					if f == "-D" || f == "-d" || f == "-m" || f == "-M" {
						return false
					}
				}
				return checkArgsWithinBoundary(fields[2:])
			}
		}
	case "go":
		if len(fields) >= 2 {
			switch fields[1] {
			case "version", "env", "list", "doc", "test", "build", "vet":
				return checkArgsWithinBoundary(fields[2:])
			}
		}
	case "lark-cli":
		if len(fields) >= 3 && fields[1] == "im" && fields[2] == "+messages-send" {
			return true
		}
	}

	// Single-command file operations confined strictly to temporary directories
	if bin == "rm" || bin == "mkdir" || bin == "touch" || bin == "rmdir" {
		hasPath := false
		for _, f := range fields[1:] {
			if strings.HasPrefix(f, "-") {
				continue
			}
			hasPath = true
			if !isTempPath(resolveTargetPath(f, workDir)) {
				return false
			}
		}
		return hasPath
	}

	return false
}

func (g *riskGuard) effectiveBashMode() string {
	return effectiveBashMode(g.policy)
}

func (g *riskGuard) matchesScopedBash(rawArgs json.RawMessage, workDir string) bool {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return false
	}
	cmd := strings.TrimSpace(args.Command)
	if cmd == "" {
		return false
	}
	segments, ok := splitShellSegments(cmd)
	if !ok || len(segments) != 1 {
		return false
	}
	fields := splitCommandFields(segments[0].cmd)
	if len(fields) == 0 {
		return false
	}

	// Check deniedPaths on all tokens first
	for _, tok := range fields {
		cleanTok := strings.Trim(tok, `"'(),;`)
		if _, denied := matchesDeniedPath(cleanTok, workDir, g.policy.DeniedPaths); denied {
			return false
		}
	}

	var matchedCap *CapabilityRule
	for i := range g.policy.Capabilities {
		capRule := &g.policy.Capabilities[i]
		kind := strings.ToLower(strings.TrimSpace(capRule.Kind))
		if kind != "skill" && kind != "cli" {
			continue
		}
		if matchCapabilityCommand(segments[0].cmd, fields, workDir, capRule.Allow) {
			matchedCap = capRule
			break
		}
	}
	if matchedCap == nil {
		return false
	}

	// Verify all path-like arguments stay within /tmp, policy.AllowedDirs, or capability's Paths
	for i, tok := range fields {
		if i == 0 || strings.HasPrefix(tok, "-") {
			continue
		}
		cleanTok := strings.Trim(tok, `"'(),;`)
		if cleanTok == "" {
			continue
		}
		looksLikePath := strings.HasPrefix(cleanTok, "/") ||
			strings.HasPrefix(cleanTok, "~/") ||
			cleanTok == "~" ||
			strings.HasPrefix(cleanTok, "./") ||
			strings.HasPrefix(cleanTok, "../") ||
			cleanTok == ".." ||
			cleanTok == "." ||
			strings.Contains(cleanTok, "/")
		if !looksLikePath {
			continue
		}
		abs := resolveTargetPath(cleanTok, workDir)
		if isTempPath(abs) {
			continue
		}
		inAllowed := len(g.policy.AllowedDirs) > 0 && isWithinAllowedBoundary(abs, workDir, g.policy.AllowedDirs)
		inCap := len(matchedCap.Paths) > 0 && isWithinAllowedBoundary(abs, workDir, matchedCap.Paths)
		if !inAllowed && !inCap {
			return false
		}
	}
	return true
}

func matchCapabilityCommand(cmd string, fields []string, workDir string, allow []string) bool {
	if len(fields) == 0 || len(allow) == 0 {
		return false
	}
	bin := fields[0]
	absBin := resolveTargetPath(bin, workDir)
	for _, entry := range allow {
		e := strings.TrimSpace(entry)
		if e == "" {
			continue
		}
		// Exact binary or multi-word command prefix (e.g., "mmdc" or "python3 scripts/render.py")
		if cmd == e || strings.HasPrefix(cmd, e+" ") || bin == e {
			return true
		}
		// Directory prefix (e.g., "~/.gp/skills/diagram/scripts/")
		if strings.HasSuffix(e, "/") {
			absDir := resolveTargetPath(strings.TrimRight(e, "/"), workDir)
			if isWithinWorkDir(absBin, absDir) {
				return true
			}
		} else if absBin == resolveTargetPath(e, workDir) {
			return true
		}
	}
	return false
}

func (g *riskGuard) staticVerdict(toolName string, rawArgs json.RawMessage, workDir string) (*riskVerdict, bool) {
	lowerTool := strings.ToLower(strings.TrimSpace(toolName))

	if strings.HasPrefix(lowerTool, "mcp__") {
		if hasRule, allowed := g.mcpCapabilityDecision(lowerTool); hasRule {
			if allowed {
				return &riskVerdict{
					RiskLevel: "low",
					Allow:     true,
					Reason:    "命中已授权 MCP 能力白名单",
				}, true
			}
			return &riskVerdict{
				RiskLevel:  "high",
				Allow:      false,
				CanApprove: false,
				Reason:     fmt.Sprintf("当前角色仅允许调用已授权的 MCP 工具，禁止调用 %s", toolName),
			}, true
		}
	}

	if lowerTool == "bash" {
		switch g.effectiveBashMode() {
		case "disabled":
			return &riskVerdict{
				RiskLevel:  "high",
				Allow:      false,
				CanApprove: false,
				Reason:     "当前角色已禁用 bash 终端执行权限",
			}, true
		case "scoped":
			if g.matchesScopedBash(rawArgs, workDir) {
				return &riskVerdict{
					RiskLevel: "low",
					Allow:     true,
					Reason:    "命中已授权 Skill/CLI 能力白名单",
				}, true
			}
			return &riskVerdict{
				RiskLevel:  "high",
				Allow:      false,
				CanApprove: false,
				Reason:     "当前角色处于受限 bash 模式（scoped），仅允许执行已授权 Skill/CLI 能力内的命令",
			}, true
		}
	}
	if g.policy.AllowWrite != nil && !*g.policy.AllowWrite && (lowerTool == "write" || lowerTool == "edit") {
		return &riskVerdict{
			RiskLevel:  "high",
			Allow:      false,
			CanApprove: false,
			Reason:     "当前角色已禁用文件写入/修改权限",
		}, true
	}

	// Any read/grep/find/ls/write/edit targeting denied_paths (permanent deny) or outside allowed boundary requires approval.
	if lowerTool == "read" || lowerTool == "grep" || lowerTool == "find" || lowerTool == "ls" || lowerTool == "write" || lowerTool == "edit" {
		target := extractToolPath(rawArgs)
		effectiveTarget := target
		if effectiveTarget == "" {
			effectiveTarget = "."
		}
		absTarget := resolveTargetPath(effectiveTarget, workDir)
		if pat, denied := matchesDeniedPath(absTarget, workDir, g.policy.DeniedPaths); denied {
			return &riskVerdict{
				RiskLevel:  "high",
				Allow:      false,
				CanApprove: false,
				Reason:     fmt.Sprintf("命中敏感路径黑名单（%s），严禁访问", pat),
			}, true
		}
		isReadOnlyTool := lowerTool == "read" || lowerTool == "grep" || lowerTool == "find" || lowerTool == "ls"
		if !isTempPath(absTarget) && !isWithinAllowedBoundary(absTarget, workDir, g.policy.AllowedDirs) && !(g.allowAgentDirFastPass() && isReadOnlyTool && isAgentDirPath(absTarget, g.agentDirs)) {
			if target == "" && len(g.policy.AllowedDirs) > 0 {
				return &riskVerdict{
					RiskLevel:  "high",
					Allow:      false,
					CanApprove: false,
					Reason:     fmt.Sprintf("当前角色仅允许访问指定目录（%s），请显式传入允许目录下的路径", strings.Join(g.policy.AllowedDirs, ", ")),
				}, true
			}
			return &riskVerdict{
				RiskLevel:  "high",
				Allow:      false,
				CanApprove: true,
				Reason:     fmt.Sprintf("尝试访问或修改允许目录外的路径（%s），需要管理员确认", effectiveTarget),
			}, true
		}
		return nil, false
	}

	if lowerTool == "bash" && len(g.policy.DeniedPaths) > 0 {
		var bArgs struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(rawArgs, &bArgs); err == nil {
			for _, tok := range splitCommandFields(bArgs.Command) {
				cleanTok := strings.Trim(tok, `"'(),;`)
				if pat, denied := matchesDeniedPath(cleanTok, workDir, g.policy.DeniedPaths); denied {
					return &riskVerdict{
						RiskLevel:  "high",
						Allow:      false,
						CanApprove: false,
						Reason:     fmt.Sprintf("命令命中敏感路径黑名单（%s），严禁执行", pat),
					}, true
				}
			}
		}
	}

	return staticRiskVerdictWithAgentDirs(toolName, rawArgs, workDir, g.allowAgentDirFastPass(), g.agentDirs)
}

// staticRiskVerdict provides 0ms static classification for out-of-workspace access and unmistakably dangerous commands.
func staticRiskVerdict(toolName string, rawArgs json.RawMessage, workDir string) (*riskVerdict, bool) {
	return staticRiskVerdictWithAgentDirs(toolName, rawArgs, workDir, false, nil)
}

func staticRiskVerdictWithAgentDirs(toolName string, rawArgs json.RawMessage, workDir string, allowAgentDirs bool, agentDirs []string) (*riskVerdict, bool) {
	lowerTool := strings.ToLower(strings.TrimSpace(toolName))

	// Any read/grep/find/ls/write/edit targeting outside workDir (and not temp) requires admin approval,
	// except read-only tools targeting trusted agent directories in full mode.
	if lowerTool == "read" || lowerTool == "grep" || lowerTool == "find" || lowerTool == "ls" || lowerTool == "write" || lowerTool == "edit" {
		target := extractToolPath(rawArgs)
		if target != "" {
			absTarget := resolveTargetPath(target, workDir)
			isReadOnly := lowerTool == "read" || lowerTool == "grep" || lowerTool == "find" || lowerTool == "ls"
			if !isTempPath(absTarget) && !isWithinWorkDir(absTarget, workDir) && !(allowAgentDirs && isReadOnly && isAgentDirPath(absTarget, agentDirs)) {
				return &riskVerdict{
					RiskLevel:  "high",
					Allow:      false,
					CanApprove: true,
					Reason:     fmt.Sprintf("尝试访问或修改工作目录外的路径（%s），需要管理员确认", target),
				}, true
			}
		}
		return nil, false
	}

	if lowerTool != "bash" {
		return nil, false
	}
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, false
	}
	cmd := strings.TrimSpace(args.Command)
	lower := strings.ToLower(cmd)

	// Strictly non-approvable destructive/system-level commands
	for _, pat := range []string{"sudo ", "mkfs", "dd if=", "rm -rf /", "rm -rf ~", "git push --force", "git push -f", "git reset --hard"} {
		if strings.Contains(lower, pat) {
			return &riskVerdict{
				RiskLevel:  "high",
				Allow:      false,
				CanApprove: false,
				Reason:     fmt.Sprintf("包含高危不可逆指令（%s）", strings.TrimSpace(pat)),
			}, true
		}
	}

	// Simple non-temp rm triggers instant admin approval without waiting for LLM
	fields := strings.Fields(cmd)
	if len(fields) > 0 && fields[0] == "rm" {
		return &riskVerdict{
			RiskLevel:  "high",
			Allow:      false,
			CanApprove: true,
			Reason:     "包含本地文件删除（rm）操作，需要管理员确认",
		}, true
	}

	// Out-of-workspace path reference in bash triggers admin approval
	for _, f := range fields {
		cleanTok := strings.Trim(f, `"'(),;`)
		hasTraversal := cleanTok == ".." || strings.HasPrefix(cleanTok, "../") || strings.Contains(cleanTok, "/../") || strings.HasSuffix(cleanTok, "/..")
		if strings.HasPrefix(cleanTok, "/") || strings.HasPrefix(cleanTok, "~/") || cleanTok == "~" || hasTraversal {
			// Allow standard system binaries like /opt/homebrew/bin/..., /usr/bin/... as executable prefix (index 0),
			// but check data/path arguments against workDir & temp dirs.
			if cleanTok == fields[0] && (strings.HasPrefix(cleanTok, "/usr/") || strings.HasPrefix(cleanTok, "/bin/") || strings.HasPrefix(cleanTok, "/opt/homebrew/")) {
				continue
			}
			abs := resolveTargetPath(cleanTok, workDir)
			if !isTempPath(abs) && !isWithinWorkDir(abs, workDir) {
				if allowAgentDirs && isAgentDirPath(abs, agentDirs) && !isBashMutatingCommand(cmd) {
					continue
				}
				return &riskVerdict{
					RiskLevel:  "high",
					Allow:      false,
					CanApprove: true,
					Reason:     fmt.Sprintf("命令尝试访问或操作工作目录外的路径（%s），需要管理员确认", cleanTok),
				}, true
			}
		}
	}

	return nil, false
}

// extractResourceID extracts a remote resource identifier (e.g., document_id, wiki_token, chat_id, url host)
// for non-filesystem tools such as MCP or network tools.
func extractResourceID(rawArgs json.RawMessage) string {
	var args map[string]any
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return ""
	}
	for _, key := range []string{
		"document_id", "doc_id", "doc", "ref", "wiki_token", "node_token",
		"spreadsheet_token", "app_token", "table_id", "chat_id",
	} {
		if v, ok := args[key].(string); ok && strings.TrimSpace(v) != "" {
			return key + "=" + strings.TrimSpace(v)
		}
	}
	if rawURL, ok := args["url"].(string); ok && strings.TrimSpace(rawURL) != "" {
		if u, err := url.Parse(strings.TrimSpace(rawURL)); err == nil && u.Host != "" {
			return "host=" + u.Scheme + "://" + u.Host
		}
	}
	return ""
}

func isResourceCacheExcludedTool(lowerTool string) bool {
	for _, kw := range []string{"delete", "remove", "drop", "send", "mail", "purge", "destroy"} {
		if strings.Contains(lowerTool, kw) {
			return true
		}
	}
	return false
}

// buildApprovalCacheKey builds a short-lived key after an admin approves an action
// so repeated accesses to the same approved directory/resource within the same scope don't re-prompt.
// Destructive commands (like rm) are intentionally never cached.
func buildApprovalCacheKey(scope, toolName string, rawArgs json.RawMessage, workDir string) string {
	lowerTool := strings.ToLower(strings.TrimSpace(toolName))
	switch lowerTool {
	case "read", "grep", "find", "ls":
		if target := extractToolPath(rawArgs); target != "" {
			abs := resolveTargetPath(target, workDir)
			return scope + ":appr:fs-read:" + filepath.Dir(abs)
		}
		return ""
	case "write", "edit":
		if target := extractToolPath(rawArgs); target != "" {
			abs := resolveTargetPath(target, workDir)
			return scope + ":appr:" + lowerTool + ":" + abs
		}
		return ""
	case "bash":
		var args struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return ""
		}
		cmd := strings.TrimSpace(args.Command)
		fields := splitCommandFields(cmd)
		if len(fields) > 0 && fields[0] == "rm" {
			return ""
		}
		if cmd == "" {
			return ""
		}
		return scope + ":appr:bash:" + cmd
	default:
		if isResourceCacheExcludedTool(lowerTool) {
			return ""
		}
		if resID := extractResourceID(rawArgs); resID != "" {
			return scope + ":appr:" + lowerTool + ":" + resID
		}
		return ""
	}
}

// buildNormalizedCacheKey computes a safe normalized cache key scoped to (who, workDir, sessionID)
// for commands/tools whose risk profile depends on the command/subcommand/flags, target file path,
// or remote resource ID rather than search keywords, line ranges, or edited content.
// Interpreters/shells (python, bash -c, etc.) are intentionally excluded and always keyed by exact rawArgs.
func buildNormalizedCacheKey(scope, toolName string, rawArgs json.RawMessage, workDir string) string {
	lowerTool := strings.ToLower(strings.TrimSpace(toolName))
	if lowerTool == "write" || lowerTool == "edit" {
		if target := extractToolPath(rawArgs); target != "" {
			abs := resolveTargetPath(target, workDir)
			if isTempPath(abs) || isWithinWorkDir(abs, workDir) {
				return scope + ":" + lowerTool + ":path:" + abs
			}
		}
		return ""
	}
	if lowerTool != "bash" {
		if !isResourceCacheExcludedTool(lowerTool) {
			if resID := extractResourceID(rawArgs); resID != "" {
				return scope + ":" + lowerTool + ":res:" + resID
			}
		}
		return ""
	}

	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return ""
	}
	cmd := strings.TrimSpace(args.Command)
	cmd = strings.ReplaceAll(cmd, "2>&1", "")
	cmd = strings.ReplaceAll(cmd, "2>/dev/null", "")
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}

	segments, ok := splitShellSegments(cmd)
	if !ok || len(segments) == 0 {
		return ""
	}

	var sigParts []string
	for _, seg := range segments {
		sig := normalizeCommandSignature(seg.cmd)
		if sig == "" {
			return ""
		}
		sigParts = append(sigParts, sig)
	}
	return scope + ":bash:sig:" + strings.Join(sigParts, "|")
}

func normalizeCommandSignature(cmd string) string {
	fields := splitCommandFields(cmd)
	if len(fields) == 0 {
		return ""
	}
	bin := filepath.Base(fields[0])
	switch bin {
	case "grep", "rg", "ag", "cat", "head", "tail", "less", "wc",
		"ls", "tree", "stat", "file", "diff", "sort", "uniq", "cut", "tr", "jq", "yq",
		"pytest", "make", "cd":
	case "sed":
		for _, f := range fields[1:] {
			if f == "-i" || strings.HasPrefix(f, "-i") || strings.HasPrefix(f, "--in-place") {
				return ""
			}
		}
	case "find":
		for _, f := range fields[1:] {
			if f == "-delete" || f == "-exec" || f == "-execdir" || f == "-ok" {
				return ""
			}
		}
	case "go", "git", "npm", "pnpm", "yarn", "cargo":
		// Subcommand-based CLIs include the first non-flag token as subcommand
	default:
		return ""
	}

	var flags []string
	subcmd := ""
	hasSubcmdCLI := bin == "go" || bin == "git" || bin == "npm" || bin == "pnpm" || bin == "yarn" || bin == "cargo"
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "-") {
			flags = append(flags, f)
			continue
		}
		if hasSubcmdCLI && subcmd == "" {
			subcmd = f
		}
	}
	if subcmd != "" {
		return bin + ":" + subcmd + ":" + strings.Join(flags, ",")
	}
	return bin + ":" + strings.Join(flags, ",")
}

func (g *riskGuard) assess(ctx context.Context, toolName string, rawArgs json.RawMessage, workDir string) (*riskVerdict, error) {
	scope := g.scopePrefix()
	lowerTool := strings.ToLower(strings.TrimSpace(toolName))
	cacheKey := scope + ":" + lowerTool + ":" + strings.TrimSpace(string(rawArgs))
	if cached, ok := g.cache.get(cacheKey); ok {
		return cached, nil
	}
	normKey := buildNormalizedCacheKey(scope, lowerTool, rawArgs, workDir)
	if normKey != "" {
		if cached, ok := g.cache.get(normKey); ok {
			return cached, nil
		}
	}

	started := time.Now()
	agentLabel := "安全审计 (risk_guard)"

	if g.emit != nil {
		g.emit(Event{
			ID:        g.reqID,
			Event:     EventSubagentStart,
			AgentName: agentLabel,
			Text:      fmt.Sprintf("正在评估工具 `%s` 的执行风险", toolName),
		})
	}

	subCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	// Security reviewer runs with an empty tool registry (read-only reasoning only)
	reviewer := agent.NewAgent("risk_guard", "security-reviewer", g.prov, g.modelName, tools.NewRegistry())
	reviewer.SetSystemPrompt(`你是一个严格的操作安全审计子 Agent。你的唯一职责是评估主 Agent 即将调用的工具操作是否存在安全风险，并输出 JSON 决策。

重要原则：
- **即使用户自己在对话中明确要求删除、修改代码/文档，只要该操作属于高风险破坏性范畴，也必须判定为拒绝（allow: false）**！因为飞书群聊机器人环境不具备高权限人工确认能力。
- **临时文件夹豁免**：仅针对系统临时目录（/tmp/、/private/tmp/、/var/folders/）内的创建、写入或清理操作，属于低风险，应予以放行（allow: true）。
- **飞书发消息/私聊豁免**：当用户明确要求向指定同事、群成员或群聊发送飞书消息/私聊（例如调用 feishu_send_message 或 lark-cli im +messages-send）时，属于机器人正常交互能力，应判定为中低风险并放行（risk_level: "medium", allow: true）；但若存在明显群发轰炸、泄露凭证密钥或非用户授权的对外发送，则应拒绝。

评判标准：
1. **高风险且不合规操作 (risk_level: "high", allow: false, can_approve: false)** —— 直接拒绝，不允许审批：
   - 破坏性系统级操作：如 sudo、mkfs、dd、chmod 777、rm -rf 系统目录或整个仓库；
   - 破坏性 Git 操作：如 git push --force、git reset --hard；
   - 泄露凭证/密钥、社工欺诈、恶意群发或通过脚本/codemode 绕过安全限制。
2. **高风险但可由管理员确认的操作 (risk_level: "high", allow: false, can_approve: true)** —— 需要群内管理员确认后方可执行：
   - 局部删除普通文件、修改或更新外部共享文档（edit_document、replace_block、update_block、delete_block）、覆盖写关键配置文件等。
3. **中低风险 (risk_level: "medium" 或 "low", allow: true, can_approve: false)**：
   - 临时目录（/tmp/、/var/folders/）内的操作；
   - 按用户要求发送飞书私聊或群消息（feishu_send_message / lark-cli im +messages-send）；
   - 常规只读探测、安全的局部开发测试或明确无破坏性的操作。

必须严格只输出以下格式的合法 JSON（不要输出 markdown 代码块外的任何多余文字）：
{"risk_level":"high|medium|low","allow":true|false,"can_approve":true|false,"reason":"简明中文原因","required_permission":"如拒绝，说明所需权限"}`)

	whoDesc := "普通成员"
	if g.caller.IsBoss {
		whoDesc = "管理员 (boss)"
	}
	if strings.TrimSpace(g.caller.SenderID) != "" {
		whoDesc += " [" + strings.TrimSpace(g.caller.SenderID) + "]"
	}
	auditPrompt := fmt.Sprintf("发起人：%s\n用户原始请求：%s\n\n待审计工具：%s\n工具参数：%s\n\n请评估风险等级并给出 JSON 决策。",
		whoDesc, g.userPrompt, toolName, string(rawArgs))

	ans, err := reviewer.Prompt(subCtx, auditPrompt)
	if err == nil && strings.TrimSpace(ans) == "" {
		// Retry once if the model returned an empty response
		ans, err = reviewer.Prompt(subCtx, auditPrompt+"\n\n注意：上次返回为空，请直接输出一行合法 JSON。")
	}
	dur := time.Since(started).Round(time.Millisecond).String()

	if err != nil {
		if g.emit != nil {
			g.emit(Event{
				ID:        g.reqID,
				Event:     EventSubagentEnd,
				AgentName: agentLabel,
				Message:   err.Error(),
				Summary:   dur,
			})
		}
		return nil, err
	}

	verdict, parseErr := parseRiskVerdict(ans)
	if parseErr != nil {
		if g.emit != nil {
			g.emit(Event{
				ID:        g.reqID,
				Event:     EventSubagentEnd,
				AgentName: agentLabel,
				Message:   parseErr.Error(),
				Summary:   dur,
			})
		}
		return nil, parseErr
	}

	g.cache.set(cacheKey, verdict)
	if normKey != "" && verdict.Allow && !strings.EqualFold(verdict.RiskLevel, "high") {
		g.cache.set(normKey, verdict)
	}

	if g.emit != nil {
		if !verdict.Allow || strings.EqualFold(verdict.RiskLevel, "high") {
			var msg string
			if verdict.CanApprove && g.caller.AllowApproval && g.requestApproval != nil {
				msg = fmt.Sprintf("🔐 **需管理员确认**（工具：`%s`，风险等级：`%s`）\n- **原因**：%s\n- **状态**：已发起管理员审批，等待确认后继续执行",
					toolName, verdict.RiskLevel, verdict.Reason)
			} else {
				perm := verdict.RequiredPermission
				if perm == "" {
					perm = "需要更高级别权限或人工确认"
				}
				msg = fmt.Sprintf("⛔ **高风险操作已拦截**（工具：`%s`，风险等级：`%s`）\n- **原因**：%s\n- **权限要求**：%s\n- **操作参数**：\n```json\n%s\n```",
					toolName, verdict.RiskLevel, verdict.Reason, perm, strings.TrimSpace(string(rawArgs)))
			}
			g.emit(Event{
				ID:        g.reqID,
				Event:     EventSubagentEnd,
				AgentName: agentLabel,
				Text:      msg,
				Summary:   dur,
			})
		} else {
			msg := fmt.Sprintf("✅ **安全审计通过**（工具：`%s`，风险等级：`%s`）\n- **说明**：%s",
				toolName, verdict.RiskLevel, verdict.Reason)
			g.emit(Event{
				ID:        g.reqID,
				Event:     EventSubagentEnd,
				AgentName: agentLabel,
				Text:      msg,
				Summary:   dur,
			})
		}
	}

	return verdict, nil
}

func parseRiskVerdict(raw string) (*riskVerdict, error) {
	cleaned := strings.TrimSpace(raw)
	if cleaned == "" {
		return nil, fmt.Errorf("安全审计模型返回空内容")
	}
	if idx := strings.Index(cleaned, "{"); idx >= 0 {
		if end := strings.LastIndex(cleaned, "}"); end > idx {
			cleaned = cleaned[idx : end+1]
		}
	}
	var v riskVerdict
	if err := json.Unmarshal([]byte(cleaned), &v); err != nil {
		return nil, fmt.Errorf("解析安全审计结果失败 (%w): %s", err, raw)
	}
	if v.RiskLevel == "" {
		v.RiskLevel = "high"
	}
	return &v, nil
}
