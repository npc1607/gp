package extensions

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var pathParamRe = regexp.MustCompile(`\{([A-Za-z0-9_.-]+)\}`)

// formatArgScalar 安全格式化标量参数，避免大整数 float64 被格式化为科学计数法（如 1.23e+09）
func formatArgScalar(v any) string {
	switch n := v.(type) {
	case json.Number:
		return n.String()
	case float64:
		if !math.IsNaN(n) && !math.IsInf(n, 0) && math.Trunc(n) == n && math.Abs(n) <= 9e15 {
			return strconv.FormatInt(int64(n), 10)
		}
		return strconv.FormatFloat(n, 'f', -1, 64)
	case string:
		return n
	default:
		return fmt.Sprint(v)
	}
}

// httpAdapter 将 REST / 内部 HTTP 服务零代码映射为结构化动作（复用连接池）
type httpAdapter struct {
	spec    *ExtensionSpec
	actions map[string]ActionSpec
	client  *http.Client
}

func newHTTPAdapter(spec *ExtensionSpec) Adapter {
	am := make(map[string]ActionSpec, len(spec.Actions))
	for _, a := range spec.Actions {
		am[a.Name] = a
	}
	return &httpAdapter{
		spec:    spec,
		actions: am,
		client:  &http.Client{},
	}
}

func (a *httpAdapter) Type() string { return "http" }

func (a *httpAdapter) DiscoverActions(_ context.Context) ([]ActionDef, error) {
	out := make([]ActionDef, 0, len(a.spec.Actions))
	for _, act := range a.spec.Actions {
		risk := act.Risk
		if risk == "" {
			risk = a.spec.EffectiveDefaultRisk()
		}
		out = append(out, ActionDef{
			Name:        act.Name,
			Description: act.Description,
			Risk:        risk,
			Timeout:     a.spec.EffectiveTimeout(act.Timeout),
			InputSchema: BuildActionSchema(act.Params),
			Tags:        act.Tags,
		})
	}
	return out, nil
}

func (a *httpAdapter) Invoke(ctx context.Context, req InvokeRequest) (*InvokeResult, error) {
	start := time.Now()
	act, ok := a.actions[req.Action]
	if !ok {
		return &InvokeResult{
			IsError:   true,
			ErrorCode: ErrNotFound,
			Content:   formatCodedError(ErrNotFound, fmt.Sprintf("action %q not found in http extension %q", req.Action, a.spec.Name)),
		}, nil
	}
	return invokeHTTPAction(ctx, a.client, a.spec, a.spec.Adapter.BaseURL, act, req, start)
}

func invokeHTTPAction(
	ctx context.Context,
	client *http.Client,
	spec *ExtensionSpec,
	baseURL string,
	act ActionSpec,
	req InvokeRequest,
	start time.Time,
) (*InvokeResult, error) {
	method := strings.ToUpper(strings.TrimSpace(act.Method))
	if method == "" {
		method = http.MethodGet
	}

	usedPathKeys := make(map[string]bool)
	var pathErr error
	resolvedPath := pathParamRe.ReplaceAllStringFunc(act.Path, func(m string) string {
		if pathErr != nil {
			return ""
		}
		sub := pathParamRe.FindStringSubmatch(m)
		if len(sub) < 2 {
			return m
		}
		key := sub[1]
		usedPathKeys[key] = true
		v, exists := req.Args[key]
		if !exists || v == nil {
			pathErr = fmt.Errorf("missing required path parameter %q", key)
			return ""
		}
		valStr := strings.TrimSpace(formatArgScalar(v))
		if valStr == "" || valStr == "." || valStr == ".." || strings.ContainsAny(valStr, "/\\") {
			pathErr = fmt.Errorf("invalid path parameter %q value %q", key, valStr)
			return ""
		}
		return url.PathEscape(valStr)
	})
	if pathErr != nil {
		return &InvokeResult{
			IsError:   true,
			ErrorCode: ErrInvalidArgs,
			Content:   formatCodedError(ErrInvalidArgs, pathErr.Error()),
			Duration:  time.Since(start),
		}, nil
	}

	fullURL := strings.TrimRight(baseURL, "/")
	if resolvedPath != "" {
		if !strings.HasPrefix(resolvedPath, "/") {
			resolvedPath = "/" + resolvedPath
		}
		fullURL += resolvedPath
	}

	parsedURL, err := url.Parse(fullURL)
	if err != nil {
		return &InvokeResult{
			IsError:   true,
			ErrorCode: ErrInvalidArgs,
			Content:   formatCodedError(ErrInvalidArgs, fmt.Sprintf("invalid url %q: %v", fullURL, err)),
			Duration:  time.Since(start),
		}, nil
	}

	isReadMethod := method == http.MethodGet || method == http.MethodHead || method == http.MethodDelete
	bodyMode := strings.ToLower(strings.TrimSpace(act.BodyMode))
	if bodyMode == "" {
		if !isReadMethod {
			bodyMode = "json"
		} else {
			bodyMode = "none"
		}
	}

	queryKeySet := make(map[string]bool, len(act.QueryParams))
	for _, qk := range act.QueryParams {
		queryKeySet[qk] = true
	}

	bodyKey := strings.TrimSpace(act.BodyParam)
	if bodyKey == "" {
		bodyKey = "body"
	}

	q := parsedURL.Query()
	bodyMap := make(map[string]any)
	for k, v := range req.Args {
		if usedPathKeys[k] || v == nil {
			continue
		}
		if (bodyMode == "json_body" || bodyMode == "raw") && k == bodyKey {
			bodyMap[k] = v
			continue
		}
		if queryKeySet[k] {
			q.Set(k, formatArgScalar(v))
			continue
		}
		switch bodyMode {
		case "none", "raw", "json_body":
			q.Set(k, formatArgScalar(v))
		default: // "json"
			bodyMap[k] = v
		}
	}

	if req.Auth.Type == "query" && req.Auth.QueryParam != "" && req.Auth.Token != "" {
		q.Set(req.Auth.QueryParam, req.Auth.Token)
	}
	parsedURL.RawQuery = q.Encode()

	var bodyReader io.Reader
	contentType := ""
	switch bodyMode {
	case "json_body":
		// OpenAPI $ref / 无顶层属性对象 / 数组 body / 字段名冲突回退模式：直接序列化 args[bodyKey]，避免双重包裹
		if rawBody, ok := bodyMap[bodyKey]; ok && rawBody != nil {
			raw, mErr := json.Marshal(rawBody)
			if mErr != nil {
				return &InvokeResult{
					IsError:   true,
					ErrorCode: ErrInvalidArgs,
					Content:   formatCodedError(ErrInvalidArgs, fmt.Sprintf("marshal json body: %v", mErr)),
					Duration:  time.Since(start),
				}, nil
			}
			bodyReader = bytes.NewReader(raw)
			contentType = "application/json"
		}
	case "json":
		if len(bodyMap) > 0 {
			raw, mErr := json.Marshal(bodyMap)
			if mErr != nil {
				return &InvokeResult{
					IsError:   true,
					ErrorCode: ErrInvalidArgs,
					Content:   formatCodedError(ErrInvalidArgs, fmt.Sprintf("marshal json body: %v", mErr)),
					Duration:  time.Since(start),
				}, nil
			}
			bodyReader = bytes.NewReader(raw)
			contentType = "application/json"
		}
	case "raw":
		if rawVal, ok := bodyMap[bodyKey]; ok && rawVal != nil {
			bodyReader = strings.NewReader(formatArgScalar(rawVal))
		}
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = spec.EffectiveTimeout(act.Timeout)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(cctx, method, parsedURL.String(), bodyReader)
	if err != nil {
		return &InvokeResult{
			IsError:   true,
			ErrorCode: ErrInvalidArgs,
			Content:   formatCodedError(ErrInvalidArgs, fmt.Sprintf("create http request: %v", err)),
			Duration:  time.Since(start),
		}, nil
	}
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	for k, v := range spec.Adapter.DefaultHeaders {
		httpReq.Header.Set(k, v)
	}
	for k, v := range act.Headers {
		httpReq.Header.Set(k, v)
	}
	applyHTTPAuth(httpReq, req.Auth)

	resp, err := client.Do(httpReq)
	dur := time.Since(start)
	if err != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return &InvokeResult{
				IsError:   true,
				ErrorCode: ErrTimeout,
				Content:   formatCodedError(ErrTimeout, fmt.Sprintf("http %s %s timed out after %s", method, parsedURL.Path, timeout)),
				Duration:  dur,
			}, nil
		}
		// 剥离 *url.Error 中可能携带 query token 的完整 URL，只暴露脱敏后的 host/path 与底层网络错误
		innerErr := err
		var urlErr *url.Error
		if errors.As(err, &urlErr) && urlErr.Err != nil {
			innerErr = urlErr.Err
		}
		return &InvokeResult{
			IsError:   true,
			ErrorCode: ErrAdapterOffline,
			Content:   formatCodedError(ErrAdapterOffline, fmt.Sprintf("http %s %s%s failed: %v", method, parsedURL.Host, parsedURL.Path, innerErr)),
			Duration:  dur,
		}, nil
	}
	defer resp.Body.Close()

	// 读取上限 4MB，防止恶意或巨型响应打爆内存，并显式校验读取错误
	rawResp, readErr := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if readErr != nil {
		return &InvokeResult{
			IsError:   true,
			ErrorCode: ErrUpstream,
			Content:   formatCodedError(ErrUpstream, fmt.Sprintf("read http response body failed: %v", readErr)),
			Duration:  dur,
		}, nil
	}
	respText := strings.TrimSpace(string(rawResp))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &InvokeResult{
			IsError:   true,
			ErrorCode: ErrUpstream,
			Content:   formatCodedError(ErrUpstream, fmt.Sprintf("HTTP %d %s: %s", resp.StatusCode, http.StatusText(resp.StatusCode), respText)),
			Duration:  dur,
			RawBytes:  len(respText),
		}, nil
	}

	if act.Extract != "" {
		respText = ExtractJSONPath(respText, act.Extract)
	} else if respText != "" && (strings.HasPrefix(respText, "{") || strings.HasPrefix(respText, "[")) {
		var pretty bytes.Buffer
		if json.Indent(&pretty, []byte(respText), "", "  ") == nil {
			respText = pretty.String()
		}
	}

	return &InvokeResult{
		Content:  respText,
		Duration: dur,
		RawBytes: len(respText),
	}, nil
}

func applyHTTPAuth(req *http.Request, auth ResolvedAuth) {
	if auth.Token == "" {
		return
	}
	switch auth.Type {
	case "bearer":
		h := auth.Header
		if h == "" {
			h = "Authorization"
		}
		prefix := auth.Prefix
		if prefix == "" {
			prefix = "Bearer "
		}
		req.Header.Set(h, prefix+auth.Token)
	case "header":
		if auth.Header != "" {
			req.Header.Set(auth.Header, auth.Prefix+auth.Token)
		}
	case "basic":
		enc := base64.StdEncoding.EncodeToString([]byte(auth.Token))
		req.Header.Set("Authorization", "Basic "+enc)
	}
}

func probeHTTPHealth(ctx context.Context, client *http.Client, baseURL string, hc HealthCheckSpec) error {
	target := strings.TrimRight(baseURL, "/")
	if p := strings.TrimSpace(hc.HTTPGet); p != "" {
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		target += p
	}
	if target == "" {
		return nil
	}
	timeout := 3 * time.Second
	if d, err := time.ParseDuration(hc.Timeout); err == nil && d > 0 {
		timeout = d
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) && urlErr.Err != nil {
			return fmt.Errorf("unreachable %s: %w", target, urlErr.Err)
		}
		return fmt.Errorf("unreachable %s: %w", target, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("unhealthy HTTP %d from %s", resp.StatusCode, target)
	}
	return nil
}

func (a *httpAdapter) HealthCheck(ctx context.Context) error {
	return probeHTTPHealth(ctx, a.client, a.spec.Adapter.BaseURL, a.spec.HealthCheck)
}

func (a *httpAdapter) Close() error { return nil }
