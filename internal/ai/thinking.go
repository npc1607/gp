package ai

import (
	"strings"
	"sync"
)

// ThinkingOption 描述单个厂商/模型支持的思考深度档位
type ThinkingOption struct {
	ID           string // 传给 Provider 的规范标识（如 "off", "minimal", "low", "medium", "high", "xhigh", "max", "on"）
	Label        string // UI 展示标签（为空时回退为 ID）
	BudgetTokens int    // 可选：面向按 Token 预算控制推理的模型（如 Claude/Gemini）的建议预算
}

// DisplayLabel 返回用于界面展示的档位名称
func (o ThinkingOption) DisplayLabel() string {
	if strings.TrimSpace(o.Label) != "" {
		return o.Label
	}
	return o.ID
}

// ThinkingProfile 描述某模型或 Provider 支持的思考深度集合与默认值
type ThinkingProfile struct {
	Options []ThinkingOption
	Default string
}

// OptionIDs 返回全部可选档位 ID 列表
func (p ThinkingProfile) OptionIDs() []string {
	out := make([]string, 0, len(p.Options))
	for _, o := range p.Options {
		out = append(out, o.ID)
	}
	return out
}

// ThinkingCapable 可选扩展接口：Provider 可实现此接口声明各模型特有的思考深度档位
type ThinkingCapable interface {
	ThinkingProfile(modelName string) ThinkingProfile
}

// ThinkingProfileMatcher 根据 providerID 与 modelName 动态匹配思考深度配置
type ThinkingProfileMatcher func(providerID, modelName string) (ThinkingProfile, bool)

var (
	thinkingMu       sync.RWMutex
	thinkingMatchers []ThinkingProfileMatcher
)

// RegisterThinkingProfileMatcher 注册自定义思考深度配置匹配器（后注册的优先生效）
func RegisterThinkingProfileMatcher(m ThinkingProfileMatcher) {
	if m == nil {
		return
	}
	thinkingMu.Lock()
	defer thinkingMu.Unlock()
	thinkingMatchers = append(thinkingMatchers, m)
}

// DefaultThinkingProfile 返回通用的四档思考深度配置（off / low / medium / high）
func DefaultThinkingProfile() ThinkingProfile {
	return ThinkingProfile{
		Default: "off",
		Options: []ThinkingOption{
			{ID: "off", Label: "off"},
			{ID: "low", Label: "low", BudgetTokens: 2048},
			{ID: "medium", Label: "medium", BudgetTokens: 8192},
			{ID: "high", Label: "high", BudgetTokens: 16384},
		},
	}
}

// ResolveThinkingProfile 解析指定 Provider 与模型支持的思考深度档位配置。
// 优先顺序：1) Provider 实现 ThinkingCapable 接口；2) 已注册的自定义匹配器；3) 内置主流模型族规则；4) 通用四档默认配置。
func ResolveThinkingProfile(prov Provider, providerID, modelName string) ThinkingProfile {
	if prov != nil {
		if tc, ok := prov.(ThinkingCapable); ok {
			if prof := tc.ThinkingProfile(modelName); len(prof.Options) > 0 {
				return prof
			}
		}
	}

	thinkingMu.RLock()
	for i := len(thinkingMatchers) - 1; i >= 0; i-- {
		if prof, ok := thinkingMatchers[i](providerID, modelName); ok && len(prof.Options) > 0 {
			thinkingMu.RUnlock()
			return prof
		}
	}
	thinkingMu.RUnlock()

	m := strings.ToLower(strings.TrimSpace(modelName))
	if _, after, ok := strings.Cut(m, "/"); ok {
		m = after
	}

	switch {
	case strings.Contains(m, "gpt-5"),
		strings.HasPrefix(m, "o1"),
		strings.HasPrefix(m, "o3"),
		strings.HasPrefix(m, "o4"):
		return ThinkingProfile{
			Default: "off",
			Options: []ThinkingOption{
				{ID: "off", Label: "off"},
				{ID: "minimal", Label: "minimal", BudgetTokens: 1024},
				{ID: "low", Label: "low", BudgetTokens: 4096},
				{ID: "medium", Label: "medium", BudgetTokens: 8192},
				{ID: "high", Label: "high", BudgetTokens: 16384},
				{ID: "xhigh", Label: "xhigh", BudgetTokens: 32768},
			},
		}
	case strings.Contains(m, "claude"),
		strings.Contains(m, "gemini"),
		m == "cmodel", m == "smodel", m == "gmodel", m == "gfmodel":
		return ThinkingProfile{
			Default: "off",
			Options: []ThinkingOption{
				{ID: "off", Label: "off"},
				{ID: "low", Label: "low", BudgetTokens: 2048},
				{ID: "medium", Label: "medium", BudgetTokens: 8192},
				{ID: "high", Label: "high", BudgetTokens: 16384},
				{ID: "max", Label: "max", BudgetTokens: 32768},
			},
		}
	case strings.Contains(m, "deepseek-r1"),
		strings.Contains(m, "deepseek-reasoner"):
		return ThinkingProfile{
			Default: "on",
			Options: []ThinkingOption{
				{ID: "off", Label: "off"},
				{ID: "on", Label: "on"},
			},
		}
	default:
		return DefaultThinkingProfile()
	}
}
