package builtin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gp/internal/tools"
)

func setupReadTestFile(t *testing.T, dir, filename, content string) string {
	t.Helper()
	filePath := filepath.Join(dir, filename)
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("setupReadTestFile(filename=%s): got error %v, want nil", filename, err)
	}
	t.Cleanup(func() {
		_ = os.Remove(filePath)
	})
	return filePath
}

func TestReadTool_Basic(t *testing.T) {
	fn := "ReadTool.Execute"
	tmpDir := t.TempDir()
	content := "line 1\nline 2\nline 3\n"
	filePath := setupReadTestFile(t, tmpDir, "test.txt", content)

	tool := &ReadTool{}
	if got, want := tool.Name(), "read"; got != want {
		t.Errorf("ReadTool.Name(): got %q, want %q", got, want)
	}

	inputs := readArgs{Path: filePath}
	rawArgs, _ := json.Marshal(inputs)
	res, err := tool.Execute(&tools.ToolContext{Cwd: tmpDir}, rawArgs)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got unexpected error %v, want nil", fn, inputs, err)
	}
	if got, want := res.IsError, false; got != want {
		t.Fatalf("%s(inputs=%+v): got IsError=%v, want %v (content=%q)", fn, inputs, got, want, res.Content)
	}

	expected := "     1 | line 1\n     2 | line 2\n     3 | line 3\n"
	if got, want := res.Content, expected; got != want {
		t.Errorf("%s(inputs=%+v): got content %q, want %q", fn, inputs, got, want)
	}
}

func TestReadTool_OffsetLimit(t *testing.T) {
	fn := "ReadTool.Execute"
	tmpDir := t.TempDir()
	content := "line 1\nline 2\nline 3\nline 4\nline 5\n"
	filePath := setupReadTestFile(t, tmpDir, "test.txt", content)

	tool := &ReadTool{}
	inputs := readArgs{
		Path:   filePath,
		Offset: 2,
		Limit:  2,
	}
	rawArgs, _ := json.Marshal(inputs)
	res, err := tool.Execute(&tools.ToolContext{Cwd: tmpDir}, rawArgs)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got unexpected error %v, want nil", fn, inputs, err)
	}
	if got, want := res.IsError, false; got != want {
		t.Fatalf("%s(inputs=%+v): got IsError=%v, want %v (content=%q)", fn, inputs, got, want, res.Content)
	}

	if !strings.Contains(res.Content, "2 | line 2") || !strings.Contains(res.Content, "3 | line 3") {
		t.Fatalf("%s(inputs=%+v): got content %q, want lines 2 and 3", fn, inputs, res.Content)
	}
	if strings.Contains(res.Content, "1 | line 1") || strings.Contains(res.Content, "4 | line 4") {
		t.Fatalf("%s(inputs=%+v): got content %q, want excluding lines 1 and 4", fn, inputs, res.Content)
	}
	if wantNotice := "[Showing lines 2-3 of 5. Use offset=4 to continue.]"; !strings.Contains(res.Content, wantNotice) {
		t.Fatalf("%s(inputs=%+v): got content %q, want notice containing %q", fn, inputs, res.Content, wantNotice)
	}
}

func TestReadTool_FileNotFound(t *testing.T) {
	fn := "ReadTool.Execute"
	tmpDir := t.TempDir()
	tool := &ReadTool{}
	inputs := readArgs{Path: "nonexistent.txt"}
	rawArgs, _ := json.Marshal(inputs)
	res, err := tool.Execute(&tools.ToolContext{Cwd: tmpDir}, rawArgs)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got unexpected go error %v, want nil", fn, inputs, err)
	}
	if got, want := res.IsError, true; got != want {
		t.Fatalf("%s(inputs=%+v): got IsError=%v, want %v", fn, inputs, got, want)
	}
	if wantSubstr := "no such file or directory"; !strings.Contains(res.Content, wantSubstr) {
		t.Fatalf("%s(inputs=%+v): got content %q, want substring %q", fn, inputs, res.Content, wantSubstr)
	}
}

func TestReadTool_OffsetOutOfRange(t *testing.T) {
	fn := "ReadTool.Execute"
	tmpDir := t.TempDir()
	filePath := setupReadTestFile(t, tmpDir, "short.txt", "only one line\n")

	tool := &ReadTool{}
	inputs := readArgs{Path: filePath, Offset: 10}
	rawArgs, _ := json.Marshal(inputs)
	res, err := tool.Execute(&tools.ToolContext{Cwd: tmpDir}, rawArgs)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got unexpected go error %v, want nil", fn, inputs, err)
	}
	if got, want := res.IsError, true; got != want {
		t.Fatalf("%s(inputs=%+v): got IsError=%v, want %v", fn, inputs, got, want)
	}
	if !strings.Contains(res.Content, "offset 10") {
		t.Fatalf("%s(inputs=%+v): got content %q, want substring containing 'offset 10'", fn, inputs, res.Content)
	}
}

func TestReadTool_Truncation(t *testing.T) {
	fn := "ReadTool.Execute"
	tmpDir := t.TempDir()
	var sb strings.Builder
	for i := 1; i <= 2500; i++ {
		sb.WriteString("some repetitive line content here\n")
	}
	filePath := setupReadTestFile(t, tmpDir, "large.txt", sb.String())

	tool := &ReadTool{}
	inputs := readArgs{Path: filePath}
	rawArgs, _ := json.Marshal(inputs)
	res, err := tool.Execute(&tools.ToolContext{Cwd: tmpDir}, rawArgs)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got unexpected error %v, want nil", fn, inputs, err)
	}
	if got, want := res.IsError, false; got != want {
		t.Fatalf("%s(inputs=%+v): got IsError=%v, want %v", fn, inputs, got, want)
	}
	if wantNotice := "[Showing lines"; !strings.Contains(res.Content, wantNotice) {
		t.Fatalf("%s(inputs=%+v): got content %q, want notice containing %q", fn, inputs, res.Content, wantNotice)
	}
}

// Table-driven tests matching ReadTool specification table
func TestReadTool_TableDriven(t *testing.T) {
	fn := "ReadTool.Execute"
	tmpDir := t.TempDir()

	fiveLinesContent := "line 1\nline 2\nline 3\nline 4\nline 5\n"
	fiveLinesPath := setupReadTestFile(t, tmpDir, "five_lines.txt", fiveLinesContent)

	cases := []struct {
		name       string
		path       string
		offset     int
		limit      int
		wantError  bool
		wantSubstr []string
	}{
		{
			name:       "normal_pagination",
			path:       fiveLinesPath,
			offset:     2,
			limit:      2,
			wantError:  false,
			wantSubstr: []string{"2 | line 2", "3 | line 3"},
		},
		{
			name:       "offset_zero_clamped",
			path:       fiveLinesPath,
			offset:     0,
			limit:      2,
			wantError:  false,
			wantSubstr: []string{"1 | line 1", "2 | line 2"},
		},
		{
			name:       "offset_out_of_bound",
			path:       fiveLinesPath,
			offset:     10,
			limit:      2,
			wantError:  true,
			wantSubstr: []string{"offset 10"},
		},
		{
			name:       "read_file_not_found",
			path:       filepath.Join(tmpDir, "non_existent.txt"),
			offset:     1,
			limit:      10,
			wantError:  true,
			wantSubstr: []string{"no such file or directory"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := &ReadTool{}
			inputs := readArgs{
				Path:   tc.path,
				Offset: tc.offset,
				Limit:  tc.limit,
			}
			rawArgs, _ := json.Marshal(inputs)
			res, err := tool.Execute(&tools.ToolContext{Cwd: tmpDir}, rawArgs)
			if err != nil {
				t.Fatalf("%s(case=%s, inputs=%+v): got unexpected go error %v, want nil", fn, tc.name, inputs, err)
			}

			if got, want := res.IsError, tc.wantError; got != want {
				t.Fatalf("%s(case=%s, inputs=%+v): got IsError=%v, want %v (content=%q)", fn, tc.name, inputs, got, want, res.Content)
			}

			for _, wantSub := range tc.wantSubstr {
				if !strings.Contains(res.Content, wantSub) {
					t.Fatalf("%s(case=%s, inputs=%+v): got content %q, want substring %q", fn, tc.name, inputs, res.Content, wantSub)
				}
			}
		})
	}
}

// TC-RD-001: Read-分页读取与行号格式化
func TestReadTool_TC_RD_001_PaginationAndFormatting(t *testing.T) {
	fn := "ReadTool.Execute"
	tmpDir := t.TempDir()

	var sb strings.Builder
	for i := 1; i <= 10; i++ {
		sb.WriteString(fmt.Sprintf("content line %d\n", i))
	}
	filePath := setupReadTestFile(t, tmpDir, "test.txt", sb.String())

	tool := &ReadTool{}
	inputs := readArgs{
		Path:   filePath,
		Offset: 2,
		Limit:  3,
	}
	rawArgs, _ := json.Marshal(inputs)

	res, err := tool.Execute(&tools.ToolContext{Cwd: tmpDir}, rawArgs)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got unexpected error %v, want nil", fn, inputs, err)
	}
	if got, want := res.IsError, false; got != want {
		t.Fatalf("%s(inputs=%+v): got IsError=%v, want %v", fn, inputs, got, want)
	}

	for lineNum := 2; lineNum <= 4; lineNum++ {
		wantLine := fmt.Sprintf("%6d | content line %d", lineNum, lineNum)
		if !strings.Contains(res.Content, wantLine) {
			t.Errorf("%s(inputs=%+v): got content %q, want line %q", fn, inputs, res.Content, wantLine)
		}
	}

	if strings.Contains(res.Content, "     1 | ") {
		t.Errorf("%s(inputs=%+v): got content containing line 1, want excluded", fn, inputs)
	}
	if strings.Contains(res.Content, "     5 | ") {
		t.Errorf("%s(inputs=%+v): got content containing line 5, want excluded", fn, inputs)
	}
}

// TC-RD-002: Read-超限截断与续读提示
func TestReadTool_TC_RD_002_TruncationAndContinueNotice(t *testing.T) {
	fn := "ReadTool.Execute"
	tmpDir := t.TempDir()

	var sb strings.Builder
	for i := 1; i <= 3000; i++ {
		sb.WriteString(fmt.Sprintf("Line number %04d: testing overflow truncation\n", i))
	}
	filePath := setupReadTestFile(t, tmpDir, "large.txt", sb.String())

	tool := &ReadTool{}
	inputs := readArgs{
		Path:   filePath,
		Offset: 1,
		Limit:  3000,
	}
	rawArgs, _ := json.Marshal(inputs)

	res, err := tool.Execute(&tools.ToolContext{Cwd: tmpDir}, rawArgs)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got unexpected error %v, want nil", fn, inputs, err)
	}
	if got, want := res.IsError, false; got != want {
		t.Fatalf("%s(inputs=%+v): got IsError=%v, want %v", fn, inputs, got, want)
	}

	lines := strings.Split(strings.TrimSpace(res.Content), "\n")
	contentLineCount := 0
	for _, l := range lines {
		if strings.Contains(l, "|") {
			contentLineCount++
		}
	}

	if contentLineCount > 2000 {
		t.Errorf("%s(inputs=%+v): got %d content lines, want <= 2000", fn, inputs, contentLineCount)
	}

	wantNoticePrefix := "[Showing lines 1-"
	if !strings.Contains(res.Content, wantNoticePrefix) || !strings.Contains(res.Content, "Use offset=") {
		t.Errorf("%s(inputs=%+v): got content %q, want truncation notice", fn, inputs, res.Content)
	}
}
