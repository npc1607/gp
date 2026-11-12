package builtin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gp/internal/tools"
)

func setupEditTestFile(t *testing.T, dir, filename, content string) string {
	t.Helper()
	filePath := filepath.Join(dir, filename)
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("setupEditTestFile(filename=%s): got error %v, want nil", filename, err)
	}
	t.Cleanup(func() {
		_ = os.Remove(filePath)
	})
	return filePath
}

func TestEditTool_Basic(t *testing.T) {
	fn := "EditTool.Execute"
	tmpDir := t.TempDir()
	initial := "package main\n\nfunc main() {\n\tprintln(\"old\")\n}\n"
	filePath := setupEditTestFile(t, tmpDir, "code.go", initial)

	tool := &EditTool{}
	if got, want := tool.Name(), "edit"; got != want {
		t.Errorf("EditTool.Name(): got %q, want %q", got, want)
	}

	inputs := editArgs{
		Path:    filePath,
		OldText: `println("old")`,
		NewText: `println("new")`,
	}
	rawArgs, _ := json.Marshal(inputs)

	res, err := tool.Execute(&tools.ToolContext{Cwd: tmpDir}, rawArgs)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got unexpected go error %v, want nil", fn, inputs, err)
	}
	if got, want := res.IsError, false; got != want {
		t.Fatalf("%s(inputs=%+v): got IsError=%v, want %v (content=%q)", fn, inputs, got, want, res.Content)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got read file error %v, want nil", fn, inputs, err)
	}
	expected := "package main\n\nfunc main() {\n\tprintln(\"new\")\n}\n"
	if got, want := string(data), expected; got != want {
		t.Fatalf("%s(inputs=%+v): got file content %q, want %q", fn, inputs, got, want)
	}
}

func TestEditTool_OldTextNotFound(t *testing.T) {
	fn := "EditTool.Execute"
	tmpDir := t.TempDir()
	filePath := setupEditTestFile(t, tmpDir, "file.txt", "alpha beta gamma")

	tool := &EditTool{}
	inputs := editArgs{
		Path:    filePath,
		OldText: "delta",
		NewText: "zeta",
	}
	rawArgs, _ := json.Marshal(inputs)

	res, err := tool.Execute(&tools.ToolContext{Cwd: tmpDir}, rawArgs)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got unexpected go error %v, want nil", fn, inputs, err)
	}
	if got, want := res.IsError, true; got != want {
		t.Fatalf("%s(inputs=%+v): got IsError=%v, want %v", fn, inputs, got, want)
	}
}

func TestEditTool_DuplicateOldText(t *testing.T) {
	fn := "EditTool.Execute"
	tmpDir := t.TempDir()
	filePath := setupEditTestFile(t, tmpDir, "file.txt", "repeat repeat repeat")

	tool := &EditTool{}
	inputs := editArgs{
		Path:    filePath,
		OldText: "repeat",
		NewText: "once",
	}
	rawArgs, _ := json.Marshal(inputs)

	res, err := tool.Execute(&tools.ToolContext{Cwd: tmpDir}, rawArgs)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got unexpected go error %v, want nil", fn, inputs, err)
	}
	if got, want := res.IsError, true; got != want {
		t.Fatalf("%s(inputs=%+v): got IsError=%v, want %v", fn, inputs, got, want)
	}
}

func TestEditTool_EmptyOldText(t *testing.T) {
	fn := "EditTool.Execute"
	tmpDir := t.TempDir()
	filePath := setupEditTestFile(t, tmpDir, "file.txt", "content")

	tool := &EditTool{}
	inputs := editArgs{
		Path:    filePath,
		OldText: "",
		NewText: "replacement",
	}
	rawArgs, _ := json.Marshal(inputs)

	res, err := tool.Execute(&tools.ToolContext{Cwd: tmpDir}, rawArgs)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got unexpected go error %v, want nil", fn, inputs, err)
	}
	if got, want := res.IsError, true; got != want {
		t.Fatalf("%s(inputs=%+v): got IsError=%v, want %v", fn, inputs, got, want)
	}
}

// Table-driven tests matching EditTool specification table
func TestEditTool_TableDriven(t *testing.T) {
	fn := "EditTool.Execute"
	tmpDir := t.TempDir()

	cases := []struct {
		name        string
		fileContent string
		filename    string
		isMissing   bool
		oldText     string
		newText     string
		wantResult  string
		wantSubstrs []string
	}{
		{
			name:        "valid_single_replace",
			filename:    "single.txt",
			fileContent: "a b c",
			oldText:     "b",
			newText:     "x",
			wantResult:  "a x c",
		},
		{
			name:        "empty_old_text",
			filename:    "empty_old.txt",
			fileContent: "a b c",
			oldText:     "",
			newText:     "x",
			wantSubstrs: []string{"oldText must not be empty"},
		},
		{
			name:        "text_not_found",
			filename:    "not_found.txt",
			fileContent: "a b c",
			oldText:     "d",
			newText:     "x",
			wantSubstrs: []string{"oldText not found in file"},
		},
		{
			name:        "duplicate_matches",
			filename:    "duplicate.txt",
			fileContent: "a b b c",
			oldText:     "b",
			newText:     "x",
			wantSubstrs: []string{"oldText appears 2 times", "must be unique"},
		},
		{
			name:        "file_not_exist",
			filename:    "missing.txt",
			isMissing:   true,
			oldText:     "a",
			newText:     "b",
			wantSubstrs: []string{"no such file or directory"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var filePath string
			if !tc.isMissing {
				filePath = setupEditTestFile(t, tmpDir, tc.filename, tc.fileContent)
			} else {
				filePath = filepath.Join(tmpDir, tc.filename)
			}

			tool := &EditTool{}
			inputs := editArgs{
				Path:    filePath,
				OldText: tc.oldText,
				NewText: tc.newText,
			}
			rawArgs, _ := json.Marshal(inputs)

			res, err := tool.Execute(&tools.ToolContext{Cwd: tmpDir}, rawArgs)
			if err != nil {
				t.Fatalf("%s(case=%s, inputs=%+v): got unexpected go error %v, want nil", fn, tc.name, inputs, err)
			}

			if len(tc.wantSubstrs) > 0 {
				if got, want := res.IsError, true; got != want {
					t.Fatalf("%s(case=%s, inputs=%+v): got IsError=%v, want %v (content=%q)", fn, tc.name, inputs, got, want, res.Content)
				}
				for _, wantSub := range tc.wantSubstrs {
					if !strings.Contains(res.Content, wantSub) {
						t.Fatalf("%s(case=%s, inputs=%+v): got content %q, want substring %q", fn, tc.name, inputs, res.Content, wantSub)
					}
				}
			} else {
				if got, want := res.IsError, false; got != want {
					t.Fatalf("%s(case=%s, inputs=%+v): got IsError=%v, want %v (content=%q)", fn, tc.name, inputs, got, want, res.Content)
				}
				data, readErr := os.ReadFile(filePath)
				if readErr != nil {
					t.Fatalf("%s(case=%s, inputs=%+v): got read error %v, want nil", fn, tc.name, inputs, readErr)
				}
				if got, want := string(data), tc.wantResult; got != want {
					t.Fatalf("%s(case=%s, inputs=%+v): got file content %q, want %q", fn, tc.name, inputs, got, want)
				}
			}
		})
	}
}

// TC-ED-001: Edit-严格唯一匹配就地替换
func TestEditTool_TC_ED_001_StrictSingleReplace(t *testing.T) {
	fn := "EditTool.Execute"
	tmpDir := t.TempDir()
	filePath := setupEditTestFile(t, tmpDir, "f.txt", "hello foo world")

	tool := &EditTool{}
	inputs := editArgs{
		Path:    filePath,
		OldText: "foo",
		NewText: "bar",
	}
	rawArgs, _ := json.Marshal(inputs)

	res, err := tool.Execute(&tools.ToolContext{Cwd: tmpDir}, rawArgs)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got unexpected go error %v, want nil", fn, inputs, err)
	}
	if got, want := res.IsError, false; got != want {
		t.Fatalf("%s(inputs=%+v): got IsError=%v, want %v (content=%q)", fn, inputs, got, want, res.Content)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got read file error %v, want nil", fn, inputs, err)
	}
	expected := "hello bar world"
	if got, want := string(data), expected; got != want {
		t.Fatalf("%s(inputs=%+v): got file content %q, want %q", fn, inputs, got, want)
	}

	// Verify no temporary files remain
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got read dir error %v, want nil", fn, inputs, err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-edit-") {
			t.Errorf("%s(inputs=%+v): found leftover temp file %q, want none", fn, inputs, entry.Name())
		}
	}
}
