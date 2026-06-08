package tgui

import (
	"html"
	"regexp"
	"strconv"
	"strings"
)

// Markdown-subset matchers. Code is pulled out first (so its contents are never
// treated as markdown), then the prose is HTML-escaped and the inline/block
// markers are applied on top.
var (
	reFence      = regexp.MustCompile("(?s)```[a-zA-Z0-9_+-]*\\n?(.*?)```")
	reInlineCode = regexp.MustCompile("`([^`\n]+)`")
	reBoldStar   = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	reBoldUnder  = regexp.MustCompile(`__([^_\n]+)__`)
	reHeading    = regexp.MustCompile(`(?m)^[ \t]*#{1,6}[ \t]+(.+?)[ \t]*$`)
	reLink       = regexp.MustCompile(`\[([^\]\n]+)\]\(([^)\s]+)\)`)
	reBullet     = regexp.MustCompile(`(?m)^([ \t]*)[-*+][ \t]+`)
)

// MarkdownToHTML converts the subset of GitHub-flavored Markdown the assistant
// commonly emits into Telegram-safe HTML (for parse_mode=HTML). Telegram's HTML
// supports only a small tag set (b/i/code/pre/a/s/u/blockquote) with no headings
// or lists, so ATX headings (#..######) render as bold and bullet markers as
// "• "; fenced/inline code, **bold**/__bold__, and [links](url) map to their
// tags. Everything else is left as HTML-escaped literal text.
//
// It is deliberately conservative and best-effort: callers MUST send the result
// with a plain-text fallback (resend the ORIGINAL text without a parse mode) on a
// Telegram parse error, so a pathological input degrades to raw text rather than
// a failed delivery. That fallback is why this favours simple, predictable rules
// over perfectly handling every nested/unbalanced case.
func MarkdownToHTML(md string) string {
	var frags []string
	stash := func(h string) string {
		frags = append(frags, h)
		return "\x00" + strconv.Itoa(len(frags)-1) + "\x00"
	}

	// 1. Protect code (fenced, then inline). The body is HTML-escaped now and the
	//    whole span replaced by a NUL-delimited placeholder that survives the
	//    escape + formatting passes below untouched.
	md = reFence.ReplaceAllStringFunc(md, func(m string) string {
		body := reFence.FindStringSubmatch(m)[1]
		return stash("<pre>" + html.EscapeString(strings.Trim(body, "\n")) + "</pre>")
	})
	md = reInlineCode.ReplaceAllStringFunc(md, func(m string) string {
		body := reInlineCode.FindStringSubmatch(m)[1]
		return stash("<code>" + html.EscapeString(body) + "</code>")
	})

	// 2. Escape the remaining prose, then layer inline/block formatting on top.
	//    Order matters: bold before bullets (so a `**` line start isn't read as a
	//    bullet), and links last (their URL is already &-escaped from here).
	md = html.EscapeString(md)
	md = reBoldStar.ReplaceAllString(md, "<b>$1</b>")
	md = reBoldUnder.ReplaceAllString(md, "<b>$1</b>")
	md = reHeading.ReplaceAllString(md, "<b>$1</b>")
	md = reBullet.ReplaceAllString(md, "$1• ")
	md = reLink.ReplaceAllStringFunc(md, func(m string) string {
		sub := reLink.FindStringSubmatch(m)
		return `<a href="` + sub[2] + `">` + sub[1] + `</a>`
	})

	// 3. Restore the protected code fragments.
	for i, f := range frags {
		md = strings.Replace(md, "\x00"+strconv.Itoa(i)+"\x00", f, 1)
	}
	return md
}
