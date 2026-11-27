package ai

import "encoding/json"

// SanitizeToolSchema 规范化工具参数的 JSON Schema，使其兼容 Moonshot/Kimi、OpenAI、Anthropic 等严格校验方言。
// 仅修正非对象类型的 items（如 bool / array / null），对本身合法的 Schema 保持原样不变。
func SanitizeToolSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil || root == nil {
		return raw
	}
	if !sanitizeSchemaObject(root) {
		return raw
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return raw
	}
	return encoded
}

// SanitizeToolDefinition 对已反序列化的 OpenAI 风格 tool 定义（{"type":"function","function":{"parameters":...}}）就地清洗。
func SanitizeToolDefinition(tool any) {
	m, ok := tool.(map[string]any)
	if !ok {
		return
	}
	if fn, ok := m["function"].(map[string]any); ok {
		if params, ok := fn["parameters"].(map[string]any); ok {
			sanitizeSchemaObject(params)
		}
	}
	if params, ok := m["parameters"].(map[string]any); ok {
		sanitizeSchemaObject(params)
	}
}

func sanitizeSchemaObject(obj map[string]any) bool {
	if obj == nil {
		return false
	}
	changed := false

	// 关键修复：Moonshot/Kimi 等上游严格要求 JSON Schema 的 items 必须是 Object，不能是 bool/array/null
	if rawItems, exists := obj["items"]; exists {
		switch iv := rawItems.(type) {
		case map[string]any:
			if sanitizeSchemaObject(iv) {
				changed = true
			}
		case []any:
			// Draft-07 tuple array items -> 转为 anyOf 对象保留各元素类型提示
			var elems []any
			for _, elem := range iv {
				if em, ok := elem.(map[string]any); ok {
					sanitizeSchemaObject(em)
					elems = append(elems, em)
				}
			}
			if len(elems) == 1 {
				obj["items"] = elems[0]
			} else if len(elems) > 1 {
				obj["items"] = map[string]any{"anyOf": elems}
			} else {
				obj["items"] = map[string]any{}
			}
			changed = true
		default:
			// bool (true/false) 或 nil -> 等价空对象 schema {}
			obj["items"] = map[string]any{}
			changed = true
		}
	}

	// 递归清洗常见嵌套子 schema 容器
	for _, mapKey := range []string{"properties", "$defs", "definitions", "patternProperties", "dependentSchemas"} {
		if sub, ok := obj[mapKey].(map[string]any); ok {
			for k, v := range sub {
				if vm, ok := v.(map[string]any); ok {
					if sanitizeSchemaObject(vm) {
						changed = true
					}
				} else if _, isBool := v.(bool); isBool && (mapKey == "$defs" || mapKey == "definitions") {
					sub[k] = map[string]any{}
					changed = true
				}
			}
		}
	}

	for _, arrKey := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
		if list, ok := obj[arrKey].([]any); ok {
			for _, item := range list {
				if im, ok := item.(map[string]any); ok {
					if sanitizeSchemaObject(im) {
						changed = true
					}
				}
			}
		}
	}

	for _, singleKey := range []string{"additionalProperties", "not", "if", "then", "else", "contains", "propertyNames"} {
		if sub, ok := obj[singleKey].(map[string]any); ok {
			if sanitizeSchemaObject(sub) {
				changed = true
			}
		}
	}

	return changed
}
