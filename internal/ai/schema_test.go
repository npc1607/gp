package ai

import (
	"encoding/json"
	"testing"
)

func TestSanitizeToolSchema_FixesNonObjectItems(t *testing.T) {
	// 复现 Moonshot/Kimi 报错路径: $defs.block.properties.rows.items.items 为 bool 或 tuple array
	raw := json.RawMessage(`{
		"type": "object",
		"$defs": {
			"block": {
				"type": "object",
				"properties": {
					"rows": {
						"type": "array",
						"items": {
							"type": "array",
							"items": true
						}
					},
					"tuple": {
						"type": "array",
						"items": [{"type": "string"}, {"type": "number"}]
					}
				}
			}
		},
		"properties": {
			"id": {"type": "string"}
		}
	}`)

	cleaned := SanitizeToolSchema(raw)
	var doc map[string]any
	if err := json.Unmarshal(cleaned, &doc); err != nil {
		t.Fatalf("unmarshal cleaned schema failed: %v", err)
	}

	defs := doc["$defs"].(map[string]any)
	block := defs["block"].(map[string]any)
	props := block["properties"].(map[string]any)
	rows := props["rows"].(map[string]any)
	rowsItems := rows["items"].(map[string]any)
	if _, ok := rowsItems["items"].(map[string]any); !ok {
		t.Fatalf("expected $defs.block.properties.rows.items.items to be object, got %T (%v)", rowsItems["items"], rowsItems["items"])
	}

	tuple := props["tuple"].(map[string]any)
	tupleItems, ok := tuple["items"].(map[string]any)
	if !ok {
		t.Fatalf("expected tuple items to be converted to object, got %T", tuple["items"])
	}
	if _, hasAnyOf := tupleItems["anyOf"]; !hasAnyOf {
		t.Fatalf("expected tuple items to preserve types via anyOf, got %v", tupleItems)
	}
}

func TestSanitizeToolSchema_ValidSchemaUntouched(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
	got := SanitizeToolSchema(raw)
	if string(got) != string(raw) {
		t.Fatalf("expected valid schema to remain untouched, got %s", string(got))
	}
}
