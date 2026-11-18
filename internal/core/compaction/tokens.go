package compaction

import (
	"strings"
	"unicode/utf8"

	"gp/internal/ai"
)

// DefaultContextWindow 未匹配到已知模型族时的默认上下文窗口大小 (128k)
const DefaultContextWindow int64 = 128000

// ResolveContextWindow 根据模型标识解析真实的上下文窗口上限（单位：token）。
// 优先匹配常见主流模型家族，未命中时回退至 DefaultContextWindow。
func ResolveContextWindow(modelName string) int64 {
	m := strings.ToLower(strings.TrimSpace(modelName))
	if m == "" || m == "auto" {
		return DefaultContextWindow
	}
	// 去掉 provider 前缀（如 antigravity/gemini-4-argon -> gemini-4-argon）
	if _, after, ok := strings.Cut(m, "/"); ok {
		m = after
	}

	switch {
	case strings.Contains(m, "gemini"),
		strings.Contains(m, "gpt-4.1"),
		strings.Contains(m, "q35"),
		strings.Contains(m, "q3-coder"),
		strings.Contains(m, "qwen3-coder"),
		m == "gmodel", m == "gfmodel",
		strings.HasPrefix(m, "qmodel"), m == "qfmodel":
		return 1000000
	case strings.Contains(m, "kimi"),
		strings.Contains(m, "moonshot"),
		strings.HasPrefix(m, "kmodel"):
		return 256000
	case strings.Contains(m, "claude"),
		strings.Contains(m, "minimax"),
		strings.Contains(m, "glm-4.7"),
		strings.Contains(m, "glm-5"),
		strings.Contains(m, "ultimate"),
		strings.Contains(m, "efficient"),
		strings.Contains(m, "lite"),
		strings.Contains(m, "gpt-5"),
		strings.HasPrefix(m, "o1"),
		strings.HasPrefix(m, "o3"),
		strings.HasPrefix(m, "o4"),
		m == "cmodel", m == "smodel", m == "mmodel":
		return 200000
	case strings.Contains(m, "gpt-4o"),
		strings.Contains(m, "deepseek"),
		strings.Contains(m, "qwen"),
		strings.Contains(m, "glm"),
		m == "dmodel", m == "dfmodel", m == "fast":
		return 128000
	default:
		return DefaultContextWindow
	}
}

// EstimateTextTokens 使用中英文混合启发式估算文本 Token 数。
// ASCII 字符约 3.8 字符/token，非 ASCII（如中文）约 1.5 字符/token。
func EstimateTextTokens(s string) int64 {
	if s == "" {
		return 0
	}
	var asciiCount, nonASCIICount int64
	for _, r := range s {
		if r < utf8.RuneSelf {
			asciiCount++
		} else {
			nonASCIICount++
		}
	}
	tokens := (asciiCount+3)/4 + (nonASCIICount*2+2)/3
	if tokens < 1 {
		return 1
	}
	return tokens
}

// EstimateMessageTokens 估算单条消息（含正文、Thinking、ToolCalls）占用的 Token 数
func EstimateMessageTokens(msg ai.Message) int64 {
	const perMessageOverhead int64 = 6
	total := perMessageOverhead
	total += EstimateTextTokens(msg.Content)
	total += EstimateTextTokens(msg.Thinking)
	for _, tc := range msg.ToolCalls {
		total += EstimateTextTokens(tc.Function.Name)
		total += EstimateTextTokens(string(tc.Function.Arguments))
		total += 4
	}
	return total
}

// EstimateMessagesTokens 估算消息切片的总 Token 占用
func EstimateMessagesTokens(msgs []ai.Message) int64 {
	var total int64
	for _, m := range msgs {
		total += EstimateMessageTokens(m)
	}
	return total
}
