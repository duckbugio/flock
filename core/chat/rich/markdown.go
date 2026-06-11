package rich

import (
	"regexp"
	"strings"
)

// Block-shape matchers. Kept intentionally close to MarkdownToHTML's regex set so
// FromMarkdown classifies the same Markdown subset; the table/ordered-list
// matchers are the unambiguous additions the rich IR can represent.
var (
	reHeading = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*$`)
	reULItem  = regexp.MustCompile(`^[-*+]\s+(.*)$`)
	reOLItem  = regexp.MustCompile(`^\d+[.)]\s+(.*)$`)
)

// tableHeaderRows is how many rows precede a pipe table's body: the header row
// plus the separator row.
const tableHeaderRows = 2

// FromMarkdown parses the Markdown subset the assistant emits into the neutral
// rich IR. It is TOTAL and best-effort: it never panics, and any line it cannot
// classify becomes a Paragraph, so a serializer always has something valid to
// emit (mirroring MarkdownToHTML's "degrade to escaped literal" guarantee). For
// any input containing a non-blank line it returns at least one Block; an
// empty/whitespace-only input returns a Message with no blocks.
func FromMarkdown(md string) Message {
	lines := strings.Split(md, "\n")
	var blocks []Block
	for i := 0; i < len(lines); {
		s := lines[i]
		switch {
		case blankLine(s):
			i++ // blank line: a block separator, nothing emitted
		case fenceLine(s):
			var blk Block
			blk, i = parseFence(lines, i)
			blocks = append(blocks, blk)
		case headingLine(s):
			blocks = append(blocks, parseHeading(s))
			i++
		case tableStart(lines, i):
			var blk Block
			blk, i = parseTable(lines, i)
			blocks = append(blocks, blk)
		case quoteLine(s):
			var blk Block
			blk, i = parseQuote(lines, i)
			blocks = append(blocks, blk)
		case listLine(s):
			var blk Block
			blk, i = parseList(lines, i)
			blocks = append(blocks, blk)
		default:
			var blk Block
			blk, i = parseParagraph(lines, i)
			blocks = append(blocks, blk)
		}
	}
	return Message{Blocks: blocks}
}

// --- line predicates ---

func blankLine(s string) bool   { return strings.TrimSpace(s) == "" }
func fenceLine(s string) bool   { return strings.HasPrefix(strings.TrimSpace(s), "```") }
func headingLine(s string) bool { return reHeading.MatchString(strings.TrimSpace(s)) }
func quoteLine(s string) bool   { return strings.HasPrefix(strings.TrimSpace(s), ">") }

func listLine(s string) bool {
	t := strings.TrimSpace(s)
	return reULItem.MatchString(t) || reOLItem.MatchString(t)
}

// tableStart reports whether line i begins a pipe table: a row containing "|"
// immediately followed by a separator row (cells of only -, :, and spaces). The
// mandatory separator is what keeps a prose line that merely contains "|" from
// being misread as a table.
func tableStart(lines []string, i int) bool {
	if i+1 >= len(lines) {
		return false
	}
	return strings.Contains(lines[i], "|") && isTableSeparator(lines[i+1])
}

// isBlockStart reports whether line i begins a non-paragraph block (or is blank),
// so parseParagraph knows where to stop without requiring a blank-line separator.
func isBlockStart(lines []string, i int) bool {
	s := lines[i]
	return blankLine(s) || fenceLine(s) || headingLine(s) ||
		quoteLine(s) || listLine(s) || tableStart(lines, i)
}

// --- block parsers (each returns the parsed block and the next line index) ---

// parseFence consumes a ``` fenced code block starting at line i, capturing the
// optional info-string as Lang and the verbatim body. An unterminated fence runs
// to EOF (best-effort), so a stray ``` never drops the rest of the message.
func parseFence(lines []string, i int) (Block, int) {
	lang := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), "```"))
	var body []string
	j := i + 1
	for j < len(lines) {
		if strings.TrimSpace(lines[j]) == "```" {
			j++ // consume the closing fence
			break
		}
		body = append(body, lines[j])
		j++
	}
	return Code{Lang: lang, Text: strings.Join(body, "\n")}, j
}

func parseHeading(s string) Block {
	m := reHeading.FindStringSubmatch(strings.TrimSpace(s))
	return Heading{Level: len(m[1]), Text: parseInline(m[2])}
}

// parseQuote consumes consecutive ">"-prefixed lines, strips the marker (and one
// optional following space) from each, and joins them into one inline run.
func parseQuote(lines []string, i int) (Block, int) {
	var parts []string
	j := i
	for j < len(lines) && quoteLine(lines[j]) {
		t := strings.TrimSpace(lines[j])
		t = strings.TrimPrefix(t, ">")
		t = strings.TrimPrefix(t, " ")
		parts = append(parts, t)
		j++
	}
	return Quote{Text: parseInline(strings.Join(parts, " "))}, j
}

// parseList consumes consecutive list items. Ordered-ness is taken from the first
// item; each item's text after its marker becomes one inline run.
func parseList(lines []string, i int) (Block, int) {
	ordered := reOLItem.MatchString(strings.TrimSpace(lines[i]))
	var items []Inline
	j := i
	for j < len(lines) && listLine(lines[j]) {
		t := strings.TrimSpace(lines[j])
		var text string
		if m := reOLItem.FindStringSubmatch(t); m != nil {
			text = m[1]
		} else if m := reULItem.FindStringSubmatch(t); m != nil {
			text = m[1]
		}
		items = append(items, parseInline(text))
		j++
	}
	return List{Ordered: ordered, Items: items}, j
}

// parseParagraph consumes consecutive lines until a blank line or the start of
// another block, joining them into one inline run.
func parseParagraph(lines []string, i int) (Block, int) {
	var parts []string
	j := i
	for j < len(lines) {
		if j > i && isBlockStart(lines, j) {
			break
		}
		parts = append(parts, strings.TrimSpace(lines[j]))
		j++
	}
	return Paragraph{Text: parseInline(strings.Join(parts, " "))}, j
}

// parseTable consumes a pipe table: the header row at i, the separator at i+1,
// then body rows until a non-row/blank line.
func parseTable(lines []string, i int) (Block, int) {
	header := splitRow(lines[i])
	var rows [][]string
	j := i + tableHeaderRows // skip header + separator
	for j < len(lines) && strings.Contains(lines[j], "|") && !blankLine(lines[j]) {
		rows = append(rows, splitRow(lines[j]))
		j++
	}
	return Table{Header: header, Rows: rows}, j
}

// isTableSeparator reports whether a line is a pipe-table separator: every
// pipe-delimited cell is non-empty and contains only '-', ':' and spaces.
func isTableSeparator(line string) bool {
	cells := splitRow(line)
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		if c == "" {
			return false
		}
		for _, r := range c {
			if r != '-' && r != ':' && r != ' ' {
				return false
			}
		}
	}
	return true
}

// splitRow splits a pipe-table row into trimmed cells, dropping the optional
// leading/trailing outer pipes.
func splitRow(line string) []string {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	parts := strings.Split(s, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// --- inline parser ---

// parseInline splits one logical run of text into styled spans. It recognizes the
// same inline subset as MarkdownToHTML: inline code (`…`), links ([text](url)),
// and bold (**…** or __…__). Matching is non-nested and left-to-right; an
// unterminated marker is emitted as literal text, so the function is total and
// never panics.
func parseInline(s string) Inline {
	var spans []Span
	var plain strings.Builder
	flush := func() {
		if plain.Len() > 0 {
			spans = append(spans, Span{Text: plain.String()})
			plain.Reset()
		}
	}
	for i := 0; i < len(s); {
		if sp, n, ok := matchInline(s, i); ok {
			flush()
			spans = append(spans, sp)
			i += n
			continue
		}
		_ = plain.WriteByte(s[i]) // strings.Builder.WriteByte never returns an error
		i++
	}
	flush()
	return Inline{Spans: spans}
}

// matchInline tries to match one inline construct at position i, returning the
// span, the number of bytes consumed, and whether a match was found. Order is by
// priority: code first (its body is verbatim), then links, then bold.
func matchInline(s string, i int) (Span, int, bool) {
	switch {
	case s[i] == '`':
		if end := strings.IndexByte(s[i+1:], '`'); end >= 0 {
			consumed := 1 + end + 1 // opening backtick + body + closing backtick
			return Span{Text: s[i+1 : i+1+end], Code: true}, consumed, true
		}
	case s[i] == '[':
		if sp, n, ok := matchLink(s, i); ok {
			return sp, n, true
		}
	case strings.HasPrefix(s[i:], "**"):
		if body, n, ok := matchDelim(s, i, "**"); ok {
			return Span{Text: body, Bold: true}, n, true
		}
	case strings.HasPrefix(s[i:], "__"):
		if body, n, ok := matchDelim(s, i, "__"); ok {
			return Span{Text: body, Bold: true}, n, true
		}
	}
	return Span{}, 0, false
}

// matchDelim matches a paired delimiter d (e.g. "**") at position i, returning the
// non-empty body between the pair and the total bytes consumed.
func matchDelim(s string, i int, d string) (string, int, bool) {
	start := i + len(d)
	if start > len(s) {
		return "", 0, false
	}
	idx := strings.Index(s[start:], d)
	if idx <= 0 { // <=0 also rejects an empty body (****)
		return "", 0, false
	}
	return s[start : start+idx], len(d) + idx + len(d), true
}

// matchLink matches a [text](url) link at position i (s[i] == '['). text may not
// contain a ']' and url may not contain whitespace or ')', matching the
// MarkdownToHTML link regex.
func matchLink(s string, i int) (Span, int, bool) {
	closeBracket := strings.IndexByte(s[i:], ']')
	if closeBracket < 0 || i+closeBracket+1 >= len(s) || s[i+closeBracket+1] != '(' {
		return Span{}, 0, false
	}
	text := s[i+1 : i+closeBracket]
	rest := s[i+closeBracket+2:] // after "]("
	closeParen := strings.IndexByte(rest, ')')
	if closeParen < 0 {
		return Span{}, 0, false
	}
	url := rest[:closeParen]
	if url == "" || strings.ContainsAny(url, " \t\n") {
		return Span{}, 0, false
	}
	consumed := closeBracket + 1 + 1 + closeParen + 1 // [text] + ( + url + )
	return Span{Text: text, URL: url}, consumed, true
}
