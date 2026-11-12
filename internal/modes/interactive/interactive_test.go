package interactive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"gp/internal/ai"
	"gp/internal/core/settings"
	"gp/internal/core/storage"
)

type mockInteractiveProvider struct {
	mu           sync.Mutex
	turnCount    int
	lastModel    string
	chunks       []ai.StreamChunk
	chatHandlers []func(req *ai.ChatRequest) []ai.StreamChunk
}

func (m *mockInteractiveProvider) Name() string { return "mock" }

func (m *mockInteractiveProvider) StreamChat(ctx context.Context, req *ai.ChatRequest) (<-chan ai.StreamChunk, error) {
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

// TC-TUI-013: Headless 模式运行流水线
func TestInteractiveRunner_TC_TUI_013_HeadlessPipeline(t *testing.T) {
	fn := "interactive.Runner.Run"

	tests := []struct {
		name          string
		initialPrompt string
		runPrompt     string
		modelName     string
		wantModel     string
		wantError     bool
	}{
		{
			name:          "headless_with_run_prompt",
			initialPrompt: "",
			runPrompt:     "headless test prompt",
			modelName:     "antigravity/gemini-4-argon",
			wantModel:     "antigravity/gemini-4-argon",
			wantError:     false,
		},
		{
			name:          "headless_with_initial_prompt_fallback",
			initialPrompt: "initial prompt fallback",
			runPrompt:     "",
			modelName:     "antigravity/gemini-4-argon",
			wantModel:     "antigravity/gemini-4-argon",
			wantError:     false,
		},
		{
			name:          "headless_empty_prompt_noop",
			initialPrompt: "",
			runPrompt:     "",
			modelName:     "antigravity/gemini-4-argon",
			wantModel:     "",
			wantError:     false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Helper()
			memStore := storage.NewMemoryStorage()
			t.Cleanup(func() {
				_ = memStore.Close()
			})

			mockProv := &mockInteractiveProvider{
				chunks: []ai.StreamChunk{
					{DeltaText: "Pipeline executing in headless mode"},
					{Usage: &ai.TokenUsage{InputTokens: 15, OutputTokens: 25, TotalTokens: 40}},
				},
			}

			opts := RunnerOptions{
				ModelName:     tc.modelName,
				Storage:       memStore,
				Provider:      mockProv,
				Headless:      true,
				InitialPrompt: tc.initialPrompt,
			}

			runner := NewRunner(opts)
			err := runner.Run(context.Background(), tc.runPrompt)

			if (err != nil) != tc.wantError {
				t.Fatalf("%s(inputs=%+v): got err %v, want error %v", fn, tc, err, tc.wantError)
			}

			if tc.wantModel != "" && mockProv.lastModel != tc.wantModel {
				t.Fatalf("%s(inputs=%+v): mock provider lastModel got %q, want %q", fn, tc, mockProv.lastModel, tc.wantModel)
			}
		})
	}
}

// TestInteractiveRunner_HeadlessRun 保持向后兼容的用例入口
func TestInteractiveRunner_HeadlessRun(t *testing.T) {
	TestInteractiveRunner_TC_TUI_013_HeadlessPipeline(t)
}

// TestInteractiveRunner_SettingsResolution 验证级联配置解析
func TestInteractiveRunner_SettingsResolution(t *testing.T) {
	fn := "interactive.Runner.SettingsResolution"
	t.Setenv("GP_MODEL", "")
	t.Setenv("PI_MODEL", "")
	t.Setenv("GP_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("GP_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	tmpDir := t.TempDir()
	gpDir := filepath.Join(tmpDir, ".gp")
	if err := os.MkdirAll(gpDir, 0755); err != nil {
		t.Fatalf("%s: mkdir %s failed: %v", fn, gpDir, err)
	}
	settingsContent := `{
		"model": "settings-test-model",
		"base_url": "https://api.test.example/v1",
		"api_key": "test-key-123",
		"default_tools": ["read", "bash"]
	}`
	if err := os.WriteFile(filepath.Join(gpDir, "settings.json"), []byte(settingsContent), 0644); err != nil {
		t.Fatalf("%s: write settings failed: %v", fn, err)
	}

	mgr := settings.NewManager(tmpDir)
	cfg, err := mgr.Load()
	if err != nil {
		t.Fatalf("%s: load settings failed: %v", fn, err)
	}

	opts := RunnerOptions{
		Cwd:         tmpDir,
		SettingsMgr: mgr,
		Settings:    cfg,
		Headless:    true,
	}

	runner := NewRunner(opts)

	model := runner.resolveModelName(cfg)
	if got, want := model, "settings-test-model"; got != want {
		t.Fatalf("%s: resolveModelName got %q, want %q", fn, got, want)
	}

	baseURL := runner.resolveBaseURL(cfg)
	if got, want := baseURL, "https://api.test.example/v1"; got != want {
		t.Fatalf("%s: resolveBaseURL got %q, want %q", fn, got, want)
	}

	apiKey := runner.resolveAPIKey(cfg)
	if got, want := apiKey, "test-key-123"; got != want {
		t.Fatalf("%s: resolveAPIKey got %q, want %q", fn, got, want)
	}

	reg, err := runner.resolveRegistry(tmpDir, cfg)
	if err != nil {
		t.Fatalf("%s: resolveRegistry failed: %v", fn, err)
	}
	if _, ok := reg.Get("read"); !ok {
		t.Fatalf("%s: registry missing expected 'read' tool, registered: %+v", fn, reg.List())
	}
	if _, ok := reg.Get("bash"); !ok {
		t.Fatalf("%s: registry missing expected 'bash' tool, registered: %+v", fn, reg.List())
	}
}
