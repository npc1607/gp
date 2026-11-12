package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	DefaultModel          = "antigravity/gemini-4-argon"
	DefaultBaseURL        = "https://api.openai.com/v1"
	DefaultThinkingLevel  = "off"
	DefaultMaxConcurrency = 10
	DefaultTokensPerMin   = 100000
)

// Settings 包含了 gp harness 的完整运行时与调度配置
type Settings struct {
	Model          string   `json:"model,omitempty"`
	BaseURL        string   `json:"base_url,omitempty"`
	APIKey         string   `json:"api_key,omitempty"`
	ThinkingLevel  string   `json:"thinking_level,omitempty"`
	DefaultTools   []string `json:"default_tools,omitempty"`
	MaxConcurrency int      `json:"max_concurrency,omitempty"`
	TokensPerMin   int      `json:"tokens_per_min,omitempty"`
	SystemPrompt   string   `json:"system_prompt,omitempty"`
}

// rawSettings 用于辅助 JSON 反序列化时区分 omitted 与 empty，防止未声明字段覆盖前序层叠值
type rawSettings struct {
	Model          *string   `json:"model"`
	BaseURL        *string   `json:"base_url"`
	APIKey         *string   `json:"api_key"`
	ThinkingLevel  *string   `json:"thinking_level"`
	DefaultTools   *[]string `json:"default_tools"`
	MaxConcurrency *int      `json:"max_concurrency"`
	TokensPerMin   *int      `json:"tokens_per_min"`
	SystemPrompt   *string   `json:"system_prompt"`
}

// DefaultSettings 返回包含标准缺省值的配置对象
func DefaultSettings() Settings {
	return Settings{
		Model:          DefaultModel,
		BaseURL:        DefaultBaseURL,
		APIKey:         "",
		ThinkingLevel:  DefaultThinkingLevel,
		DefaultTools:   []string{"read", "write", "edit", "bash"},
		MaxConcurrency: DefaultMaxConcurrency,
		TokensPerMin:   DefaultTokensPerMin,
	}
}

// Clone 深拷贝 Settings 结构体及其切片字段
func (s Settings) Clone() Settings {
	cloned := s
	if s.DefaultTools != nil {
		cloned.DefaultTools = make([]string, len(s.DefaultTools))
		copy(cloned.DefaultTools, s.DefaultTools)
	}
	return cloned
}

// Manager 统一配置管理器，负责用户级与项目级层叠合并及持久化
type Manager struct {
	cwd     string
	homeDir string
}

// NewManager 使用当前工作目录及系统用户 Home 目录创建配置管理器
func NewManager(cwd string) *Manager {
	if cwd == "" {
		if dir, err := os.Getwd(); err == nil {
			cwd = dir
		} else {
			cwd = "."
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	return &Manager{
		cwd:     cwd,
		homeDir: home,
	}
}

// NewManagerWithPaths 显式注入路径，便于单元测试完全隔离真实环境
func NewManagerWithPaths(cwd, homeDir string) *Manager {
	if cwd == "" {
		cwd = "."
	}
	if homeDir == "" {
		homeDir = "."
	}
	return &Manager{
		cwd:     cwd,
		homeDir: homeDir,
	}
}

// GlobalPath 返回全局用户级配置文件路径 (~/.gp/settings.json)
func (m *Manager) GlobalPath() string {
	return filepath.Join(m.homeDir, ".gp", "settings.json")
}

// ProjectPath 返回项目级配置文件路径 (<cwd>/.gp/settings.json)
func (m *Manager) ProjectPath() string {
	return filepath.Join(m.cwd, ".gp", "settings.json")
}

// Load 按优先级合并加载配置：默认值 -> 全局配置 -> 项目配置 -> 环境变量
func (m *Manager) Load() (*Settings, error) {
	s := DefaultSettings()

	if err := m.loadFromFile(m.GlobalPath(), &s); err != nil {
		return nil, fmt.Errorf("load global settings from %s: %w", m.GlobalPath(), err)
	}

	if err := m.loadFromFile(m.ProjectPath(), &s); err != nil {
		return nil, fmt.Errorf("load project settings from %s: %w", m.ProjectPath(), err)
	}

	m.applyEnvOverrides(&s)
	res := s.Clone()
	return &res, nil
}

func (m *Manager) loadFromFile(path string, target *Settings) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil
	}

	var raw rawSettings
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	if raw.Model != nil {
		target.Model = *raw.Model
	}
	if raw.BaseURL != nil {
		target.BaseURL = *raw.BaseURL
	}
	if raw.APIKey != nil {
		target.APIKey = *raw.APIKey
	}
	if raw.ThinkingLevel != nil {
		target.ThinkingLevel = *raw.ThinkingLevel
	}
	if raw.DefaultTools != nil {
		tools := make([]string, len(*raw.DefaultTools))
		copy(tools, *raw.DefaultTools)
		target.DefaultTools = tools
	}
	if raw.MaxConcurrency != nil {
		target.MaxConcurrency = *raw.MaxConcurrency
	}
	if raw.TokensPerMin != nil {
		target.TokensPerMin = *raw.TokensPerMin
	}
	if raw.SystemPrompt != nil {
		target.SystemPrompt = *raw.SystemPrompt
	}
	return nil
}

func (m *Manager) applyEnvOverrides(target *Settings) {
	if val := os.Getenv("GP_MODEL"); val != "" {
		target.Model = val
	} else if val := os.Getenv("PI_MODEL"); val != "" {
		target.Model = val
	}

	if val := os.Getenv("GP_BASE_URL"); val != "" {
		target.BaseURL = val
	} else if val := os.Getenv("OPENAI_BASE_URL"); val != "" {
		target.BaseURL = val
	}

	if val := os.Getenv("GP_API_KEY"); val != "" {
		target.APIKey = val
	} else if val := os.Getenv("OPENAI_API_KEY"); val != "" {
		target.APIKey = val
	}

	if val := os.Getenv("GP_THINKING_LEVEL"); val != "" {
		target.ThinkingLevel = val
	}

	if val := os.Getenv("GP_SYSTEM_PROMPT"); val != "" {
		target.SystemPrompt = val
	}

	if val := os.Getenv("GP_MAX_CONCURRENCY"); val != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && n > 0 {
			target.MaxConcurrency = n
		}
	}

	if val := os.Getenv("GP_TOKENS_PER_MIN"); val != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && n > 0 {
			target.TokensPerMin = n
		}
	}

	if val := os.Getenv("GP_TOOLS"); val != "" {
		var tools []string
		for _, t := range strings.Split(val, ",") {
			if trimmed := strings.TrimSpace(t); trimmed != "" {
				tools = append(tools, trimmed)
			}
		}
		if len(tools) > 0 {
			target.DefaultTools = tools
		}
	}
}

// SaveToFile 将配置序列化格式化保存至指定文件
func (m *Manager) SaveToFile(path string, s Settings) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write settings to %s: %w", path, err)
	}
	return nil
}

// SaveGlobal 保存配置至全局配置文件 (~/.gp/settings.json)
func (m *Manager) SaveGlobal(s Settings) error {
	return m.SaveToFile(m.GlobalPath(), s)
}

// SaveProject 保存配置至项目级配置文件 (<cwd>/.gp/settings.json)
func (m *Manager) SaveProject(s Settings) error {
	return m.SaveToFile(m.ProjectPath(), s)
}
