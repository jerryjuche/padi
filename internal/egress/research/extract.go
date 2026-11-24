// Package research implements the read-only research API: bounded fetching,
// redirect re-validation and extraction of readable text from static pages.
package research

import (
	htmlstd "html"
	"io"
	"strings"
)

type stackEntry struct {
	name string
	skip bool
}

// ExtractText returns a title and readable text with HTML markup, comments,
// script/style/template/noscript content and hidden elements removed. The text
// is capped at maxChars characters (runes, not bytes).
func ExtractText(r io.Reader, maxChars int) (title, text string, err error) {
	title, text, _, err = extractText(r, maxChars)
	return title, text, err
}

func extractText(r io.Reader, maxChars int) (title, text string, truncated bool, err error) {
	if r == nil {
		return "", "", false, nil
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return "", "", false, err
	}
	if maxChars < 0 {
		maxChars = 0
	}
	src := string(data)
	var stack []stackEntry
	var titleRunes, bodyRunes []rune
	skipDepth := 0
	titleDepth := 0

	for i := 0; i < len(src); {
		textPart, raw, next := nextChunk(src, i)
		if next == i {
			i++
			continue
		}
		i = next
		if clean := cleanText(textPart); clean != "" {
			if titleDepth == 0 && skipDepth == 0 {
				bodyRunes, truncated = appendLimited(bodyRunes, clean, maxChars)
			} else if titleDepth > 0 && skipDepth == 0 {
				titleRunes, _ = appendLimited(titleRunes, clean, 512)
			}
		}
		if raw == "" {
			continue
		}
		if isComment(raw) || strings.HasPrefix(raw, "<!") || strings.HasPrefix(raw, "<?") {
			continue
		}
		name, closing, selfClosing, attrs := parseTag(raw)
		if name == "" {
			continue
		}
		if closing {
			idx := -1
			for j := len(stack) - 1; j >= 0; j-- {
				if stack[j].name == name {
					idx = j
					break
				}
			}
			if idx >= 0 {
				for j := len(stack) - 1; j >= idx; j-- {
					if stack[j].skip {
						skipDepth--
					}
					if stack[j].name == "title" {
						titleDepth--
					}
				}
				stack = stack[:idx]
			}
			continue
		}
		if isVoidTag(name) || selfClosing {
			continue
		}
		skip := isSkipTag(name) || isHidden(attrs)
		stack = append(stack, stackEntry{name: name, skip: skip})
		if skip {
			skipDepth++
		}
		if name == "title" && !skip {
			titleDepth++
		}
	}
	return strings.TrimSpace(string(titleRunes)), strings.TrimSpace(string(bodyRunes)), truncated, nil
}

func nextChunk(data string, i int) (textPart, raw string, next int) {
	rel := strings.IndexByte(data[i:], '<')
	if rel < 0 {
		return data[i:], "", len(data)
	}
	start := i + rel
	textPart = data[i:start]
	rest := data[start:]
	if strings.HasPrefix(rest, "<!--") {
		if end := strings.Index(rest[4:], "-->"); end >= 0 {
			return textPart, "", start + 4 + end + 3
		}
		return textPart, "", len(data)
	}
	quote := byte(0)
	for j := 1; j < len(rest); j++ {
		c := rest[j]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			continue
		}
		if c == '>' {
			return textPart, rest[:j+1], start + j + 1
		}
	}
	return textPart, rest, len(data)
}

func isComment(raw string) bool { return strings.HasPrefix(raw, "<!--") }

func parseTag(raw string) (name string, closing, selfClosing bool, attrs map[string]string) {
	inner := strings.TrimSpace(raw[1 : len(raw)-1])
	if strings.HasPrefix(inner, "/") {
		closing = true
		inner = strings.TrimSpace(inner[1:])
	}
	if strings.HasSuffix(inner, "/") {
		selfClosing = true
		inner = strings.TrimSpace(strings.TrimSuffix(inner, "/"))
	}
	i := 0
	for i < len(inner) && isNameChar(inner[i]) {
		i++
	}
	name = strings.ToLower(inner[:i])
	return name, closing, selfClosing, parseAttrs(inner[i:])
}

func parseAttrs(s string) map[string]string {
	attrs := map[string]string{}
	for i := 0; i < len(s); {
		if isSpace(s[i]) || s[i] == '/' {
			i++
			continue
		}
		start := i
		for i < len(s) && isNameChar(s[i]) {
			i++
		}
		if start == i {
			i++
			continue
		}
		key := strings.ToLower(s[start:i])
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		value := ""
		if i < len(s) && s[i] == '=' {
			i++
			for i < len(s) && isSpace(s[i]) {
				i++
			}
			if i < len(s) && (s[i] == '"' || s[i] == '\'') {
				q := s[i]
				i++
				vstart := i
				for i < len(s) && s[i] != q {
					i++
				}
				value = s[vstart:i]
				if i < len(s) {
					i++
				}
			} else {
				vstart := i
				for i < len(s) && !isSpace(s[i]) && s[i] != '/' {
					i++
				}
				value = s[vstart:i]
			}
		}
		attrs[key] = value
	}
	return attrs
}

func isNameChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') || c == '-' || c == '_' || c == ':'
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' }

func isSkipTag(name string) bool {
	switch name {
	case "script", "style", "template", "noscript":
		return true
	default:
		return false
	}
}

func isVoidTag(name string) bool {
	switch name {
	case "area", "base", "br", "col", "embed", "hr", "img", "input",
		"link", "meta", "param", "source", "track", "wbr":
		return true
	default:
		return false
	}
}

func isHidden(attrs map[string]string) bool {
	if _, ok := attrs["hidden"]; ok {
		return true
	}
	style := strings.ToLower(strings.ReplaceAll(attrs["style"], " ", ""))
	if strings.Contains(style, "display:none") || strings.Contains(style, "visibility:hidden") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(attrs["aria-hidden"]), "true")
}

func cleanText(s string) string {
	clean := htmlstd.UnescapeString(s)
	fields := strings.Fields(clean)
	if len(fields) == 0 {
		return ""
	}
	return strings.Join(fields, " ")
}

func appendLimited(dst []rune, s string, max int) ([]rune, bool) {
	if max <= 0 {
		return dst, s != ""
	}
	room := max - len(dst)
	if room <= 0 {
		return dst, true
	}
	runes := []rune(s)
	if len(runes) > room {
		return append(dst, runes[:room]...), true
	}
	return append(dst, runes...), false
}
