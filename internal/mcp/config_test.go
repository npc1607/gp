package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMCP_TC_MCP_006_ConfigCascadingMerge(t *testing.T) {
	fn := "DiscoverConfig"
	homeDir := t.TempDir()
	cwdDir := t.TempDir()

	// 1. Global config in homeDir/.gp/mcp.json
	globalDir := filepath.Join(homeDir, ".gp")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatalf("%s: mkdir global dir error %v, want nil", fn, err)
	}
	globalJSON := `{
  "mcpServers": {
    "common": { "command": "echo", "args": ["common-global"] },
    "override_server": { "command": "node", "args": ["global.js"] }
  }
}`
	if err := os.WriteFile(filepath.Join(globalDir, "mcp.json"), []byte(globalJSON), 0644); err != nil {
		t.Fatalf("%s: write global mcp.json error %v, want nil", fn, err)
	}

	// 2. Local config in cwdDir/.gp/mcp.json
	localDir := filepath.Join(cwdDir, ".gp")
	if err := os.MkdirAll(localDir, 0755); err != nil {
		t.Fatalf("%s: mkdir local dir error %v, want nil", fn, err)
	}
	localJSON := `{
  "mcpServers": {
    "override_server": { "command": "node", "args": ["local.js"] },
    "local_only": { "command": "python", "args": ["local.py"] }
  }
}`
	if err := os.WriteFile(filepath.Join(localDir, "mcp.json"), []byte(localJSON), 0644); err != nil {
		t.Fatalf("%s: write local mcp.json error %v, want nil", fn, err)
	}

	cfg, err := DiscoverConfig(cwdDir, homeDir)
	if err != nil {
		t.Fatalf("%s(cwd=%q, home=%q): got error %v, want nil", fn, cwdDir, homeDir, err)
	}
	if cfg == nil {
		t.Fatalf("%s(cwd=%q, home=%q): got nil config, want non-nil", fn, cwdDir, homeDir)
	}

	servers := cfg.GetServers()
	if got, want := len(servers), 3; got != want {
		t.Fatalf("%s(cwd=%q, home=%q): got %d servers, want %d", fn, cwdDir, homeDir, got, want)
	}

	// Verify common from global
	commonSrv, ok := servers["common"]
	if !ok || len(commonSrv.Args) == 0 || commonSrv.Args[0] != "common-global" {
		t.Fatalf("%s: got common server %+v, want args=[%q]", fn, commonSrv, "common-global")
	}

	// Verify override_server overridden by local
	overrideSrv, ok := servers["override_server"]
	if !ok || len(overrideSrv.Args) == 0 || overrideSrv.Args[0] != "local.js" {
		t.Fatalf("%s: got override_server %+v, want args=[%q]", fn, overrideSrv, "local.js")
	}

	// Verify local_only from local
	localOnlySrv, ok := servers["local_only"]
	if !ok || len(localOnlySrv.Args) == 0 || localOnlySrv.Args[0] != "local.py" {
		t.Fatalf("%s: got local_only server %+v, want args=[%q]", fn, localOnlySrv, "local.py")
	}
}
