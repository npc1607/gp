package compaction

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gp/internal/ai"
)

func TestContextLedger_LifecycleAndCanonicalDeterminism(t *testing.T) {
	l1 := NewContextLedger()
	l1.Apply("add", "cst_db", "tech_stack", "Use MySQL 8.0", "", []string{"entry_10"}, 1)
	l1.Apply("add", "cst_net", "environment", "Offline VPC only", "", []string{"entry_12"}, 2)
	l1.Apply("supersede", "cst_db_v2", "tech_stack", "Migrate fully to PostgreSQL 16", "cst_db", []string{"entry_55"}, 10)
	l1.Apply("add", "cst_issue", "issue", "Fix flaky build", "", []string{"entry_60"}, 11)
	l1.Apply("resolve", "cst_issue", "issue", "", "", []string{"entry_72"}, 15)

	// Build second ledger in reverse insertion order to verify canonical determinism.
	l2 := NewContextLedger()
	l2.Apply("add", "cst_net", "environment", "Offline VPC only", "", []string{"entry_12"}, 2)
	l2.Apply("add", "cst_db", "tech_stack", "Use MySQL 8.0", "", []string{"entry_10"}, 1)
	l2.Apply("add", "cst_issue", "issue", "Fix flaky build", "", []string{"entry_60"}, 11)
	l2.Apply("resolve", "cst_issue", "issue", "", "", []string{"entry_72"}, 15)
	l2.Apply("supersede", "cst_db_v2", "tech_stack", "Migrate fully to PostgreSQL 16", "cst_db", []string{"entry_55"}, 10)

	r1 := l1.CanonicalRender()
	r2 := l2.CanonicalRender()
	if r1 == "" || r1 != r2 {
		t.Fatalf("expected identical canonical renders regardless of insertion order:\nr1:\n%s\nr2:\n%s", r1, r2)
	}
	if !strings.Contains(r1, "cst_db_v2") || !strings.Contains(r1, "supersedes:cst_db") {
		t.Fatalf("expected active cst_db_v2 with supersedes footnote, got:\n%s", r1)
	}
	if strings.Contains(r1, "Use MySQL 8.0") || strings.Contains(r1, "cst_issue") {
		t.Fatalf("superseded/resolved constraints should not appear as active in CanonicalRender:\n%s", r1)
	}

	sup := l1.SupersededEntryIDs()
	if _, ok := sup["entry_10"]; !ok {
		t.Fatalf("expected entry_10 in SupersededEntryIDs, got %+v", sup)
	}
	if _, ok := sup["entry_55"]; ok {
		t.Fatalf("did not expect active entry_55 in SupersededEntryIDs")
	}

	// JSON roundtrip check
	raw, err := json.Marshal(l1)
	if err != nil {
		t.Fatalf("marshal ledger: %v", err)
	}
	restored, err := UnmarshalContextLedger(raw)
	if err != nil {
		t.Fatalf("UnmarshalContextLedger: %v", err)
	}
	if restored.CanonicalRender() != r1 {
		t.Fatalf("restored ledger canonical render mismatch:\ngot:\n%s\nwant:\n%s", restored.CanonicalRender(), r1)
	}
}

func TestCompactWithEntries_StructuredLLMLedgerAndFallback(t *testing.T) {
	entries := []EntryRecord{
		{EntryID: "entry_1", Message: ai.NewUserMessage("Initial DB: MySQL 8.0")},
		{EntryID: "entry_2", Message: ai.NewAssistantMessage("Noted MySQL 8.0", "")},
		{EntryID: "entry_3", Message: ai.NewUserMessage("Switch DB to PostgreSQL 16, drop MySQL")},
		{EntryID: "entry_4", Message: ai.NewAssistantMessage("Switched to PostgreSQL 16", "")},
		{EntryID: "entry_5", Message: ai.NewUserMessage("Recent step A")},
		{EntryID: "entry_6", Message: ai.NewAssistantMessage("Ack A", "")},
		{EntryID: "entry_7", Message: ai.NewUserMessage("Recent step B")},
		{EntryID: "entry_8", Message: ai.NewAssistantMessage("Ack B", "")},
	}

	initialLedger := NewContextLedger()
	initialLedger.Apply("add", "cst_db_1", "tech_stack", "Use MySQL 8.0", "", []string{"entry_1"}, 0)

	jsonPayload := "```json\n" + `{
  "ledger_events": [
    {
      "action": "supersede",
      "id": "cst_db_2",
      "category": "tech_stack",
      "statement": "Use PostgreSQL 16 only",
      "supersedes_id": "cst_db_1",
      "provenance": ["entry_3"]
    }
  ],
  "handoff_notes": "Switched database stack; ready to run migrations."
}` + "\n```"

	prov := &mockCompactProvider{summary: jsonPayload}
	res, err := CompactWithEntries(context.Background(), prov, "claude-4-sonnet", entries, Options{
		KeepRecentTurns:    2,
		KeepRecentMessages: 4,
		Ledger:             initialLedger,
	})
	if err != nil {
		t.Fatalf("CompactWithEntries() error = %v", err)
	}
	if !res.UsedLLM {
		t.Fatalf("expected UsedLLM=true")
	}
	if res.HandoffNotes != "Switched database stack; ready to run migrations." {
		t.Fatalf("unexpected HandoffNotes: %q", res.HandoffNotes)
	}
	rendered := res.Ledger.CanonicalRender()
	if !strings.Contains(rendered, "PostgreSQL 16") || !strings.Contains(rendered, "entry_3") || strings.Contains(rendered, "MySQL 8.0") {
		t.Fatalf("unexpected ledger after structured compaction:\n%s", rendered)
	}

	// Ensure invalid JSON or nil provider preserves existing ledger without heuristic corruption.
	badProv := &mockCompactProvider{summary: "not-json-plain-text"}
	resBad, err := CompactWithEntries(context.Background(), badProv, "claude-4-sonnet", entries, Options{
		KeepRecentTurns:    2,
		KeepRecentMessages: 4,
		Ledger:             initialLedger,
	})
	if err != nil {
		t.Fatalf("CompactWithEntries(bad json) error = %v", err)
	}
	if resBad.Ledger.Version != initialLedger.Version {
		t.Fatalf("expected invalid JSON not to mutate ContextLedger version (%d vs %d)", resBad.Ledger.Version, initialLedger.Version)
	}
	if resBad.Summary != "not-json-plain-text" {
		t.Fatalf("expected plain text fallback summary, got %q", resBad.Summary)
	}

	// Ensure nil provider returns extractive fallback summary with entry IDs and leaves ledger untouched.
	resNil, err := CompactWithEntries(context.Background(), nil, "", entries, Options{
		KeepRecentTurns:    2,
		KeepRecentMessages: 4,
		Ledger:             initialLedger,
	})
	if err != nil || resNil.UsedLLM {
		t.Fatalf("CompactWithEntries(nil prov) err=%v usedLLM=%v", err, resNil.UsedLLM)
	}
	if !strings.Contains(resNil.Summary, fmt.Sprintf("[%s]", entries[0].EntryID)) {
		t.Fatalf("expected fallback summary to preserve entry ID reference, got:\n%s", resNil.Summary)
	}
}
