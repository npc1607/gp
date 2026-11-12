package builtin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gp/internal/tools"
)

func setupWriteTestDir(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	t.Cleanup(func() {
		_ = os.RemoveAll(tmpDir)
	})
	return tmpDir
}

func TestWriteTool_Basic(t *testing.T) {
	fn := "WriteTool.Execute"
	tmpDir := setupWriteTestDir(t)
	tool := &WriteTool{}
	if got, want := tool.Name(), "write"; got != want {
		t.Errorf("WriteTool.Name(): got %q, want %q", got, want)
	}

	targetFile := filepath.Join(tmpDir, "sub", "dir", "output.txt")
	content := "hello world from write tool!"

	inputs := writeArgs{
		Path:    targetFile,
		Content: content,
	}
	rawArgs, _ := json.Marshal(inputs)

	res, err := tool.Execute(&tools.ToolContext{Cwd: tmpDir}, rawArgs)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got unexpected error %v, want nil", fn, inputs, err)
	}
	if got, want := res.IsError, false; got != want {
		t.Fatalf("%s(inputs=%+v): got IsError=%v, want %v (content=%q)", fn, inputs, got, want, res.Content)
	}

	// Verify file was written
	data, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got read file error %v, want nil", fn, inputs, err)
	}
	if got, want := string(data), content; got != want {
		t.Fatalf("%s(inputs=%+v): got file content %q, want %q", fn, inputs, got, want)
	}

	// Verify no temporary files left in dir
	entries, err := os.ReadDir(filepath.Dir(targetFile))
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got read dir error %v, want nil", fn, inputs, err)
	}
	if got, want := len(entries), 1; got != want {
		t.Fatalf("%s(inputs=%+v): got %d entries in directory, want %d", fn, inputs, got, want)
	}
}

func TestWriteTool_EmptyPath(t *testing.T) {
	fn := "WriteTool.Execute"
	tool := &WriteTool{}
	inputs := writeArgs{
		Path:    "",
		Content: "foo",
	}
	rawArgs, _ := json.Marshal(inputs)
	res, err := tool.Execute(&tools.ToolContext{}, rawArgs)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got unexpected go error %v, want nil", fn, inputs, err)
	}
	if got, want := res.IsError, true; got != want {
		t.Fatalf("%s(inputs=%+v): got IsError=%v, want %v", fn, inputs, got, want)
	}
	if wantSub := "path is required"; !strings.Contains(res.Content, wantSub) {
		t.Fatalf("%s(inputs=%+v): got content %q, want substring %q", fn, inputs, res.Content, wantSub)
	}
}

// Table-driven tests for WriteTool
func TestWriteTool_TableDriven(t *testing.T) {
	fn := "WriteTool.Execute"
	tmpDir := setupWriteTestDir(t)

	cases := []struct {
		name       string
		pathRel    string
		content    string
		wantError  bool
		wantErrSub string
	}{
		{
			name:       "write_new_file",
			pathRel:    "nested/folder/new_file.txt",
			content:    "brand new file content",
			wantError:  false,
			wantErrSub: "",
		},
		{
			name:       "write_empty_content",
			pathRel:    "empty.txt",
			content:    "",
			wantError:  false,
			wantErrSub: "",
		},
		{
			name:       "empty_path",
			pathRel:    "",
			content:    "data",
			wantError:  true,
			wantErrSub: "path is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			targetPath := ""
			if tc.pathRel != "" {
				targetPath = filepath.Join(tmpDir, tc.pathRel)
			}
			tool := &WriteTool{}
			inputs := writeArgs{
				Path:    targetPath,
				Content: tc.content,
			}
			rawArgs, _ := json.Marshal(inputs)

			res, err := tool.Execute(&tools.ToolContext{Cwd: tmpDir}, rawArgs)
			if err != nil {
				t.Fatalf("%s(case=%s, inputs=%+v): got unexpected go error %v, want nil", fn, tc.name, inputs, err)
			}

			if got, want := res.IsError, tc.wantError; got != want {
				t.Fatalf("%s(case=%s, inputs=%+v): got IsError=%v, want %v (content=%q)", fn, tc.name, inputs, got, want, res.Content)
			}

			if tc.wantErrSub != "" {
				if !strings.Contains(res.Content, tc.wantErrSub) {
					t.Fatalf("%s(case=%s, inputs=%+v): got content %q, want substring %q", fn, tc.name, inputs, res.Content, tc.wantErrSub)
				}
			} else {
				data, readErr := os.ReadFile(targetPath)
				if readErr != nil {
					t.Fatalf("%s(case=%s, inputs=%+v): got read error %v, want nil", fn, tc.name, inputs, readErr)
				}
				if got, want := string(data), tc.content; got != want {
					t.Fatalf("%s(case=%s, inputs=%+v): got content %q, want %q", fn, tc.name, inputs, got, want)
				}
			}
		})
	}
}

// TC-WR-001: Write-多级父目录创建与同目录原子替换
func TestWriteTool_TC_WR_001_MultiLevelAtomicWrite(t *testing.T) {
	fn := "WriteTool.Execute"
	tmpDir := setupWriteTestDir(t)

	targetPath := filepath.Join(tmpDir, "d1", "d2", "f.txt")
	content := "hello"

	// Verify target parent directory does NOT exist yet
	parentDir := filepath.Dir(targetPath)
	if _, statErr := os.Stat(parentDir); !os.IsNotExist(statErr) {
		t.Fatalf("%s(path=%s): parent directory already exists, want not exist", fn, targetPath)
	}

	tool := &WriteTool{}
	inputs := writeArgs{
		Path:    targetPath,
		Content: content,
	}
	rawArgs, _ := json.Marshal(inputs)

	res, err := tool.Execute(&tools.ToolContext{Cwd: tmpDir}, rawArgs)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got unexpected error %v, want nil", fn, inputs, err)
	}
	if got, want := res.IsError, false; got != want {
		t.Fatalf("%s(inputs=%+v): got IsError=%v, want %v (content=%q)", fn, inputs, got, want, res.Content)
	}

	// Verify parent directory was created
	dirInfo, err := os.Stat(parentDir)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got stat error %v, want directory to exist", fn, inputs, err)
	}
	if !dirInfo.IsDir() {
		t.Fatalf("%s(inputs=%+v): got isDir=false, want true", fn, inputs)
	}

	// Verify file content matches
	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got read file error %v, want nil", fn, inputs, err)
	}
	if got, want := string(data), content; got != want {
		t.Fatalf("%s(inputs=%+v): got file content %q, want %q", fn, inputs, got, want)
	}

	// Verify no temporary files remain in d1/d2/
	entries, err := os.ReadDir(parentDir)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got read dir error %v, want nil", fn, inputs, err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-write-") {
			t.Errorf("%s(inputs=%+v): found leftover temp file %q, want none", fn, inputs, entry.Name())
		}
	}
	if got, want := len(entries), 1; got != want {
		t.Errorf("%s(inputs=%+v): got %d entries in directory, want %d", fn, inputs, got, want)
	}
}
