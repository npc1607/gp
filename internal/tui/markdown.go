package tui

import (
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	mdBoldRe       = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	mdInlineCodeRe = regexp.MustCompile("`([^`]+)`")
)

// RenderMarkdownWithCodeBlocks 将 Markdown 文本渲染为终端行，支持 pi-tidy 风格的分割线代码块 (├─ lang ──┤) 与 hex 色块
func RenderMarkdownWithCodeBlocks(text string, width int, t Theme, cfg UIConfig, frameColor lipgloss.Color, insideFrame bool) []string {
	if width < 8 {
		width = 8
	}
	rawLines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var out []string

	inFence := false
	fenceLang := ""
	var fenceBuf []string

	flushFence := func() {
		if len(fenceBuf) == 0 && fenceLang == "" {
			return
		}
		lang := fenceLang
		if lang == "" {
			lang = "code"
		}
		if cfg.CodeBlocks {
			chip := CopyChip(cfg.Icons)
			openLabel := t.Fg(t.Muted, lang) + " " + t.Fg(t.Accent, chip)
			openRule := LabeledRule(width, t, t.BorderMuted, openLabel, "left", 1)
			closeRule := t.Fg(t.BorderMuted, strings.Repeat("─", width))
			out = append(out, openRule)

			if lang == "diff" && cfg.SplitDiff && width >= cfg.SplitDiffMinWidth {
				var oldLines, newLines []string
				for _, fl := range fenceBuf {
					switch {
					case strings.HasPrefix(fl, "---"), strings.HasPrefix(fl, "+++"), strings.HasPrefix(fl, "@@"), strings.HasPrefix(fl, "diff "):
						continue
					case strings.HasPrefix(fl, "-"):
						oldLines = append(oldLines, strings.TrimPrefix(fl, "-"))
					case strings.HasPrefix(fl, "+"):
						newLines = append(newLines, strings.TrimPrefix(fl, "+"))
					default:
						clean := strings.TrimPrefix(fl, " ")
						oldLines = append(oldLines, clean)
						newLines = append(newLines, clean)
					}
				}
				out = append(out, RenderSplitDiff(strings.Join(oldLines, "\n"), strings.Join(newLines, "\n"), width, 0, true, cfg.SplitDiffMinWidth, t)...)
			} else {
				for _, fl := range fenceBuf {
					expanded := strings.ReplaceAll(fl, "\t", "    ")
					if lang == "bash" || lang == "sh" || lang == "zsh" {
						out = append(out, " "+ClipToWidth(HighlightBashCommand(expanded, t), width-1, "…"))
					} else {
						out = append(out, t.Fg(t.Text, " "+ClipToWidth(expanded, width-1, "…")))
					}
				}
			}
			out = append(out, closeRule)
		} else {
			out = append(out, t.Fg(t.Muted, "```"+fenceLang))
			for _, fl := range fenceBuf {
				out = append(out, t.Fg(t.Text, ClipToWidth(fl, width, "…")))
			}
			out = append(out, t.Fg(t.Muted, "```"))
		}
		fenceLang = ""
		fenceBuf = nil
	}

	for _, raw := range rawLines {
		trimmed := strings.TrimSpace(raw)
		if after, ok := strings.CutPrefix(trimmed, "```"); ok {
			if !inFence {
				inFence = true
				fenceLang = strings.TrimSpace(after)
				if idx := strings.IndexByte(fenceLang, ' '); idx >= 0 {
					fenceLang = fenceLang[:idx]
				}
				fenceBuf = nil
			} else {
				inFence = false
				flushFence()
			}
			continue
		}
		if inFence {
			fenceBuf = append(fenceBuf, raw)
			continue
		}

		// 标题
		if strings.HasPrefix(trimmed, "#") {
			level := 0
			for level < len(trimmed) && trimmed[level] == '#' {
				level++
			}
			if level > 0 && level <= 6 && (len(trimmed) == level || trimmed[level] == ' ') {
				headingText := strings.TrimSpace(trimmed[level:])
				styled := t.BoldFg(t.Accent, headingText)
				out = append(out, wrapStyledLine(styled, width)...)
				continue
			}
		}

		// 水平分割线
		if trimmed == "---" || trimmed == "***" || trimmed == "___" {
			out = append(out, t.Fg(t.BorderMuted, strings.Repeat("─", width)))
			continue
		}

		// 引用块
		if after, ok := strings.CutPrefix(trimmed, ">"); ok {
			quoteBody := strings.TrimSpace(after)
			styled := t.Fg(t.BorderMuted, "▎ ") + t.ItalicFg(t.Muted, renderInlineMarkdown(quoteBody, t, cfg))
			out = append(out, wrapStyledLine(styled, width)...)
			continue
		}

		// 列表项
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			indent := len(raw) - len(strings.TrimLeft(raw, " "))
			lead := strings.Repeat(" ", indent) + t.Fg(t.Accent, "• ")
			body := renderInlineMarkdown(trimmed[2:], t, cfg)
			out = append(out, wrapStyledLine(lead+body, width)...)
			continue
		}

		if trimmed == "" {
			out = append(out, "")
			continue
		}

		rendered := renderInlineMarkdown(raw, t, cfg)
		out = append(out, wrapStyledLine(rendered, width)...)
	}

	if inFence {
		flushFence()
	}
	if len(out) == 0 {
		return []string{""}
	}
	_ = frameColor
	_ = insideFrame
	return out
}

func renderInlineMarkdown(line string, t Theme, cfg UIConfig) string {
	// 先保护 inline code 不被 hexSwatches 改写
	var codeSpans []string
	protected := mdInlineCodeRe.ReplaceAllStringFunc(line, func(m string) string {
		inner := m[1 : len(m)-1]
		idx := len(codeSpans)
		codeSpans = append(codeSpans, t.Fg(t.SyntaxKeyword, "`"+inner+"`"))
		return "\x00CODE" + string(rune('A'+idx%26)) + string(rune('0'+idx/26)) + "\x00"
	})

	if cfg.HexSwatches {
		protected = RenderHexSwatches(protected)
	}
	protected = mdBoldRe.ReplaceAllStringFunc(protected, func(m string) string {
		return t.BoldFg(t.Text, m[2:len(m)-2])
	})

	// 还原 inline code
	for idx, span := range codeSpans {
		token := "\x00CODE" + string(rune('A'+idx%26)) + string(rune('0'+idx/26)) + "\x00"
		protected = strings.Replace(protected, token, span, 1)
	}
	return protected
}

func wrapStyledLine(line string, width int) []string {
	if width <= 1 || VisibleWidth(line) <= width {
		return []string{line}
	}
	// 使用 lipgloss.Width 感知的简单软换行
	wrapped := lipgloss.NewStyle().Width(width).Render(line)
	return strings.Split(wrapped, "\n")
}
