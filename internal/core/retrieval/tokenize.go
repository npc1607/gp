package retrieval

import (
	"strings"
	"unicode"
)

// CodeTokenize splits source code, file paths, identifiers, and Chinese prose into searchable terms.
// It handles CamelCase, snake_case, path separators, and CJK unigrams/bigrams.
func CodeTokenize(text string) []string {
	if text == "" {
		return nil
	}
	var out []string
	var asciiBuf strings.Builder
	var cjkRunes []rune

	flushCJK := func() {
		if len(cjkRunes) == 0 {
			return
		}
		for i, r := range cjkRunes {
			out = append(out, string(r))
			if i+1 < len(cjkRunes) {
				out = append(out, string([]rune{r, cjkRunes[i+1]}))
			}
		}
		cjkRunes = cjkRunes[:0]
	}

	flushASCII := func() {
		if asciiBuf.Len() == 0 {
			return
		}
		raw := asciiBuf.String()
		asciiBuf.Reset()
		lower := strings.ToLower(raw)
		if !isStopToken(lower) {
			out = append(out, lower)
		}
		parts := splitCamelAndSnake(raw)
		if len(parts) > 1 {
			for _, p := range parts {
				lp := strings.ToLower(p)
				if len(lp) >= 2 && !isStopToken(lp) {
					out = append(out, lp)
				}
			}
		}
	}

	for _, r := range text {
		switch {
		case r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'):
			flushCJK()
			asciiBuf.WriteRune(r)
		case unicode.Is(unicode.Han, r):
			flushASCII()
			cjkRunes = append(cjkRunes, r)
		default:
			flushASCII()
			flushCJK()
		}
	}
	flushASCII()
	flushCJK()
	return out
}

func splitCamelAndSnake(s string) []string {
	var parts []string
	var cur strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if r == '_' {
			if cur.Len() > 0 {
				parts = append(parts, cur.String())
				cur.Reset()
			}
			continue
		}
		if i > 0 && unicode.IsUpper(r) && cur.Len() > 0 {
			prev := runes[i-1]
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				parts = append(parts, cur.String())
				cur.Reset()
			}
		}
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	return parts
}

func isStopToken(t string) bool {
	switch t {
	case "the", "and", "for", "with", "that", "this", "from", "are", "was", "not", "can", "you", "your",
		"的", "了", "在", "是", "我", "有", "和", "就", "不", "也", "都", "把", "被", "给", "到", "请":
		return true
	}
	return len(t) == 0
}
