package main

import (
	"bytes"
	"context"
	"flag"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gp/internal/modes"
)

func buildBinary(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "gp_test_bin")
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("buildBinary: go build failed: %v, output: %s", err, string(out))
	}
	return binPath
}

func TestVersion(t *testing.T) {
	fn := "cmd/gp.Version"
	if got := Version; !strings.HasPrefix(got, "v") {
		t.Fatalf("%s: got %q, want prefix 'v'", fn, got)
	}
}

// TC-CLI-01: CLI resume命令及标志位解析
func TestCLI_TC_CLI_01_ResumeCommandAndFlags(t *testing.T) {
	fn := "cmd/gp.ParseResumeFlags"

	// 1. Table-driven 标志位解析单元测试
	tests := []struct {
		name          string
		args          []string
		wantDB        string
		wantModel     string
		wantTools     string
		wantSessionID string
	}{
		{
			name:          "resume_subcommand_defaults",
			args:          []string{},
			wantDB:        ".gp/gp.db",
			wantModel:     "",
			wantTools:     "",
			wantSessionID: "",
		},
		{
			name:          "resume_subcommand_custom_args",
			args:          []string{"-db", "custom.db", "-model", "custom-model", "-tools", "read,write", "sess_123"},
			wantDB:        "custom.db",
			wantModel:     "custom-model",
			wantTools:     "read,write",
			wantSessionID: "sess_123",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			resumeCmd := flag.NewFlagSet("resume", flag.ContinueOnError)
			resumeDB := resumeCmd.String("db", ".gp/gp.db", "SQLite 数据库路径")
			resumeModel := resumeCmd.String("model", "", "指定模型标识")
			resumeTools := resumeCmd.String("tools", "", "工具白名单列表")

			if err := resumeCmd.Parse(tc.args); err != nil {
				t.Fatalf("%s(args=%v): got parse error %v, want nil", fn, tc.args, err)
			}

			if got, want := *resumeDB, tc.wantDB; got != want {
				t.Fatalf("%s(args=%v): db got %q, want %q", fn, tc.args, got, want)
			}
			if got, want := *resumeModel, tc.wantModel; got != want {
				t.Fatalf("%s(args=%v): model got %q, want %q", fn, tc.args, got, want)
			}
			if got, want := *resumeTools, tc.wantTools; got != want {
				t.Fatalf("%s(args=%v): tools got %q, want %q", fn, tc.args, got, want)
			}
			var gotSession string
			if len(resumeCmd.Args()) > 0 {
				gotSession = resumeCmd.Args()[0]
			}
			if got, want := gotSession, tc.wantSessionID; got != want {
				t.Fatalf("%s(args=%v): sessionID got %q, want %q", fn, tc.args, got, want)
			}
		})
	}

	// 2. Runner 逻辑测试：未完成任务缺省时不崩溃
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "empty_session.db")

	var buf bytes.Buffer
	runner := &modes.ResumeRunner{
		Opts: modes.ResumeRunnerOptions{
			DBPath: dbPath,
			Writer: &buf,
		},
	}

	if err := runner.Run(context.Background(), ""); err != nil {
		t.Fatalf("%s: runner.Run on empty DB got error %v, want nil", fn, err)
	}

	if output := buf.String(); !strings.Contains(output, "No unfinished tasks found to resume.") {
		t.Fatalf("%s: runner output %q does not contain %q", fn, output, "No unfinished tasks found to resume.")
	}

	// 3. Integration 二进制执行测试：gp resume 与 gp --resume --db=...
	binPath := buildBinary(t)

	// 3a. 执行 gp resume -db=<tmpDir>/bin_test.db
	binDbPath := filepath.Join(tmpDir, "bin_test.db")
	cmdResumeSub := exec.Command(binPath, "resume", "-db", binDbPath)
	outSub, err := cmdResumeSub.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: exec %s resume -db %s got error %v, output: %s", fn, binPath, binDbPath, err, string(outSub))
	}
	if !strings.Contains(string(outSub), "No unfinished tasks found to resume.") {
		t.Fatalf("%s: exec output %q does not contain expected graceful exit message", fn, string(outSub))
	}

	// 3b. 执行 gp --resume --db=<tmpDir>/bin_test.db
	cmdResumeFlag := exec.Command(binPath, "--resume", "--db", binDbPath)
	outFlag, err := cmdResumeFlag.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: exec %s --resume --db %s got error %v, output: %s", fn, binPath, binDbPath, err, string(outFlag))
	}
	if !strings.Contains(string(outFlag), "No unfinished tasks found to resume.") {
		t.Fatalf("%s: exec output %q does not contain expected graceful exit message", fn, string(outFlag))
	}
}
