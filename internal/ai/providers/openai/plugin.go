package openai

import (
	"strings"

	"gp/internal/ai"
)

func init() {
	ai.RegisterProviderPlugin(ai.ProviderPlugin{
		ID:          "deepseek",
		Aliases:     []string{"ds"},
		DisplayName: "DeepSeek",
		DefaultURL:  "https://api.deepseek.com/v1",
		KeyHint:     "sk-...",
		EnvKeys:     []string{"DEEPSEEK_API_KEY"},
		DefaultModels: []string{
			"deepseek-flash",
			"deepseek-v4-pro",
			"deepseek-chat",
			"deepseek-reasoner",
		},
		InferModel: func(model string) bool {
			return strings.Contains(strings.ToLower(model), "deepseek")
		},
		MatchCredential: func(apiKey, baseURL, model string) bool {
			return strings.Contains(strings.ToLower(baseURL), "deepseek")
		},
		NewProvider: func(apiKey, baseURL string) ai.Provider {
			if strings.TrimSpace(baseURL) == "" {
				baseURL = "https://api.deepseek.com/v1"
			}
			return NewProvider(apiKey, baseURL)
		},
	})
}
