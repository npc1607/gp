package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strings"
	"text/template"
	"time"
)

const maxSubprocessOutputBytes = 4 * 1024 * 1024

// cappedBuffer 限制子进程 stdout/stderr 内存缓冲上限，避免失控输出引发 OOM
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	origLen := len(p)
	if c.max <= 0 {
		c.max = maxSubprocessOutputBytes
	}
	remain := c.max - c.buf.Len()
	if remain > 0 {
		if len(p) > remain {
			p = p[:remain]
		}
		_, _ = c.buf.Write(p)
	}
	return origLen, nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }

// cliAdapter 将成熟命令行工具以安全参数数组方式封装为结构化动作（严禁 sh -c，杜绝命令注入）
type cliAdapter struct {
	spec    *ExtensionSpec
	actions map[string]ActionSpec
}

func newCLIAdapter(spec *ExtensionSpec) Adapter {
	am := make(map[string]ActionSpec, len(spec.Actions))
	for _, a := range spec.Actions {
		am[a.Name] = a
	}
	return &cliAdapter{spec: spec, actions: am}
}

func (a *cliAdapter) Type() string { return "cli" }

func (a *cliAdapter) DiscoverActions(_ context.Context) ([]ActionDef, error) {
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

func (a *cliAdapter) Invoke(ctx context.Context, req InvokeRequest) (*InvokeResult, error) {
	start := time.Now()
	act, ok := a.actions[req.Action]
	if !ok {
		return &InvokeResult{
			IsError:   true,
			ErrorCode: ErrNotFound,
			Content:   formatCodedError(ErrNotFound, fmt.Sprintf("action %q not found in cli extension %q", req.Action, a.spec.Name)),
		}, nil
	}

	argv, err := renderArgvTemplates(act.Args, req.Args)
	if err != nil {
		return &InvokeResult{
			IsError:   true,
			ErrorCode: ErrInvalidArgs,
			Content:   formatCodedError(ErrInvalidArgs, fmt.Sprintf("render cli args failed: %v", err)),
			Duration:  time.Since(start),
		}, nil
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = a.spec.EffectiveTimeout(act.Timeout)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, a.spec.Adapter.Command, argv...)
	cwd := a.spec.Adapter.Cwd
	if cwd == "" {
		cwd = req.Cwd
	}
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = buildMergedEnv(a.spec.Adapter.Env, req.Auth)

	stdout := &cappedBuffer{max: maxSubprocessOutputBytes}
	stderr := &cappedBuffer{max: maxSubprocessOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	runErr := cmd.Run()
	dur := time.Since(start)
	if runErr != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return &InvokeResult{
				IsError:   true,
				ErrorCode: ErrTimeout,
				Content:   formatCodedError(ErrTimeout, fmt.Sprintf("cli command timed out after %s", timeout)),
				Duration:  dur,
			}, nil
		}
		var execErr *exec.Error
		if errors.As(runErr, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound) {
			return &InvokeResult{
				IsError:   true,
				ErrorCode: ErrAdapterOffline,
				Content:   formatCodedError(ErrAdapterOffline, fmt.Sprintf("command %q not found in PATH", a.spec.Adapter.Command)),
				Duration:  dur,
			}, nil
		}
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = strings.TrimSpace(stdout.String())
		}
		if errMsg == "" {
			errMsg = runErr.Error()
		}
		return &InvokeResult{
			IsError:   true,
			ErrorCode: ErrUpstream,
			Content:   formatCodedError(ErrUpstream, fmt.Sprintf("cli exit error (%v): %s", runErr, errMsg)),
			Duration:  dur,
		}, nil
	}

	outStr := strings.TrimRight(stdout.String(), "\r\n")
	if strings.EqualFold(act.Output, "json") && outStr != "" {
		var pretty bytes.Buffer
		if json.Indent(&pretty, []byte(outStr), "", "  ") == nil {
			outStr = pretty.String()
		}
	}
	if act.Extract != "" {
		outStr = ExtractJSONPath(outStr, act.Extract)
	}
	return &InvokeResult{
		Content:  outStr,
		Duration: dur,
		RawBytes: len(outStr),
	}, nil
}

func (a *cliAdapter) HealthCheck(ctx context.Context) error {
	path, err := exec.LookPath(a.spec.Adapter.Command)
	if err != nil {
		return fmt.Errorf("command %q not found in PATH", a.spec.Adapter.Command)
	}
	if len(a.spec.HealthCheck.CLIArgs) == 0 {
		return nil
	}
	timeout := 3 * time.Second
	if d, err := time.ParseDuration(a.spec.HealthCheck.Timeout); err == nil && d > 0 {
		timeout = d
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, path, a.spec.HealthCheck.CLIArgs...)
	if a.spec.Adapter.Cwd != "" {
		cmd.Dir = a.spec.Adapter.Cwd
	}
	cmd.Env = buildMergedEnv(a.spec.Adapter.Env, ResolvedAuth{})
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("healthcheck failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (a *cliAdapter) Close() error { return nil }

func normalizeTemplateArgs(args map[string]any) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		switch n := v.(type) {
		case json.Number:
			if i, err := n.Int64(); err == nil {
				out[k] = i
			} else if f, err := n.Float64(); err == nil {
				out[k] = f
			} else {
				out[k] = n.String()
			}
		case float64:
			if !math.IsNaN(n) && !math.IsInf(n, 0) && math.Trunc(n) == n && math.Abs(n) <= 9e15 {
				out[k] = int64(n)
			} else {
				out[k] = n
			}
		default:
			out[k] = v
		}
	}
	return out
}

func renderArgvTemplates(templates []string, args map[string]any) ([]string, error) {
	if len(templates) == 0 {
		return nil, nil
	}
	normArgs := normalizeTemplateArgs(args)
	out := make([]string, 0, len(templates))
	for _, raw := range templates {
		if !strings.Contains(raw, "{{") {
			out = append(out, raw)
			continue
		}
		tmpl, err := template.New("arg").Option("missingkey=zero").Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("parse arg template %q: %w", raw, err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, normArgs); err != nil {
			return nil, fmt.Errorf("execute arg template %q: %w", raw, err)
		}
		// Go text/template 对 map[string]any 缺失键会渲染为 "<no value>"，统一清洗为空字符串
		rendered := strings.ReplaceAll(buf.String(), "<no value>", "")
		if rendered == "" && (strings.Contains(raw, "if ") || strings.Contains(raw, "with ")) && strings.Contains(raw, "{{") {
			continue
		}
		out = append(out, rendered)
	}
	return out, nil
}

func buildMergedEnv(extra map[string]string, resolvedAuth ResolvedAuth) []string {
	env := append([]string(nil), os.Environ()...)
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	if resolvedAuth.Token != "" {
		env = append(env, "GP_EXT_AUTH_TOKEN="+resolvedAuth.Token)
	}
	return env
}
