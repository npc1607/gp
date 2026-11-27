package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"gp/internal/ai"

	"gopkg.in/yaml.v3"
)

// openapiAdapter 自动解析 OpenAPI 3.0 YAML/JSON（含本地 #/components/schemas/$ref 解析）生成动作目录并复用 HTTP 执行引擎
type openapiAdapter struct {
	spec    *ExtensionSpec
	client  *http.Client
	mu      sync.RWMutex
	loaded  bool
	baseURL string
	defs    []ActionDef
	actions map[string]ActionSpec
}

func newOpenAPIAdapter(spec *ExtensionSpec) Adapter {
	return &openapiAdapter{
		spec:    spec,
		client:  &http.Client{},
		baseURL: spec.Adapter.BaseURL,
		actions: make(map[string]ActionSpec),
	}
}

func (a *openapiAdapter) Type() string { return "openapi" }

func (a *openapiAdapter) ensureLoaded(ctx context.Context) error {
	a.mu.RLock()
	if a.loaded {
		a.mu.RUnlock()
		return nil
	}
	a.mu.RUnlock()

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loaded {
		return nil
	}

	raw, err := a.loadSpecBytes(ctx)
	if err != nil {
		return err
	}

	var doc struct {
		Servers []struct {
			URL string `yaml:"url" json:"url"`
		} `yaml:"servers" json:"servers"`
		Components struct {
			Schemas map[string]map[string]any `yaml:"schemas" json:"schemas"`
		} `yaml:"components" json:"components"`
		Paths map[string]map[string]any `yaml:"paths" json:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse openapi spec: %w", err)
	}
	resolvedBase := a.baseURL
	if resolvedBase == "" && len(doc.Servers) > 0 {
		resolvedBase = doc.Servers[0].URL
	}

	includeTags := make(map[string]bool, len(a.spec.Adapter.IncludeTags))
	for _, t := range a.spec.Adapter.IncludeTags {
		includeTags[strings.ToLower(strings.TrimSpace(t))] = true
	}

	paths := make([]string, 0, len(doc.Paths))
	for p := range doc.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var defs []ActionDef
	actions := make(map[string]ActionSpec)
	for _, path := range paths {
		if isPathExcluded(path, a.spec.Adapter.ExcludePaths) {
			continue
		}
		pathItem := doc.Paths[path]
		pathLevelParams := parseOpenAPIParams(pathItem["parameters"], doc.Components.Schemas)

		methodKeys := make([]string, 0, len(pathItem))
		for m := range pathItem {
			methodKeys = append(methodKeys, m)
		}
		sort.Strings(methodKeys)

		for _, m := range methodKeys {
			upperM := strings.ToUpper(m)
			switch upperM {
			case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD":
			default:
				continue
			}
			opMap, ok := pathItem[m].(map[string]any)
			if !ok {
				continue
			}
			tags := toStringSlice(opMap["tags"])
			if len(includeTags) > 0 && !matchesAnyTag(tags, includeTags) {
				continue
			}

			rawOpID := strings.TrimSpace(fmt.Sprint(opMap["operationId"]))
			if opMap["operationId"] == nil {
				rawOpID = ""
			}
			actName := sanitizeOpenAPIActionName(rawOpID)
			if rawOpID == "" {
				actName = sanitizeOpenAPIActionName(upperM + "_" + path)
			}
			baseActName := actName
			for suffix := 2; ; suffix++ {
				if _, dup := actions[actName]; !dup {
					break
				}
				// 预先为去重后缀留出空间，防止 >=64 字符的同前缀长路径陷入死循环
				suf := fmt.Sprintf("_%s_%d", strings.ToLower(upperM), suffix)
				prefixMax := max(64-len(suf), 1)
				prefix := baseActName
				if len(prefix) > prefixMax {
					prefix = strings.TrimRight(prefix[:prefixMax], "_")
				}
				actName = sanitizeOpenAPIActionName(prefix + suf)
			}

			desc := strings.TrimSpace(stringField(opMap, "summary"))
			if desc == "" {
				desc = strings.TrimSpace(stringField(opMap, "description"))
			}
			if desc == "" {
				desc = upperM + " " + path
			}

			params := make(map[string]ParamSpec)
			var queryParams []string
			allParams := append(append([]openAPIParam(nil), pathLevelParams...), parseOpenAPIParams(opMap["parameters"], doc.Components.Schemas)...)
			for _, p := range allParams {
				if p.Name == "" || (p.In != "path" && p.In != "query") {
					continue
				}
				typ := p.Type
				if typ == "" {
					typ = "string"
				}
				params[p.Name] = ParamSpec{
					Type:        typ,
					Required:    p.Required || p.In == "path",
					Default:     p.Default,
					Enum:        p.Enum,
					Description: p.Description,
				}
				if p.In == "query" {
					queryParams = append(queryParams, p.Name)
				}
			}

			// 解析 requestBody（支持本地 #/components/schemas/... $ref、冲突自动降级 json_body、以及顶层数组 body）
			bodyMode := "none"
			bodyParamName := ""
			if rb, ok := opMap["requestBody"].(map[string]any); ok {
				rbRequired, _ := rb["required"].(bool)
				if content, ok := rb["content"].(map[string]any); ok {
					if jc, ok := content["application/json"].(map[string]any); ok {
						if rawSchema, ok := jc["schema"].(map[string]any); ok {
							resolved := resolveLocalSchemaRef(rawSchema, doc.Components.Schemas, 0)
							props, hasProps := resolved["properties"].(map[string]any)
							hasCollision := false
							if hasProps && len(props) > 0 {
								for pk := range props {
									if _, exists := params[pk]; exists {
										hasCollision = true
										break
									}
								}
							}
							if hasProps && len(props) > 0 && !hasCollision {
								bodyMode = "json"
								reqSet := toLowerSet(toStringSlice(resolved["required"]))
								for pk, pv := range props {
									ps := ParamSpec{Required: reqSet[strings.ToLower(pk)]}
									if pm, ok := pv.(map[string]any); ok {
										pm = resolveLocalSchemaRef(pm, doc.Components.Schemas, 1)
										ps.Type = inferSchemaType(pm)
										ps.Description = stringField(pm, "description")
										if def, exists := pm["default"]; exists {
											ps.Default = def
										}
										if enumRaw, ok := pm["enum"].([]any); ok {
											ps.Enum = enumRaw
										}
									}
									params[pk] = ps
								}
							} else {
								// 无顶层 properties / 顶层数组 / 与 path/query 存在同名字段冲突 -> 使用不冲突的 json_body 直发
								bodyMode = "json_body"
								bodyParamName = "body"
								for _, cand := range []string{"body", "_body", "request_body"} {
									if _, exists := params[cand]; !exists {
										bodyParamName = cand
										break
									}
								}
								bodyType := inferSchemaType(resolved)
								params[bodyParamName] = ParamSpec{
									Type:        bodyType,
									Required:    rbRequired,
									Description: "JSON request body payload",
								}
							}
						}
					}
				}
			}

			risk := a.resolveMethodRisk(upperM)
			actSpec := ActionSpec{
				Name:        actName,
				Description: desc,
				Risk:        risk,
				Tags:        tags,
				Params:      params,
				Method:      upperM,
				Path:        path,
				QueryParams: queryParams,
				BodyMode:    bodyMode,
				BodyParam:   bodyParamName,
			}
			actions[actName] = actSpec
			defs = append(defs, ActionDef{
				Name:        actName,
				Description: fmt.Sprintf("[%s %s] %s", upperM, path, desc),
				Risk:        risk,
				Timeout:     a.spec.EffectiveTimeout(""),
				InputSchema: ai.SanitizeToolSchema(BuildActionSchema(params)),
				Tags:        tags,
			})
		}
	}

	a.baseURL = resolvedBase
	a.defs = defs
	a.actions = actions
	a.loaded = true
	return nil
}

type openAPIParam struct {
	Name        string
	In          string
	Required    bool
	Description string
	Type        string
	Default     any
	Enum        []any
}

func inferSchemaType(schema map[string]any) string {
	if schema == nil {
		return ""
	}
	if ts := stringField(schema, "type"); ts != "" {
		return ts
	}
	if _, ok := schema["properties"].(map[string]any); ok {
		return "object"
	}
	if _, ok := schema["items"]; ok {
		return "array"
	}
	return ""
}

func parseOpenAPIParams(raw any, components map[string]map[string]any) []openAPIParam {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]openAPIParam, 0, len(list))
	for _, it := range list {
		pm, ok := it.(map[string]any)
		if !ok {
			continue
		}
		p := openAPIParam{
			Name:        stringField(pm, "name"),
			In:          stringField(pm, "in"),
			Description: stringField(pm, "description"),
		}
		p.Required, _ = pm["required"].(bool)
		if sch, ok := pm["schema"].(map[string]any); ok {
			sch = resolveLocalSchemaRef(sch, components, 0)
			p.Type = inferSchemaType(sch)
			if def, exists := sch["default"]; exists {
				p.Default = def
			}
			if enumRaw, ok := sch["enum"].([]any); ok {
				p.Enum = enumRaw
			}
		}
		out = append(out, p)
	}
	return out
}

func resolveLocalSchemaRef(schema map[string]any, components map[string]map[string]any, depth int) map[string]any {
	if schema == nil || depth > 6 {
		return schema
	}
	ref := stringField(schema, "$ref")
	if strings.HasPrefix(ref, "#/components/schemas/") && len(components) > 0 {
		name := strings.TrimPrefix(ref, "#/components/schemas/")
		if target, ok := components[name]; ok {
			return resolveLocalSchemaRef(target, components, depth+1)
		}
	}
	return schema
}

func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func toStringSlice(raw any) []string {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (a *openapiAdapter) loadSpecBytes(ctx context.Context) ([]byte, error) {
	if sf := strings.TrimSpace(a.spec.Adapter.SpecFile); sf != "" {
		safePath, err := safeResolveSpecFile(a.spec.Dir, a.spec.WorkspaceCwd, sf)
		if err != nil {
			return nil, err
		}
		return os.ReadFile(safePath)
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, a.spec.Adapter.SpecURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch openapi spec HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
}

func (a *openapiAdapter) resolveMethodRisk(upperMethod string) RiskLevel {
	if r, ok := a.spec.Adapter.MethodRiskMap[upperMethod]; ok && r != "" {
		return r
	}
	switch upperMethod {
	case "GET", "HEAD":
		return RiskRead
	case "DELETE":
		return RiskDestructive
	default:
		return RiskWrite
	}
}

func (a *openapiAdapter) DiscoverActions(ctx context.Context) ([]ActionDef, error) {
	if err := a.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]ActionDef, len(a.defs))
	copy(out, a.defs)
	return out, nil
}

func (a *openapiAdapter) Invoke(ctx context.Context, req InvokeRequest) (*InvokeResult, error) {
	start := time.Now()
	if err := a.ensureLoaded(ctx); err != nil {
		return &InvokeResult{
			IsError:   true,
			ErrorCode: ErrAdapterOffline,
			Content:   formatCodedError(ErrAdapterOffline, fmt.Sprintf("load openapi spec failed: %v", err)),
			Duration:  time.Since(start),
		}, nil
	}
	a.mu.RLock()
	act, ok := a.actions[req.Action]
	baseURL := a.baseURL
	a.mu.RUnlock()
	if !ok {
		return &InvokeResult{
			IsError:   true,
			ErrorCode: ErrNotFound,
			Content:   formatCodedError(ErrNotFound, fmt.Sprintf("action %q not found in openapi extension %q", req.Action, a.spec.Name)),
		}, nil
	}
	return invokeHTTPAction(ctx, a.client, a.spec, baseURL, act, req, start)
}

func (a *openapiAdapter) HealthCheck(ctx context.Context) error {
	if err := a.ensureLoaded(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(a.spec.HealthCheck.HTTPGet) != "" {
		a.mu.RLock()
		baseURL := a.baseURL
		a.mu.RUnlock()
		return probeHTTPHealth(ctx, a.client, baseURL, a.spec.HealthCheck)
	}
	return nil
}

func (a *openapiAdapter) Close() error { return nil }

func isPathExcluded(path string, patterns []string) bool {
	for _, pat := range patterns {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if strings.HasSuffix(pat, "*") {
			if strings.HasPrefix(path, strings.TrimSuffix(pat, "*")) {
				return true
			}
		} else if path == pat {
			return true
		}
	}
	return false
}

func matchesAnyTag(tags []string, allowed map[string]bool) bool {
	for _, t := range tags {
		if allowed[strings.ToLower(strings.TrimSpace(t))] {
			return true
		}
	}
	return false
}

func sanitizeOpenAPIActionName(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	out := strings.Trim(sb.String(), "_")
	for strings.Contains(out, "__") {
		out = strings.ReplaceAll(out, "__", "_")
	}
	if len(out) > 64 {
		out = out[:64]
	}
	if out == "" {
		return "endpoint"
	}
	return out
}

var _ = json.Valid
