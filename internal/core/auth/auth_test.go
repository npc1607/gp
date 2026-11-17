package auth

import (
	"os"
	"path/filepath"
	"testing"

	_ "gp/internal/ai/providers/openai"
	_ "gp/plugins/qoder"
)

func TestAuthStore_LoginLogoutAndAliases(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, ".gp", "auth.db")
	store := NewStoreWithPath(dbPath)
	defer func() { _ = store.Close() }()

	// 1. Login via alias "qcorder" -> normalized to "qoder"
	if err := store.Set("qcorder", Credential{Type: "api_key", Key: "pt-test-123"}); err != nil {
		t.Fatalf("Set(qcorder) failed: %v", err)
	}
	if err := store.Set("deepseek", Credential{Type: "api_key", Key: "sk-ds-456"}); err != nil {
		t.Fatalf("Set(deepseek) failed: %v", err)
	}
	if err := store.SaveProviderModels("qoder", []string{"auto", "ultimate", "q35-plus", "kimi-k2.5"}); err != nil {
		t.Fatalf("SaveProviderModels failed: %v", err)
	}

	// Verify file permissions 0600
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat auth.db failed: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("auth.db perm got %o, want 0600", perm)
	}

	// Resolve qoder & alias qcorder
	key, baseURL, src, ok := store.ResolveCredential("qoder")
	if !ok || key != "pt-test-123" || baseURL != "https://api.qoder.com/v1" || src != "sqlite" {
		t.Fatalf("ResolveCredential(qoder) got (%q, %q, %q, %v)", key, baseURL, src, ok)
	}
	key2, _, _, ok2 := store.ResolveCredential("qcorder")
	if !ok2 || key2 != "pt-test-123" {
		t.Fatalf("ResolveCredential(qcorder) alias lookup failed: (%q, %v)", key2, ok2)
	}

	// ListModels marks both qoder and deepseek authenticated
	models := store.ListModels("", "", "")
	var foundQoder, foundDS bool
	for _, m := range models {
		if m.Provider == "qoder" && m.Authenticated {
			foundQoder = true
		}
		if m.Provider == "deepseek" && m.Authenticated {
			foundDS = true
		}
	}
	if !foundQoder || !foundDS {
		t.Fatalf("ListModels expected both qoder and deepseek authenticated, got %+v", models)
	}

	// Logout qoder
	removed, err := store.Remove("qoder")
	if err != nil || !removed {
		t.Fatalf("Remove(qoder) got (%v, %v), want (true, nil)", removed, err)
	}
	if _, _, _, ok := store.ResolveCredential("qoder"); ok {
		t.Fatalf("expected qoder credential removed after logout")
	}
}
