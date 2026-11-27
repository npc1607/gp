package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// scriptAdapter 实现极简 stdin/stdout JSON 协议，替代 80% 轻量自研 MCP Server
type scriptAdapter struct {
	spec    *ExtensionSpec
	actions map[string]ActionSpec
}

func newScriptAdapter(spec *ExtensionSpec) Adapter {
	am := make(map[string]ActionSpec, len(spec.Actions))
	for _, a := range spec.Actions {
		am[a.Name] = a
	}
	return &scriptAdapter{spec: spec, actions: am}
}

func (a *scriptAdapter) Type() string { return "script" }

func (a *scriptAdapter) DiscoverActions(_ context.Context) ([]ActionDef, error) {
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

func (a *scriptAdapter) resolveExecPath() (string, error) {
	if a.spec.Dir == "" {
		return "", fmt.Errorf("extension %q missing base directory", a.spec.Name)
	}
	return safeJoinUnderDir(a.spec.Dir, a.spec.Adapter.Exec)
}

func (a *scriptAdapter) Invoke(ctx context.Context, req InvokeRequest) (*InvokeResult, error) {
	start := time.Now()
	act, ok := a.actions[req.Action]
	if !ok {
		return &InvokeResult{
			IsError:   true,
			ErrorCode: ErrNotFound,
			Content:   formatCodedError(ErrNotFound, fmt.Sprintf("action %q not found in script extension %q", req.Action, a.spec.Name)),
		}, nil
	}

	execPath, err := a.resolveExecPath()
	if err != nil {
		return &InvokeResult{
			IsError:   true,
			ErrorCode: ErrDenied,
			Content:   formatCodedError(ErrDenied, fmt.Sprintf("invalid script path: %v", err)),
			Duration:  time.Since(start),
		}, nil
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = a.spec.EffectiveTimeout(act.Timeout)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	payload := map[string]any{
		"api_version": APIVersionV1,
		"action":      req.Action,
		"args":        req.Args,
		"context": map[string]any{
			"cwd":        req.Cwd,
			"agent_id":   req.AgentID,
			"timeout_ms": timeout.Milliseconds(),
		},
	}
	if req.Auth.Token != "" {
		payload["auth"] = map[string]any{
			"type":  req.Auth.Type,
			"token": req.Auth.Token,
		}
	}
	rawIn, err := json.Marshal(payload)
	if err != nil {
		return &InvokeResult{
			IsError:   true,
			ErrorCode: ErrInvalidArgs,
			Content:   formatCodedError(ErrInvalidArgs, fmt.Sprintf("marshal script input: %v", err)),
			Duration:  time.Since(start),
		}, nil
	}

	var cmd *exec.Cmd
	if interp := strings.TrimSpace(a.spec.Adapter.Interpreter); interp != "" {
		cmd = exec.CommandContext(cctx, interp, execPath)
	} else {
		cmd = exec.CommandContext(cctx, execPath)
	}
	if a.spec.Adapter.Cwd != "" {
		cmd.Dir = a.spec.Adapter.Cwd
	} else if req.Cwd != "" {
		cmd.Dir = req.Cwd
	} else {
		cmd.Dir = a.spec.Dir
	}
	cmd.Env = buildMergedEnv(a.spec.Adapter.Env, req.Auth)
	cmd.Stdin = bytes.NewReader(rawIn)

	stdout := &cappedBuffer{max: maxSubprocessOutputBytes}
	stderr := &cappedBuffer{max: maxSubprocessOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	runErr := cmd.Run()
	dur := time.Since(start)
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return &InvokeResult{
			IsError:   true,
			ErrorCode: ErrTimeout,
			Content:   formatCodedError(ErrTimeout, fmt.Sprintf("script timed out after %s", timeout)),
			Duration:  dur,
		}, nil
	}

	// 尝试解析标准 JSON 响应协议
	var resp struct {
		OK        *bool  `json:"ok"`
		Content   string `json:"content"`
		Data      any    `json:"data"`
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	rawOut := strings.TrimSpace(stdout.String())
	if rawOut != "" && json.Unmarshal([]byte(rawOut), &resp) == nil && (resp.OK != nil || resp.Content != "" || resp.Data != nil || resp.Error != "") {
		// 非零退出码时除非脚本显式声明 ok=true，否则不得静默吞掉 runErr
		isErr := (resp.OK != nil && !*resp.OK) || (resp.OK == nil && (resp.Error != "" || runErr != nil))
		if isErr {
			code := resp.ErrorCode
			if code == "" {
				code = ErrUpstream
			}
			msg := resp.Error
			if msg == "" {
				msg = strings.TrimSpace(stderr.String())
			}
			if msg == "" {
				msg = "script returned ok=false"
			}
			return &InvokeResult{
				IsError:   true,
				ErrorCode: code,
				Content:   formatCodedError(code, msg),
				Duration:  dur,
			}, nil
		}
		var outText string
		switch {
		case resp.Content != "" && resp.Data != nil:
			b, _ := json.MarshalIndent(resp.Data, "", "  ")
			outText = resp.Content + "\n" + string(b)
		case resp.Content != "":
			outText = resp.Content
		case resp.Data != nil:
			b, _ := json.MarshalIndent(resp.Data, "", "  ")
			outText = string(b)
		}
		if act.Extract != "" {
			outText = ExtractJSONPath(outText, act.Extract)
		}
		return &InvokeResult{
			Content:  outText,
			Duration: dur,
			RawBytes: len(outText),
		}, nil
	}

	if runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = rawOut
		}
		if msg == "" {
			msg = runErr.Error()
		}
		return &InvokeResult{
			IsError:   true,
			ErrorCode: ErrUpstream,
			Content:   formatCodedError(ErrUpstream, fmt.Sprintf("script failed (%v): %s", runErr, msg)),
			Duration:  dur,
		}, nil
	}

	if act.Extract != "" {
		rawOut = ExtractJSONPath(rawOut, act.Extract)
	}
	return &InvokeResult{
		Content:  rawOut,
		Duration: dur,
		RawBytes: len(rawOut),
	}, nil
}

func (a *scriptAdapter) HealthCheck(_ context.Context) error {
	p, err := a.resolveExecPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); err != nil {
		return fmt.Errorf("script %q not accessible: %w", p, err)
	}
	if interp := strings.TrimSpace(a.spec.Adapter.Interpreter); interp != "" {
		if _, err := exec.LookPath(interp); err != nil {
			return fmt.Errorf("interpreter %q not found in PATH", interp)
		}
	}
	return nil
}

func (a *scriptAdapter) Close() error { return nil }
