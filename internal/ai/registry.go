package ai

import (
	"sort"
	"strings"
	"sync"
)

// ProviderPlugin 描述一个可插拔的大模型 Provider 插件（对齐 Go database/sql 驱动注册模式）
type ProviderPlugin struct {
	ID            string
	Aliases       []string
	DisplayName   string
	DefaultURL    string
	KeyHint       string
	EnvKeys       []string
	DefaultModels []string

	// MatchCredential 判断给定的 apiKey / baseURL / model 是否应由该插件接管
	MatchCredential func(apiKey, baseURL, model string) bool
	// InferModel 判断模型名称是否属于该插件
	InferModel func(model string) bool
	// NewProvider 创建该插件的 ai.Provider 实例
	NewProvider func(apiKey, baseURL string) Provider
	// FetchModels 可选：向插件自定义的线上目录端点拉取模型列表
	FetchModels func(apiKey, baseURL string) ([]string, error)
}

var (
	pluginMu      sync.RWMutex
	plugins       = make(map[string]ProviderPlugin)
	pluginOrder   []string
	pluginAliases = make(map[string]string)
)

// RegisterProviderPlugin 注册一个 Provider 插件（通常在插件包的 init() 中调用）
func RegisterProviderPlugin(p ProviderPlugin) {
	id := strings.ToLower(strings.TrimSpace(p.ID))
	if id == "" {
		return
	}
	p.ID = id
	pluginMu.Lock()
	defer pluginMu.Unlock()
	if _, exists := plugins[id]; !exists {
		pluginOrder = append(pluginOrder, id)
	}
	plugins[id] = p
	pluginAliases[id] = id
	for _, a := range p.Aliases {
		norm := strings.ToLower(strings.TrimSpace(a))
		if norm != "" {
			pluginAliases[norm] = id
		}
	}
}

// NormalizePluginID 将插件别名归一化为规范 ID
func NormalizePluginID(id string) string {
	norm := strings.ToLower(strings.TrimSpace(id))
	pluginMu.RLock()
	defer pluginMu.RUnlock()
	if target, ok := pluginAliases[norm]; ok {
		return target
	}
	return norm
}

// GetProviderPlugin 根据 ID 或别名查找已注册的插件
func GetProviderPlugin(id string) (ProviderPlugin, bool) {
	norm := NormalizePluginID(id)
	pluginMu.RLock()
	defer pluginMu.RUnlock()
	p, ok := plugins[norm]
	return p, ok
}

// ListProviderPlugins 按注册顺序返回全部已挂载的 Provider 插件
func ListProviderPlugins() []ProviderPlugin {
	pluginMu.RLock()
	defer pluginMu.RUnlock()
	out := make([]ProviderPlugin, 0, len(pluginOrder))
	for _, id := range pluginOrder {
		if p, ok := plugins[id]; ok {
			out = append(out, p)
		}
	}
	return out
}

// InferPluginByModel 根据模型名称推断所属的已注册插件 ID
func InferPluginByModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if idx := strings.IndexByte(m, '/'); idx > 0 {
		prefix := m[:idx]
		if p, ok := GetProviderPlugin(prefix); ok {
			return p.ID
		}
		return prefix
	}
	pluginMu.RLock()
	defer pluginMu.RUnlock()
	for _, id := range pluginOrder {
		p := plugins[id]
		if p.InferModel != nil && p.InferModel(m) {
			return p.ID
		}
	}
	return ""
}

// CreateProviderFromPlugins 遍历已注册插件创建匹配的 Provider；若无特殊插件匹配返回 nil
func CreateProviderFromPlugins(providerID, apiKey, baseURL, model string) Provider {
	if providerID != "" {
		if p, ok := GetProviderPlugin(providerID); ok && p.NewProvider != nil {
			if p.MatchCredential == nil || p.MatchCredential(apiKey, baseURL, model) {
				return p.NewProvider(apiKey, baseURL)
			}
		}
	}
	pluginMu.RLock()
	ids := append([]string(nil), pluginOrder...)
	sort.Strings(ids)
	pluginMu.RUnlock()

	for _, id := range ids {
		p, ok := GetProviderPlugin(id)
		if !ok || p.NewProvider == nil || p.MatchCredential == nil {
			continue
		}
		if p.MatchCredential(apiKey, baseURL, model) {
			return p.NewProvider(apiKey, baseURL)
		}
	}
	return nil
}
