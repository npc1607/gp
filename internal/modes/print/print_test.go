package print

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gp/internal/ai"
)

type mockProvider struct {
	mu           sync.Mutex
	turnCount    int
	lastModel    string
	chunks       []ai.StreamChunk
	chatHandlers []func(req *ai.ChatRequest) []ai.StreamChunk
}

func (m *mockProvider) Name() string { return "mock" }

func (m *mockProvider) StreamChat(ctx context.Context, req *ai.ChatRequest) (<-chan ai.StreamChunk, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.lastModel = req.Model

	if len(m.chatHandlers) > 0 {
		idx := m.turnCount
		m.turnCount++
		if idx >= len(m.chatHandlers) {
			return nil, fmt.Errorf("no more mock responses for turn %d", idx)
		}
		chunks := m.chatHandlers[idx](req)
		ch := make(chan ai.StreamChunk, len(chunks))
		for _, c := range chunks {
			ch <- c
		}
		close(ch)
		return ch, nil
	}

	ch := make(chan ai.StreamChunk, len(m.chunks))
	for _, c := range m.chunks {
		ch <- c
	}
	close(ch)
	return ch, nil
}

func setupTestFile(t *testing.T, dir, filename, content string) string {
	t.Helper()
	filePath := filepath.Join(dir, filename)
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("setupTestFile(filename=%s): got error %v, want nil", filename, err)
	}
	t.Cleanup(func() {
		_ = os.Remove(filePath)
	})
	return filePath
}

func TestPrintRunner_Run(t *testing.T) {
	fn := "PrintRunner.Run"
	var buf bytes.Buffer
	mock := &mockProvider{
		chunks: []ai.StreamChunk{
			{DeltaText: "Hello "},
			{DeltaText: "world!"},
		},
	}

	opts := RunnerOptions{
		Writer:   &buf,
		Provider: mock,
	}
	runner := NewRunner(opts)

	prompt := "greet the world"
	err := runner.Run(context.Background(), prompt)
	if err != nil {
		t.Fatalf("%s(prompt=%q): got error %v, want nil", fn, prompt, err)
	}

	expected := "Hello world!\n"
	if got, want := buf.String(), expected; got != want {
		t.Fatalf("%s(prompt=%q): got output %q, want %q", fn, prompt, got, want)
	}
}

func TestPrintRunner_EmptyPrompt(t *testing.T) {
	fn := "PrintRunner.Run"
	runner := NewRunner(RunnerOptions{})
	prompt := "   "
	err := runner.Run(context.Background(), prompt)
	if err == nil {
		t.Fatalf("%s(prompt=%q): got nil error on empty prompt, want non-nil error", fn, prompt)
	}
}

// Table-driven tests for model resolution logic
func TestPrintRunner_ModelResolution(t *testing.T) {
	fn := "PrintRunner.resolveModelName"

	cases := []struct {
		name       string
		optModel   string
		gpModelEnv string
		piModelEnv string
		wantModel  string
	}{
		{
			name:       "explicit_options_model",
			optModel:   "custom/my-model",
			gpModelEnv: "env/gp-model",
			piModelEnv: "env/pi-model",
			wantModel:  "custom/my-model",
		},
		{
			name:       "fallback_to_gp_model_env",
			optModel:   "",
			gpModelEnv: "openai/gpt-4o",
			piModelEnv: "antigravity/gemini-fallback",
			wantModel:  "openai/gpt-4o",
		},
		{
			name:       "fallback_to_pi_model_env",
			optModel:   "",
			gpModelEnv: "",
			piModelEnv: "antigravity/gemini-3.8-flash",
			wantModel:  "antigravity/gemini-3.8-flash",
		},
		{
			name:       "default_fallback",
			optModel:   "",
			gpModelEnv: "",
			piModelEnv: "",
			wantModel:  defaultModel, // antigravity/gemini-4-argon
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GP_MODEL", tc.gpModelEnv)
			t.Setenv("PI_MODEL", tc.piModelEnv)

			mock := &mockProvider{
				chunks: []ai.StreamChunk{{DeltaText: "ok"}},
			}
			var buf bytes.Buffer
			runner := NewRunner(RunnerOptions{
				ModelName: tc.optModel,
				Writer:    &buf,
				Provider:  mock,
			})

			err := runner.Run(context.Background(), "test prompt")
			if err != nil {
				t.Fatalf("%s(case=%s): got unexpected run error %v, want nil", fn, tc.name, err)
			}

			if got, want := mock.lastModel, tc.wantModel; got != want {
				t.Fatalf("%s(case=%s): got model %q, want %q", fn, tc.name, got, want)
			}
		})
	}
}

// TC-PR-001: Print-CLI执行驱动本地工具与输出流
func TestPrintRunner_TC_PR_001_CLIExecutionLocalTools(t *testing.T) {
	fn := "PrintRunner.Run"
	tmpDir := t.TempDir()

	testContent := "package main\n\nfunc main() {\n\tprintln(\"gp works\")\n}\n"
	testFilePath := setupTestFile(t, tmpDir, "main.go", testContent)

	// Ensure env vars do not shadow default model fallback
	t.Setenv("GP_MODEL", "")
	t.Setenv("PI_MODEL", "")

	mock := &mockProvider{
		chatHandlers: []func(req *ai.ChatRequest) []ai.StreamChunk{
			// Turn 0: Model emits tool call requesting read on main.go
			func(req *ai.ChatRequest) []ai.StreamChunk {
				idx := 0
				return []ai.StreamChunk{
					{
						ToolCallDelta: &ai.ToolCall{
							Index: &idx,
							ID:    "call_read_main",
							Type:  "function",
							Function: ai.FunctionCall{
								Name:      "read",
								Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q}`, testFilePath)),
							},
						},
					},
				}
			},
			// Turn 1: Model receives tool output and streams final explanation
			func(req *ai.ChatRequest) []ai.StreamChunk {
				return []ai.StreamChunk{
					{DeltaText: "代码分析："},
					{DeltaText: "main.go 定义了程序入口并打印 gp works。"},
				}
			},
		},
	}

	var outBuf bytes.Buffer
	runner := NewRunner(RunnerOptions{
		Cwd:      tmpDir,
		Writer:   &outBuf,
		Provider: mock,
	})

	task := "请用 read 工具读取 main.go 并做简要说明"
	err := runner.Run(context.Background(), task)
	if err != nil {
		t.Fatalf("%s(task=%q): got error %v, want nil", fn, task, err)
	}

	// 1. Verify default model fallback to antigravity/gemini-4-argon
	wantModel := "antigravity/gemini-4-argon"
	if got, want := mock.lastModel, wantModel; got != want {
		t.Errorf("%s(task=%q): got model %q, want %q", fn, task, got, want)
	}

	// 2. Verify captured streamed text
	capturedText := outBuf.String()
	wantSubstr := "代码分析：main.go 定义了程序入口并打印 gp works。"
	if !strings.Contains(capturedText, wantSubstr) {
		t.Fatalf("%s(task=%q): got output %q, want containing %q", fn, task, capturedText, wantSubstr)
	}
	if !strings.HasSuffix(capturedText, "\n") {
		t.Errorf("%s(task=%q): got output without newline suffix: %q", fn, task, capturedText)
	}
}
