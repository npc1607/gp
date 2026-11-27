package extensions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gp/internal/ai"
)

const APIVersionV1 = "gp.ext/v1"

var (
	extNameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	actionNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
	envExprRe    = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)
)

// ExtensionSpec 对应 extension.yaml 的完整声明清单
type ExtensionSpec struct {
	APIVersion  string          `yaml:"api_version" json:"api_version"`
	Name        string          `yaml:"name" json:"name"`
	Version     string          `yaml:"version,omitempty" json:"version,omitempty"`
	Description string          `yaml:"description" json:"description"`
	Triggers    []string        `yaml:"triggers,omitempty" json:"triggers,omitempty"`
	Guide       string          `yaml:"guide,omitempty" json:"guide,omitempty"`
	Enabled     *bool           `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Governance  GovernanceSpec  `yaml:"governance,omitempty" json:"governance,omitempty"`
	Auth        AuthSpec        `yaml:"auth,omitempty" json:"auth,omitempty"`
	HealthCheck HealthCheckSpec `yaml:"healthcheck,omitempty" json:"healthcheck,omitempty"`
	Adapter     AdapterSpec     `yaml:"adapter" json:"adapter"`
	Actions     []ActionSpec    `yaml:"actions,omitempty" json:"actions,omitempty"`

	// 运行时元数据（由 Loader 填充，不参与 YAML 序列化）
	Dir                string   `yaml:"-" json:"dir,omitempty"`
	WorkspaceCwd       string   `yaml:"-" json:"-"`
	FilePath           string   `yaml:"-" json:"file_path,omitempty"`
	Source             string   `yaml:"-" json:"source,omitempty"` // "global" | "custom" | "project" | "legacy-skill" | "legacy-mcp"
	GuideText          string   `yaml:"-" json:"-"`
	ResolvedEnvSecrets []string `yaml:"-" json:"-"`
}

// GovernanceSpec 声明能力包级安全、子 Agent 权限、超时与输出防污染预算
type GovernanceSpec struct {
	DefaultRisk    RiskLevel `yaml:"default_risk,omitempty" json:"default_risk,omitempty"`
	SubagentPolicy string    `yaml:"subagent_policy,omitempty" json:"subagent_policy,omitempty"` // inherit | read_only | deny
	OutputMaxBytes int       `yaml:"output_max_bytes,omitempty" json:"output_max_bytes,omitempty"`
	OutputMaxLines int       `yaml:"output_max_lines,omitempty" json:"output_max_lines,omitempty"`
	Timeout        string    `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// AuthSpec 声明统一鉴权规则（禁止明文写入密钥）
type AuthSpec struct {
	Type       string `yaml:"type,omitempty" json:"type,omitempty"`               // none | bearer | header | basic | query
	Header     string `yaml:"header,omitempty" json:"header,omitempty"`           // 自定义 Header 名
	QueryParam string `yaml:"query_param,omitempty" json:"query_param,omitempty"` // type=query 时必填
	Prefix     string `yaml:"prefix,omitempty" json:"prefix,omitempty"`           // type=bearer 默认 "Bearer "
	TokenRef   string `yaml:"token_ref,omitempty" json:"token_ref,omitempty"`     // env:// | auth:// | cmd://
	Required   bool   `yaml:"required,omitempty" json:"required,omitempty"`
}

// HealthCheckSpec 声明 `/ext test <name>` 连通性探测方式
type HealthCheckSpec struct {
	HTTPGet string   `yaml:"http_get,omitempty" json:"http_get,omitempty"`
	CLIArgs []string `yaml:"cli_args,omitempty" json:"cli_args,omitempty"`
	Timeout string   `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// AdapterSpec 声明底层协议适配器参数
type AdapterSpec struct {
	Type string `yaml:"type" json:"type"` // guide | cli | script | http | openapi | mcp

	// cli / script
	Command     string            `yaml:"command,omitempty" json:"command,omitempty"`
	Exec        string            `yaml:"exec,omitempty" json:"exec,omitempty"`
	Interpreter string            `yaml:"interpreter,omitempty" json:"interpreter,omitempty"`
	Cwd         string            `yaml:"cwd,omitempty" json:"cwd,omitempty"`
	Env         map[string]string `yaml:"env,omitempty" json:"env,omitempty"`

	// http / openapi
	BaseURL        string               `yaml:"base_url,omitempty" json:"base_url,omitempty"`
	DefaultHeaders map[string]string    `yaml:"default_headers,omitempty" json:"default_headers,omitempty"`
	SpecFile       string               `yaml:"spec_file,omitempty" json:"spec_file,omitempty"`
	SpecURL        string               `yaml:"spec_url,omitempty" json:"spec_url,omitempty"`
	IncludeTags    []string             `yaml:"include_tags,omitempty" json:"include_tags,omitempty"`
	ExcludePaths   []string             `yaml:"exclude_paths,omitempty" json:"exclude_paths,omitempty"`
	MethodRiskMap  map[string]RiskLevel `yaml:"method_risk_map,omitempty" json:"method_risk_map,omitempty"`

	// mcp
	Transport    string   `yaml:"transport,omitempty" json:"transport,omitempty"` // stdio | streamable_http
	Args         []string `yaml:"args,omitempty" json:"args,omitempty"`
	URL          string   `yaml:"url,omitempty" json:"url,omitempty"`
	IdleTTL      string   `yaml:"idle_ttl,omitempty" json:"idle_ttl,omitempty"`
	IncludeTools []string `yaml:"include_tools,omitempty" json:"include_tools,omitempty"`
	ExcludeTools []string `yaml:"exclude_tools,omitempty" json:"exclude_tools,omitempty"`

	// 公共超时（可覆盖）
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// ActionSpec 声明单个可执行动作的契约
type ActionSpec struct {
	Name        string               `yaml:"name" json:"name"`
	Description string               `yaml:"description" json:"description"`
	Risk        RiskLevel            `yaml:"risk,omitempty" json:"risk,omitempty"`
	Timeout     string               `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Tags        []string             `yaml:"tags,omitempty" json:"tags,omitempty"`
	Params      map[string]ParamSpec `yaml:"params,omitempty" json:"params,omitempty"`

	// cli 专属
	Args   []string `yaml:"args,omitempty" json:"args,omitempty"`
	Output string   `yaml:"output,omitempty" json:"output,omitempty"` // text | json

	// http 专属
	Method      string            `yaml:"method,omitempty" json:"method,omitempty"`
	Path        string            `yaml:"path,omitempty" json:"path,omitempty"`
	QueryParams []string          `yaml:"query_params,omitempty" json:"query_params,omitempty"`
	Headers     map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	BodyMode    string            `yaml:"body_mode,omitempty" json:"body_mode,omitempty"`   // json | json_body | raw | none
	BodyParam   string            `yaml:"body_param,omitempty" json:"body_param,omitempty"` // json_body / raw 模式下承载请求体的参数名（默认 "body"）
	Extract     string            `yaml:"extract,omitempty" json:"extract,omitempty"`
}

// ParamSpec 声明动作的单个参数规范
type ParamSpec struct {
	Type        string `yaml:"type" json:"type"` // string | integer | number | boolean | array | object
	Required    bool   `yaml:"required,omitempty" json:"required,omitempty"`
	Default     any    `yaml:"default,omitempty" json:"default,omitempty"`
	Enum        []any  `yaml:"enum,omitempty" json:"enum,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

func (s *ExtensionSpec) IsDefaultEnabled() bool {
	if s.Enabled == nil {
		return true
	}
	return *s.Enabled
}

func (s *ExtensionSpec) EffectiveDefaultRisk() RiskLevel {
	switch s.Governance.DefaultRisk {
	case RiskRead, RiskWrite, RiskDestructive:
		return s.Governance.DefaultRisk
	default:
		return RiskWrite
	}
}

func (s *ExtensionSpec) EffectiveSubagentPolicy() string {
	switch strings.ToLower(strings.TrimSpace(s.Governance.SubagentPolicy)) {
	case "inherit", "read_only", "deny":
		return strings.ToLower(strings.TrimSpace(s.Governance.SubagentPolicy))
	default:
		return "read_only"
	}
}

func (s *ExtensionSpec) EffectiveOutputMaxBytes() int {
	if s.Governance.OutputMaxBytes > 0 {
		return s.Governance.OutputMaxBytes
	}
	return 16384
}

func (s *ExtensionSpec) EffectiveOutputMaxLines() int {
	if s.Governance.OutputMaxLines > 0 {
		return s.Governance.OutputMaxLines
	}
	return 200
}

func (s *ExtensionSpec) EffectiveTimeout(actionTimeout string) time.Duration {
	for _, raw := range []string{actionTimeout, s.Adapter.Timeout, s.Governance.Timeout} {
		if d, err := time.ParseDuration(strings.TrimSpace(raw)); err == nil && d > 0 {
			if d > 5*time.Minute {
				return 5 * time.Minute
			}
			return d
		}
	}
	return 30 * time.Second
}

func (s *ExtensionSpec) EffectiveIdleTTL() time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(s.Adapter.IdleTTL)); err == nil && d > 0 {
		return d
	}
	return 10 * time.Minute
}

// ExpandEnvPlaceholders 仅展开 ${VAR} 与 ${VAR:-default}，不执行 shell 命令
func ExpandEnvPlaceholders(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return envExprRe.ReplaceAllStringFunc(s, func(match string) string {
		sub := envExprRe.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		val, ok := os.LookupEnv(sub[1])
		if ok && val != "" {
			return val
		}
		if len(sub) >= 3 {
			return sub[2]
		}
		return ""
	})
}

func isSensitiveEnvName(name string) bool {
	upper := strings.ToUpper(name)
	for _, kw := range []string{"TOKEN", "SECRET", "KEY", "PASSWORD", "PASSWD", "AUTH", "CREDENTIAL"} {
		if strings.Contains(upper, kw) {
			return true
		}
	}
	return false
}

// expandAndCollectSecrets 展开 ${VAR:-default} 并收集敏感环境变量值供 SecretScrubber 脱敏
func expandAndCollectSecrets(s string, secrets *[]string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return envExprRe.ReplaceAllStringFunc(s, func(match string) string {
		sub := envExprRe.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		varName := sub[1]
		val, ok := os.LookupEnv(varName)
		if !ok || val == "" {
			if len(sub) >= 3 {
				val = sub[2]
			} else {
				val = ""
			}
		}
		if val != "" && secrets != nil && isSensitiveEnvName(varName) {
			*secrets = append(*secrets, val)
		}
		return val
	})
}

// ExpandSpecEnv 对清单中允许使用环境变量的配置项进行展开，同时收集敏感变量值用于后续脱敏
func (s *ExtensionSpec) ExpandSpecEnv() {
	var secrets []string
	s.Adapter.BaseURL = expandAndCollectSecrets(s.Adapter.BaseURL, &secrets)
	s.Adapter.URL = expandAndCollectSecrets(s.Adapter.URL, &secrets)
	s.Adapter.SpecURL = expandAndCollectSecrets(s.Adapter.SpecURL, &secrets)
	s.Adapter.SpecFile = expandAndCollectSecrets(s.Adapter.SpecFile, &secrets)
	s.Adapter.Cwd = expandAndCollectSecrets(s.Adapter.Cwd, &secrets)
	s.Adapter.Command = expandAndCollectSecrets(s.Adapter.Command, &secrets)
	for i := range s.Adapter.Args {
		s.Adapter.Args[i] = expandAndCollectSecrets(s.Adapter.Args[i], &secrets)
	}
	for k, v := range s.Adapter.Env {
		expanded := expandAndCollectSecrets(v, &secrets)
		if expanded != "" && isSensitiveEnvName(k) {
			secrets = append(secrets, expanded)
		}
		s.Adapter.Env[k] = expanded
	}
	for k, v := range s.Adapter.DefaultHeaders {
		expanded := expandAndCollectSecrets(v, &secrets)
		if expanded != "" && isSensitiveEnvName(k) {
			secrets = append(secrets, expanded)
		}
		s.Adapter.DefaultHeaders[k] = expanded
	}
	for i := range s.Actions {
		for k, v := range s.Actions[i].Headers {
			expanded := expandAndCollectSecrets(v, &secrets)
			if expanded != "" && isSensitiveEnvName(k) {
				secrets = append(secrets, expanded)
			}
			s.Actions[i].Headers[k] = expanded
		}
	}
	s.ResolvedEnvSecrets = secrets
}

// Validate 校验 extension.yaml 的必填字段、命名合法性、鉴权安全与适配器完整性
func (s *ExtensionSpec) Validate() error {
	if s.APIVersion != "" && s.APIVersion != APIVersionV1 {
		return fmt.Errorf("unsupported api_version %q (want %q)", s.APIVersion, APIVersionV1)
	}
	if !extNameRe.MatchString(s.Name) {
		return fmt.Errorf("invalid extension name %q (must match %s)", s.Name, extNameRe.String())
	}
	if strings.TrimSpace(s.Description) == "" {
		return fmt.Errorf("extension %q missing required field: description", s.Name)
	}

	// 校验 auth 配置合法性并提前拦截明文密钥
	authType := strings.ToLower(strings.TrimSpace(s.Auth.Type))
	switch authType {
	case "", "none":
	case "bearer", "basic":
	case "header":
		if strings.TrimSpace(s.Auth.Header) == "" {
			return fmt.Errorf("extension %q auth.type=header requires auth.header", s.Name)
		}
	case "query":
		if strings.TrimSpace(s.Auth.QueryParam) == "" {
			return fmt.Errorf("extension %q auth.type=query requires auth.query_param", s.Name)
		}
	default:
		return fmt.Errorf("extension %q unsupported auth.type %q", s.Name, s.Auth.Type)
	}
	if ref := strings.TrimSpace(s.Auth.TokenRef); ref != "" {
		if !strings.HasPrefix(ref, "env://") && !strings.HasPrefix(ref, "auth://") && !strings.HasPrefix(ref, "cmd://") {
			return fmt.Errorf("extension %q invalid auth.token_ref %q: must use env://, auth://, or cmd:// (plaintext secrets are forbidden)", s.Name, ref)
		}
	}

	switch s.Adapter.Type {
	case "guide":
		// 纯文档扩展
	case "cli":
		if strings.TrimSpace(s.Adapter.Command) == "" {
			return fmt.Errorf("extension %q (cli) missing adapter.command", s.Name)
		}
	case "script":
		if strings.TrimSpace(s.Adapter.Exec) == "" {
			return fmt.Errorf("extension %q (script) missing adapter.exec", s.Name)
		}
	case "http":
		if strings.TrimSpace(s.Adapter.BaseURL) == "" {
			return fmt.Errorf("extension %q (http) missing adapter.base_url", s.Name)
		}
	case "openapi":
		if strings.TrimSpace(s.Adapter.SpecFile) == "" && strings.TrimSpace(s.Adapter.SpecURL) == "" {
			return fmt.Errorf("extension %q (openapi) requires adapter.spec_file or adapter.spec_url", s.Name)
		}
	case "mcp":
		tr := strings.ToLower(strings.TrimSpace(s.Adapter.Transport))
		if tr == "" || tr == "stdio" {
			if strings.TrimSpace(s.Adapter.Command) == "" {
				return fmt.Errorf("extension %q (mcp stdio) missing adapter.command", s.Name)
			}
		} else if tr == "streamable_http" || tr == "http" {
			return fmt.Errorf("extension %q mcp transport %q is not yet implemented (use stdio)", s.Name, s.Adapter.Transport)
		} else {
			return fmt.Errorf("extension %q unsupported mcp transport %q", s.Name, s.Adapter.Transport)
		}
	default:
		return fmt.Errorf("extension %q unsupported adapter.type %q", s.Name, s.Adapter.Type)
	}

	seenActions := make(map[string]bool, len(s.Actions))
	for _, a := range s.Actions {
		if !actionNameRe.MatchString(a.Name) {
			return fmt.Errorf("extension %q invalid action name %q", s.Name, a.Name)
		}
		if seenActions[a.Name] {
			return fmt.Errorf("extension %q duplicate action %q", s.Name, a.Name)
		}
		seenActions[a.Name] = true
	}
	return nil
}

// safeJoinUnderDir 确保相对路径经词法与符号链接解析后严格位于 baseDir 之内，防止路径穿越与软链逃逸
func safeJoinUnderDir(baseDir, relPath string) (string, error) {
	if strings.TrimSpace(relPath) == "" {
		return "", fmt.Errorf("empty relative path")
	}
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("absolute path %q not allowed inside extension package", relPath)
	}
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}
	target := filepath.Join(absBase, filepath.Clean(relPath))
	if !isSubPath(absBase, target) {
		return "", fmt.Errorf("path %q escapes extension directory", relPath)
	}
	realBase, err := filepath.EvalSymlinks(absBase)
	if err != nil {
		realBase = absBase
	}
	realTarget, err := resolveRealPath(target)
	if err != nil {
		return "", fmt.Errorf("resolve symlink for %q: %w", relPath, err)
	}
	if !isSubPath(realBase, realTarget) {
		return "", fmt.Errorf("symlink target %q escapes extension directory", relPath)
	}
	return realTarget, nil
}

// safeResolveSpecFile 校验 openapi.spec_file 必须位于扩展目录或当前工作区目录之内（防任意文件读取与软链逃逸）
func safeResolveSpecFile(extDir, workspaceCwd, specFile string) (string, error) {
	sf := strings.TrimSpace(specFile)
	if sf == "" {
		return "", fmt.Errorf("empty spec_file")
	}
	if filepath.IsAbs(sf) {
		return "", fmt.Errorf("absolute openapi spec_file %q is forbidden", sf)
	}
	base := extDir
	if base == "" {
		base = workspaceCwd
	}
	if base == "" {
		return "", fmt.Errorf("no allowed base directory for openapi spec_file")
	}
	absTarget, err := filepath.Abs(filepath.Join(base, filepath.Clean(sf)))
	if err != nil {
		return "", err
	}
	realTarget, err := resolveRealPath(absTarget)
	if err != nil {
		return "", err
	}
	allowedBases := []string{}
	for _, b := range []string{extDir, workspaceCwd} {
		if strings.TrimSpace(b) == "" {
			continue
		}
		ab, aErr := filepath.Abs(b)
		if aErr != nil {
			continue
		}
		if rb, rErr := filepath.EvalSymlinks(ab); rErr == nil {
			allowedBases = append(allowedBases, rb)
		} else {
			allowedBases = append(allowedBases, ab)
		}
	}
	for _, rb := range allowedBases {
		if isSubPath(rb, realTarget) {
			return realTarget, nil
		}
	}
	return "", fmt.Errorf("openapi spec_file %q escapes extension and workspace boundaries", specFile)
}

func isSubPath(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func resolveRealPath(target string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		return resolved, nil
	}
	// 若目标文件尚未存在，解析其存在的最长父目录再拼回后缀
	dir := filepath.Dir(target)
	base := filepath.Base(target)
	if dir == target {
		return target, nil
	}
	resolvedDir, err := resolveRealPath(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedDir, base), nil
}

// BuildActionSchema 将 ActionSpec.Params 编译为标准化、经 ai.SanitizeToolSchema 清洗的 JSON Schema
func BuildActionSchema(params map[string]ParamSpec) json.RawMessage {
	if len(params) == 0 {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	props := make(map[string]any, len(params))
	var required []string
	names := make([]string, 0, len(params))
	for k := range params {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		p := params[k]
		typ := strings.TrimSpace(p.Type)
		if typ == "" {
			typ = "string"
		}
		entry := map[string]any{"type": typ}
		if typ == "array" {
			entry["items"] = map[string]any{}
		}
		if p.Description != "" {
			entry["description"] = p.Description
		}
		if p.Default != nil {
			entry["default"] = p.Default
		}
		if len(p.Enum) > 0 {
			entry["enum"] = p.Enum
		}
		props[k] = entry
		if p.Required {
			required = append(required, k)
		}
	}
	schema := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return ai.SanitizeToolSchema(raw)
}
