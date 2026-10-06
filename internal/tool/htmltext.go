package tool

import (
	"io"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// skipTags are elements whose content is never useful page text.
var skipTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "svg": true, "iframe": true,
	"canvas": true, "template": true, "head": true, "nav": true, "footer": true,
	"aside": true, "form": true, "button": true, "select": true, "dialog": true,
}

var blockTags = map[string]bool{
	"p": true, "div": true, "section": true, "article": true, "main": true,
	"header": true, "ul": true, "ol": true, "table": true, "tr": true,
	"blockquote": true, "pre": true, "figure": true, "figcaption": true,
	"dl": true, "dt": true, "dd": true, "details": true, "summary": true,
	"address": true, "body": true,
}

// htmlToText converts an HTML document into readable plain text with light
// Markdown structure (headings, lists, links). base resolves relative links.
func htmlToText(r io.Reader, base *url.URL) (title, text string, err error) {
	doc, err := html.Parse(r)
	if err != nil {
		return "", "", err
	}
	title = strings.Join(strings.Fields(findTitle(doc)), " ")

	body := findElement(doc, "body")
	if body == nil {
		body = doc
	}
	root := findElement(body, "main")
	if root == nil {
		root = findElement(body, "article")
	}
	if root != nil {
		if t := extract(root, base); len([]rune(t)) >= 200 {
			return title, t, nil
		}
	}
	return title, extract(body, base), nil
}

func extract(n *html.Node, base *url.URL) string {
	e := &extractor{base: base}
	e.walk(n)
	return tidy(e.sb.String())
}

func findTitle(n *html.Node) string {
	if t := findElement(n, "title"); t != nil {
		var sb strings.Builder
		for c := t.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.TextNode {
				sb.WriteString(c.Data)
			}
		}
		return sb.String()
	}
	return ""
}

func findElement(n *html.Node, tag string) *html.Node {
	if n.Type == html.ElementNode && n.Data == tag {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := findElement(c, tag); f != nil {
			return f
		}
	}
	return nil
}

type extractor struct {
	sb   strings.Builder
	base *url.URL
	pre  int
}

// newline ensures the output ends with at least n newlines.
func (e *extractor) newline(n int) {
	s := e.sb.String()
	if s == "" {
		return
	}
	have := 0
	for i := len(s) - 1; i >= 0 && s[i] == '\n'; i-- {
		have++
	}
	for ; have < n; have++ {
		e.sb.WriteByte('\n')
	}
}

func (e *extractor) writeText(s string) {
	if e.pre > 0 {
		e.sb.WriteString(s)
		return
	}
	collapsed := strings.Join(strings.Fields(s), " ")
	if collapsed == "" {
		// whitespace-only node: keep a single separating space
		if s != "" && e.sb.Len() > 0 && !endsWithSpace(e.sb.String()) {
			e.sb.WriteByte(' ')
		}
		return
	}
	cur := e.sb.String()
	if len(s) > 0 && isSpace(s[0]) && cur != "" && !endsWithSpace(cur) {
		e.sb.WriteByte(' ')
	}
	e.sb.WriteString(collapsed)
	if isSpace(s[len(s)-1]) {
		e.sb.WriteByte(' ')
	}
}

func isSpace(b byte) bool { return b == ' ' || b == '\n' || b == '\t' || b == '\r' }

func endsWithSpace(s string) bool {
	return s == "" || isSpace(s[len(s)-1])
}

func (e *extractor) walkChildren(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		e.walk(c)
	}
}

func (e *extractor) walk(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		e.writeText(n.Data)
		return
	case html.ElementNode:
		// handled below
	default:
		e.walkChildren(n)
		return
	}
	tag := n.Data
	if skipTags[tag] {
		return
	}
	switch {
	case len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6':
		e.newline(2)
		e.sb.WriteString(strings.Repeat("#", int(tag[1]-'0')) + " ")
		e.walkChildren(n)
		e.newline(2)
	case tag == "br":
		e.sb.WriteByte('\n')
	case tag == "hr":
		e.newline(2)
		e.sb.WriteString("---")
		e.newline(2)
	case tag == "li":
		e.newline(1)
		e.sb.WriteString("- ")
		e.walkChildren(n)
		e.newline(1)
	case tag == "td" || tag == "th":
		e.walkChildren(n)
		e.sb.WriteString(" | ")
	case tag == "pre":
		e.newline(2)
		e.pre++
		e.walkChildren(n)
		e.pre--
		e.newline(2)
	case tag == "a":
		e.writeLink(n)
	case blockTags[tag]:
		e.newline(2)
		e.walkChildren(n)
		e.newline(2)
	default:
		e.walkChildren(n)
	}
}

func (e *extractor) writeLink(n *html.Node) {
	sub := &extractor{base: e.base, pre: e.pre}
	sub.walkChildren(n)
	label := strings.TrimSpace(sub.sb.String())
	href := ""
	for _, a := range n.Attr {
		if a.Key == "href" {
			href = strings.TrimSpace(a.Val)
		}
	}
	if label == "" {
		return
	}
	if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(strings.ToLower(href), "javascript:") ||
		strings.HasPrefix(strings.ToLower(href), "mailto:") {
		e.writeText(label)
		return
	}
	if e.base != nil {
		if ref, err := url.Parse(href); err == nil {
			href = e.base.ResolveReference(ref).String()
		}
	}
	e.sb.WriteString("[" + label + "](" + href + ")")
}

// tidy trims trailing spaces per line and collapses runs of blank lines.
func tidy(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, l := range lines {
		l = strings.TrimRight(l, " \t\r")
		if strings.TrimSpace(l) == "" {
			blank++
			if blank > 1 {
				continue
			}
			out = append(out, "")
			continue
		}
		blank = 0
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
