package extensions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// truncateUTF8Bytes 按最大字节数安全截断字符串，保证绝不在 UTF-8 多字节字符（如中文）中间切开
func truncateUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	n := maxBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// ExtractJSONPath 对 JSON 字符串执行轻量字段提取与数组切片（如 "data.logs[:20]" 或 "items[0].name"）。
// 若输入非合法 JSON 或路径不匹配，原样返回输入文本以免丢数据。
func ExtractJSONPath(raw string, expr string) string {
	expr = strings.TrimSpace(expr)
	if expr == "" || strings.TrimSpace(raw) == "" {
		return raw
	}
	var cur any
	if err := json.Unmarshal([]byte(raw), &cur); err != nil {
		return raw
	}
	segments := strings.Split(expr, ".")
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		key := seg
		bracketExpr := ""
		if idx := strings.Index(seg, "["); idx >= 0 && strings.HasSuffix(seg, "]") {
			key = seg[:idx]
			bracketExpr = seg[idx+1 : len(seg)-1]
		}
		if key != "" {
			m, ok := cur.(map[string]any)
			if !ok {
				return raw
			}
			v, exists := m[key]
			if !exists {
				return raw
			}
			cur = v
		}
		if bracketExpr != "" {
			arr, ok := cur.([]any)
			if !ok {
				return raw
			}
			sliced, ok := applyArrayBracket(arr, bracketExpr)
			if !ok {
				return raw
			}
			cur = sliced
		}
	}
	out, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return raw
	}
	return string(out)
}

func applyArrayBracket(arr []any, expr string) (any, bool) {
	if strings.Contains(expr, ":") {
		parts := strings.SplitN(expr, ":", 2)
		start := 0
		end := len(arr)
		if strings.TrimSpace(parts[0]) != "" {
			v, err := strconv.Atoi(strings.TrimSpace(parts[0]))
			if err != nil {
				return nil, false
			}
			start = v
		}
		if strings.TrimSpace(parts[1]) != "" {
			v, err := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err != nil {
				return nil, false
			}
			end = v
		}
		if start < 0 {
			start = max(len(arr)+start, 0)
		}
		if end < 0 {
			end = max(len(arr)+end, 0)
		}
		if start > len(arr) {
			start = len(arr)
		}
		if end > len(arr) {
			end = len(arr)
		}
		if start > end {
			return []any{}, true
		}
		return arr[start:end], true
	}
	idx, err := strconv.Atoi(strings.TrimSpace(expr))
	if err != nil {
		return nil, false
	}
	if idx < 0 {
		idx = len(arr) + idx
	}
	if idx < 0 || idx >= len(arr) {
		return nil, false
	}
	return arr[idx], true
}

// ApplyOutputBudget 按字节与行数预算裁剪工具输出；超限时将脱敏后的完整原文落盘并附带 read 提示。
func ApplyOutputBudget(
	content string,
	maxBytes int,
	maxLines int,
	spillDir string,
	extName string,
	actionName string,
) (trimmed string, truncated bool, spillPath string) {
	if maxBytes <= 0 {
		maxBytes = 16384
	}
	if maxLines <= 0 {
		maxLines = 200
	}

	lines := strings.Split(content, "\n")
	if len(content) <= maxBytes && len(lines) <= maxLines {
		return content, false, ""
	}

	// 将完整原文写入落盘文件，便于模型按需使用 read 分段查看
	if spillDir == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			spillDir = filepath.Join(home, ".gp", "tmp", "ext-outputs")
		} else {
			spillDir = filepath.Join(os.TempDir(), "gp-ext-outputs")
		}
	}
	if err := os.MkdirAll(spillDir, 0o755); err == nil {
		safeName := fmt.Sprintf("%s_%s_%d.out", sanitizeFilename(extName), sanitizeFilename(actionName), time.Now().UnixNano())
		candidate := filepath.Join(spillDir, safeName)
		if wErr := os.WriteFile(candidate, []byte(content), 0o600); wErr == nil {
			spillPath = candidate
		}
	}

	// 按行数与字节双上限裁剪前段摘要（保证 UTF-8 字符边界与换行完整）
	var sb strings.Builder
	keptLines := 0
	for _, line := range lines {
		if keptLines >= maxLines {
			break
		}
		sepLen := 0
		if keptLines > 0 {
			sepLen = 1
		}
		if sb.Len()+sepLen+len(line) > maxBytes {
			remain := maxBytes - sb.Len() - sepLen
			if remain > 16 {
				if keptLines > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(truncateUTF8Bytes(line, remain))
				keptLines++
			}
			break
		}
		if keptLines > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(line)
		keptLines++
	}

	footer := fmt.Sprintf("\n\n... [output truncated: showed %d/%d lines, %d/%d bytes]", keptLines, len(lines), sb.Len(), len(content))
	if spillPath != "" {
		footer += fmt.Sprintf("\nFull output saved to: %s (use `read` with offset/limit to inspect specific ranges)", spillPath)
	}
	return sb.String() + footer, true, spillPath
}

func sanitizeFilename(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	if sb.Len() == 0 {
		return "ext"
	}
	return sb.String()
}
