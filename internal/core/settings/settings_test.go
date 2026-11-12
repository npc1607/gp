package settings

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func cleanEnv(t *testing.T) {
	t.Helper()
	vars := []string{
		"GP_MODEL", "PI_MODEL",
		"GP_BASE_URL", "OPENAI_BASE_URL",
		"GP_API_KEY", "OPENAI_API_KEY",
		"GP_THINKING_LEVEL",
		"GP_MAX_CONCURRENCY",
		"GP_TOKENS_PER_MIN",
		"GP_TOOLS",
	}
	saved := make(map[string]string, len(vars))
	for _, v := range vars {
		saved[v] = os.Getenv(v)
		t.Setenv(v, "")
	}
	t.Cleanup(func() {
		for k, v := range saved {
			if v != "" {
				_ = os.Setenv(k, v)
			} else {
				_ = os.Unsetenv(k)
			}
		}
	})
}

// TC-SET-001: 默认配置初始化
func TestSettings_TC_SET_001_DefaultSettings(t *testing.T) {
	fn := "DefaultSettings"
	s := DefaultSettings()

	if got, want := s.Model, DefaultModel; got != want {
		t.Fatalf("%s(): Model got %q, want %q", fn, got, want)
	}
	if got, want := s.BaseURL, DefaultBaseURL; got != want {
		t.Fatalf("%s(): BaseURL got %q, want %q", fn, got, want)
	}
	if got, want := s.APIKey, ""; got != want {
		t.Fatalf("%s(): APIKey got %q, want %q", fn, got, want)
	}
	if got, want := s.ThinkingLevel, DefaultThinkingLevel; got != want {
		t.Fatalf("%s(): ThinkingLevel got %q, want %q", fn, got, want)
	}
	if got, want := s.MaxConcurrency, DefaultMaxConcurrency; got != want {
		t.Fatalf("%s(): MaxConcurrency got %d, want %d", fn, got, want)
	}
	if got, want := s.TokensPerMin, DefaultTokensPerMin; got != want {
		t.Fatalf("%s(): TokensPerMin got %d, want %d", fn, got, want)
	}

	wantTools := []string{"read", "write", "edit", "bash"}
	if got := s.DefaultTools; !reflect.DeepEqual(got, wantTools) {
		t.Fatalf("%s(): DefaultTools got %v, want %v", fn, got, wantTools)
	}
}

func TestDefaultSettings(t *testing.T) {
	TestSettings_TC_SET_001_DefaultSettings(t)
}

// TC-SET-002: 深拷贝隔离性
func TestSettings_TC_SET_002_CloneIsolation(t *testing.T) {
	fn := "Settings.Clone"
	orig := DefaultSettings()
	cloned := orig.Clone()

	if !reflect.DeepEqual(orig.DefaultTools, cloned.DefaultTools) {
		t.Fatalf("%s(): cloned tools mismatch: got %v, want %v", fn, cloned.DefaultTools, orig.DefaultTools)
	}

	cloned.DefaultTools[0] = "custom"
	cloned.DefaultTools = append(cloned.DefaultTools, "new_tool")

	if got, want := orig.DefaultTools[0], "read"; got != want {
		t.Fatalf("%s(): mutating cloned slice affected original[0]: got %q, want %q", fn, got, want)
	}
	if got, want := len(orig.DefaultTools), 4; got != want {
		t.Fatalf("%s(): mutating cloned slice affected original len: got %d, want %d", fn, got, want)
	}
}

func TestSettings_Clone(t *testing.T) {
	TestSettings_TC_SET_002_CloneIsolation(t)
}

// TC-SET-003: 全局与项目配置层叠合并
func TestSettings_TC_SET_003_CascadingMerge(t *testing.T) {
	fn := "Manager.Load"
	cleanEnv(t)
	tmpHome := t.TempDir()
	tmpCwd := t.TempDir()

	mgr := NewManagerWithPaths(tmpCwd, tmpHome)

	// 1. 无配置文件时返回默认值
	initial, err := mgr.Load()
	if err != nil {
		t.Fatalf("%s(initial): got error %v, want nil", fn, err)
	}
	if got, want := initial.Model, DefaultModel; got != want {
		t.Fatalf("%s(initial): Model got %q, want %q", fn, got, want)
	}

	// 2. 写入全局配置文件
	globalJSON := `{
		"model": "global-model-name",
		"max_concurrency": 25,
		"thinking_level": "medium"
	}`
	globalPath := mgr.GlobalPath()
	if err := os.MkdirAll(filepath.Dir(globalPath), 0755); err != nil {
		t.Fatalf("%s(mkdir global): got error %v", fn, err)
	}
	if err := os.WriteFile(globalPath, []byte(globalJSON), 0644); err != nil {
		t.Fatalf("%s(write global): got error %v", fn, err)
	}

	afterGlobal, err := mgr.Load()
	if err != nil {
		t.Fatalf("%s(after global): got error %v, want nil", fn, err)
	}
	if got, want := afterGlobal.Model, "global-model-name"; got != want {
		t.Fatalf("%s(after global): Model got %q, want %q", fn, got, want)
	}
	if got, want := afterGlobal.MaxConcurrency, 25; got != want {
		t.Fatalf("%s(after global): MaxConcurrency got %d, want %d", fn, got, want)
	}
	if got, want := afterGlobal.ThinkingLevel, "medium"; got != want {
		t.Fatalf("%s(after global): ThinkingLevel got %q, want %q", fn, got, want)
	}
	if got, want := afterGlobal.TokensPerMin, DefaultTokensPerMin; got != want {
		t.Fatalf("%s(after global): TokensPerMin got %d, want %d", fn, got, want)
	}

	// 3. 写入项目配置文件：覆盖 Model，但保留全局 MaxConcurrency 与 ThinkingLevel
	projectJSON := `{
		"model": "project-model-override",
		"tokens_per_min": 50000,
		"default_tools": ["read", "write"]
	}`
	projectPath := mgr.ProjectPath()
	if err := os.MkdirAll(filepath.Dir(projectPath), 0755); err != nil {
		t.Fatalf("%s(mkdir project): got error %v", fn, err)
	}
	if err := os.WriteFile(projectPath, []byte(projectJSON), 0644); err != nil {
		t.Fatalf("%s(write project): got error %v", fn, err)
	}

	afterProject, err := mgr.Load()
	if err != nil {
		t.Fatalf("%s(after project): got error %v, want nil", fn, err)
	}
	if got, want := afterProject.Model, "project-model-override"; got != want {
		t.Fatalf("%s(after project): Model got %q, want %q", fn, got, want)
	}
	if got, want := afterProject.MaxConcurrency, 25; got != want {
		t.Fatalf("%s(after project): MaxConcurrency got %d, want %d", fn, got, want)
	}
	if got, want := afterProject.ThinkingLevel, "medium"; got != want {
		t.Fatalf("%s(after project): ThinkingLevel got %q, want %q", fn, got, want)
	}
	if got, want := afterProject.TokensPerMin, 50000; got != want {
		t.Fatalf("%s(after project): TokensPerMin got %d, want %d", fn, got, want)
	}
	wantProjectTools := []string{"read", "write"}
	if got := afterProject.DefaultTools; !reflect.DeepEqual(got, wantProjectTools) {
		t.Fatalf("%s(after project): DefaultTools got %v, want %v", fn, got, wantProjectTools)
	}
}

func TestCascadingMerge(t *testing.T) {
	TestSettings_TC_SET_003_CascadingMerge(t)
}

// TC-SET-004: 环境变量覆盖文件配置
func TestSettings_TC_SET_004_EnvVarOverrides(t *testing.T) {
	fn := "Manager.Load"
	cleanEnv(t)
	tmpHome := t.TempDir()
	tmpCwd := t.TempDir()
	mgr := NewManagerWithPaths(tmpCwd, tmpHome)

	// 写入底层文件配置
	globalJSON := `{"model": "file-model", "api_key": "file-key", "base_url": "https://file.url"}`
	if err := os.MkdirAll(filepath.Dir(mgr.GlobalPath()), 0755); err != nil {
		t.Fatalf("%s(mkdir global): got error %v", fn, err)
	}
	if err := os.WriteFile(mgr.GlobalPath(), []byte(globalJSON), 0644); err != nil {
		t.Fatalf("%s(write global): got error %v", fn, err)
	}

	// 注入环境变量覆盖
	t.Setenv("GP_MODEL", "env-m")
	t.Setenv("GP_BASE_URL", "https://env.endpoint/v1")
	t.Setenv("GP_API_KEY", "sk-env-override")

	inputs := map[string]string{
		"GP_MODEL":    "env-m",
		"GP_BASE_URL": "https://env.endpoint/v1",
		"GP_API_KEY":  "sk-env-override",
	}

	s, err := mgr.Load()
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got error %v, want nil", fn, inputs, err)
	}
	if got, want := s.Model, "env-m"; got != want {
		t.Fatalf("%s(inputs=%+v): Model got %q, want %q", fn, inputs, got, want)
	}
	if got, want := s.BaseURL, "https://env.endpoint/v1"; got != want {
		t.Fatalf("%s(inputs=%+v): BaseURL got %q, want %q", fn, inputs, got, want)
	}
	if got, want := s.APIKey, "sk-env-override"; got != want {
		t.Fatalf("%s(inputs=%+v): APIKey got %q, want %q", fn, inputs, got, want)
	}
}

func TestEnvVarOverrides(t *testing.T) {
	TestSettings_TC_SET_004_EnvVarOverrides(t)
}

// TC-SET-005: 配置文件不存在容错
func TestSettings_TC_SET_005_MissingFileFaultTolerance(t *testing.T) {
	fn := "Manager.Load"
	cleanEnv(t)
	tmpHome := t.TempDir()
	tmpCwd := t.TempDir()
	mgr := NewManagerWithPaths(tmpCwd, tmpHome)

	s, err := mgr.Load()
	if err != nil {
		t.Fatalf("%s(missing files): got error %v, want nil", fn, err)
	}
	if s == nil {
		t.Fatalf("%s(missing files): got nil Settings, want non-nil", fn)
	}
	if got, want := s.Model, DefaultModel; got != want {
		t.Fatalf("%s(missing files): Model got %q, want %q", fn, got, want)
	}
}

// TC-SET-006: 畸形 JSON 配置文件解析
func TestSettings_TC_SET_006_MalformedJSONFaultTolerance(t *testing.T) {
	fn := "Manager.Load"
	cleanEnv(t)
	tmpHome := t.TempDir()
	tmpCwd := t.TempDir()
	mgr := NewManagerWithPaths(tmpCwd, tmpHome)

	malformedPath := mgr.ProjectPath()
	if err := os.MkdirAll(filepath.Dir(malformedPath), 0755); err != nil {
		t.Fatalf("%s(mkdir): got error %v", fn, err)
	}
	if err := os.WriteFile(malformedPath, []byte("{ malformed json ..."), 0644); err != nil {
		t.Fatalf("%s(write malformed): got error %v", fn, err)
	}

	_, err := mgr.Load()
	if err == nil {
		t.Fatalf("%s(malformed JSON): got err nil, want non-nil syntax error", fn)
	}
	if !strings.Contains(err.Error(), "project settings") {
		t.Fatalf("%s(malformed JSON): got error %q, want containing project settings path indicator", fn, err.Error())
	}
}

// TC-SET-007: 配置持久化与多级目录创建
func TestSettings_TC_SET_007_PersistenceAndDirCreation(t *testing.T) {
	fn := "Manager.SaveToFile"
	cleanEnv(t)
	tmpDir := t.TempDir()
	mgr := NewManager(tmpDir)

	toSave := Settings{
		Model:          "custom-saved-model",
		BaseURL:        "https://custom.endpoint/v1",
		APIKey:         "secret-key-123",
		ThinkingLevel:  "low",
		DefaultTools:   []string{"edit", "bash"},
		MaxConcurrency: 15,
		TokensPerMin:   120000,
	}

	deepPath := filepath.Join(tmpDir, "level1", "level2", "deep_settings.json")
	inputs := map[string]interface{}{
		"path":     deepPath,
		"settings": toSave,
	}

	if err := mgr.SaveToFile(deepPath, toSave); err != nil {
		t.Fatalf("%s(inputs=%+v): got error %v, want nil", fn, inputs, err)
	}

	if _, err := os.Stat(deepPath); err != nil {
		t.Fatalf("%s(inputs=%+v): file %s does not exist: %v", fn, inputs, deepPath, err)
	}

	// 验证回读
	readBackMgr := NewManagerWithPaths(filepath.Dir(deepPath), t.TempDir())
	loaded := DefaultSettings()
	if err := readBackMgr.loadFromFile(deepPath, &loaded); err != nil {
		t.Fatalf("loadFromFile(path=%s): got error %v, want nil", deepPath, err)
	}

	if got, want := loaded.Model, toSave.Model; got != want {
		t.Fatalf("%s: Model got %q, want %q", fn, got, want)
	}
	if got, want := loaded.BaseURL, toSave.BaseURL; got != want {
		t.Fatalf("%s: BaseURL got %q, want %q", fn, got, want)
	}
	if got, want := loaded.APIKey, toSave.APIKey; got != want {
		t.Fatalf("%s: APIKey got %q, want %q", fn, got, want)
	}
	if got, want := loaded.ThinkingLevel, toSave.ThinkingLevel; got != want {
		t.Fatalf("%s: ThinkingLevel got %q, want %q", fn, got, want)
	}
	if got, want := loaded.MaxConcurrency, toSave.MaxConcurrency; got != want {
		t.Fatalf("%s: MaxConcurrency got %d, want %d", fn, got, want)
	}
	if got, want := loaded.TokensPerMin, toSave.TokensPerMin; got != want {
		t.Fatalf("%s: TokensPerMin got %d, want %d", fn, got, want)
	}
	if got, want := loaded.DefaultTools, toSave.DefaultTools; !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: DefaultTools got %v, want %v", fn, got, want)
	}
}

func TestPersistence(t *testing.T) {
	TestSettings_TC_SET_007_PersistenceAndDirCreation(t)
}

func TestFaultTolerance(t *testing.T) {
	TestSettings_TC_SET_005_MissingFileFaultTolerance(t)
	TestSettings_TC_SET_006_MalformedJSONFaultTolerance(t)
}

// Table-Driven Case Spec: 环境变量与边界解析 (settings.Manager)
func TestSettings_EnvVarTableDriven(t *testing.T) {
	fn := "Manager.Load(envOverrides)"

	tests := []struct {
		name        string
		envMap      map[string]string
		assertFunc  func(t *testing.T, s *Settings)
		expectedErr bool
		notes       string
	}{
		{
			name: "env_model_primary",
			envMap: map[string]string{
				"GP_MODEL": "m-gp",
				"PI_MODEL": "m-pi",
			},
			assertFunc: func(t *testing.T, s *Settings) {
				t.Helper()
				if got, want := s.Model, "m-gp"; got != want {
					t.Errorf("Model: got %q, want %q (GP_MODEL should take priority)", got, want)
				}
			},
			notes: "GP_MODEL 优先于 PI_MODEL",
		},
		{
			name: "env_model_secondary",
			envMap: map[string]string{
				"GP_MODEL": "",
				"PI_MODEL": "m-pi",
			},
			assertFunc: func(t *testing.T, s *Settings) {
				t.Helper()
				if got, want := s.Model, "m-pi"; got != want {
					t.Errorf("Model: got %q, want %q (fallback to PI_MODEL)", got, want)
				}
			},
			notes: "GP_MODEL 为空时降级至 PI_MODEL",
		},
		{
			name: "env_base_url_fallback",
			envMap: map[string]string{
				"GP_BASE_URL":     "",
				"OPENAI_BASE_URL": "https://proxy.ai/v1",
			},
			assertFunc: func(t *testing.T, s *Settings) {
				t.Helper()
				if got, want := s.BaseURL, "https://proxy.ai/v1"; got != want {
					t.Errorf("BaseURL: got %q, want %q (fallback to OPENAI_BASE_URL)", got, want)
				}
			},
			notes: "OPENAI_BASE_URL 备选链生效",
		},
		{
			name: "env_api_key_fallback",
			envMap: map[string]string{
				"GP_API_KEY":     "",
				"OPENAI_API_KEY": "sk-fallback",
			},
			assertFunc: func(t *testing.T, s *Settings) {
				t.Helper()
				if got, want := s.APIKey, "sk-fallback"; got != want {
					t.Errorf("APIKey: got %q, want %q (fallback to OPENAI_API_KEY)", got, want)
				}
			},
			notes: "OPENAI_API_KEY 备选链生效",
		},
		{
			name: "env_concurrency_valid",
			envMap: map[string]string{
				"GP_MAX_CONCURRENCY": "20",
			},
			assertFunc: func(t *testing.T, s *Settings) {
				t.Helper()
				if got, want := s.MaxConcurrency, 20; got != want {
					t.Errorf("MaxConcurrency: got %d, want %d", got, want)
				}
			},
			notes: "正常整数字符串解析",
		},
		{
			name: "env_concurrency_non_numeric",
			envMap: map[string]string{
				"GP_MAX_CONCURRENCY": "invalid_int",
			},
			assertFunc: func(t *testing.T, s *Settings) {
				t.Helper()
				if got, want := s.MaxConcurrency, DefaultMaxConcurrency; got != want {
					t.Errorf("MaxConcurrency: got %d, want %d (retains default on non-numeric)", got, want)
				}
			},
			notes: "非数字静默忽略并保留上一级配置值",
		},
		{
			name: "env_concurrency_non_positive",
			envMap: map[string]string{
				"GP_MAX_CONCURRENCY": "-5",
			},
			assertFunc: func(t *testing.T, s *Settings) {
				t.Helper()
				if got, want := s.MaxConcurrency, DefaultMaxConcurrency; got != want {
					t.Errorf("MaxConcurrency: got %d, want %d (retains default on non-positive)", got, want)
				}
			},
			notes: "非正整数非法值静默忽略",
		},
		{
			name: "env_tools_csv_split",
			envMap: map[string]string{
				"GP_TOOLS": "read,write,bash",
			},
			assertFunc: func(t *testing.T, s *Settings) {
				t.Helper()
				wantTools := []string{"read", "write", "bash"}
				if got := s.DefaultTools; !reflect.DeepEqual(got, wantTools) {
					t.Errorf("DefaultTools: got %v, want %v", got, wantTools)
				}
			},
			notes: "逗号分隔列表解析并裁剪空白",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			cleanEnv(t)
			for k, v := range tt.envMap {
				t.Setenv(k, v)
			}
			tmpHome := t.TempDir()
			tmpCwd := t.TempDir()
			mgr := NewManagerWithPaths(tmpCwd, tmpHome)

			cfg, err := mgr.Load()
			if (err != nil) != tt.expectedErr {
				t.Fatalf("%s(inputs=%+v): got err %v, want expectedErr=%v (%s)", fn, tt.envMap, err, tt.expectedErr, tt.notes)
			}
			if err == nil && tt.assertFunc != nil {
				tt.assertFunc(t, cfg)
			}
		})
	}
}
