package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/html/charset"
)

// WebFetch downloads a web page and returns its readable text.
type WebFetch struct {
	Client    *http.Client // use NewSafeClient
	MaxBytes  int64        // download limit
	MaxChars  int          // default/maximum characters returned per call
	UserAgent string
}

func (w *WebFetch) Name() string { return "web_fetch" }

func (w *WebFetch) Description() string {
	return "Download a web page (http/https) and return its main text content as Markdown-like text. " +
		"Use it to read pages found with web_search. Long pages are paged: pass offset to continue."
}

func (w *WebFetch) Parameters() []byte {
	return []byte(`{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "Absolute http(s) URL to fetch."},
    "offset": {"type": "integer", "description": "Character offset to start from, for continuing a long page. Default 0."},
    "max_chars": {"type": "integer", "description": "Maximum characters to return. Optional."}
  },
  "required": ["url"]
}`)
}

func (w *WebFetch) Execute(ctx context.Context, _ CallContext, raw []byte) (string, error) {
	var args struct {
		URL      string `json:"url"`
		Offset   int    `json:"offset"`
		MaxChars int    `json:"max_chars"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	u, err := url.Parse(strings.TrimSpace(args.URL))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("url must be an absolute http or https URL")
	}
	maxChars := w.MaxChars
	if maxChars <= 0 {
		maxChars = 12000
	}
	if args.MaxChars > 0 && args.MaxChars < maxChars {
		maxChars = args.MaxChars
	}
	if args.Offset < 0 {
		args.Offset = 0
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	ua := w.UserAgent
	if ua == "" {
		ua = "Mozilla/5.0 (compatible; jannyq; +https://github.com/authapon/jannyq)"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")
	resp, err := w.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("server returned HTTP %d", resp.StatusCode)
	}

	ctype := resp.Header.Get("Content-Type")
	media, _, _ := mime.ParseMediaType(ctype)
	if media == "" {
		media = "text/html" // sniffed below; most pages without a type are HTML
	}
	isHTML := media == "text/html" || media == "application/xhtml+xml"
	if !isHTML && !isTextual(media) {
		return "", fmt.Errorf("unsupported content type %q (only text and HTML pages can be read)", media)
	}

	limit := w.MaxBytes
	if limit <= 0 {
		limit = 2 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}
	r, err := charset.NewReader(strings.NewReader(string(body)), ctype)
	if err != nil {
		return "", fmt.Errorf("decoding response: %w", err)
	}

	var title, text string
	if isHTML {
		title, text, err = htmlToText(r, resp.Request.URL)
		if err != nil {
			return "", fmt.Errorf("parsing HTML: %w", err)
		}
	} else {
		b, err := io.ReadAll(r)
		if err != nil {
			return "", fmt.Errorf("decoding response: %w", err)
		}
		text = strings.TrimSpace(string(b))
	}

	runes := []rune(text)
	total := len(runes)
	if args.Offset >= total && total > 0 {
		return "", fmt.Errorf("offset %d is beyond the end of the page (%d characters)", args.Offset, total)
	}
	end := args.Offset + maxChars
	if end > total {
		end = total
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "URL: %s\n", resp.Request.URL)
	if title != "" {
		fmt.Fprintf(&sb, "Title: %s\n", title)
	}
	sb.WriteString("\n")
	if total == 0 {
		sb.WriteString("(the page has no readable text content)")
		return sb.String(), nil
	}
	sb.WriteString(string(runes[args.Offset:end]))
	if end < total {
		fmt.Fprintf(&sb, "\n\n[Showing characters %d-%d of %d. Call web_fetch again with offset=%d to continue.]",
			args.Offset, end, total, end)
	}
	return sb.String(), nil
}

func isTextual(media string) bool {
	if strings.HasPrefix(media, "text/") {
		return true
	}
	switch media {
	case "application/json", "application/xml", "application/rss+xml", "application/atom+xml",
		"application/javascript", "application/x-yaml", "application/yaml", "application/ld+json":
		return true
	}
	return strings.HasSuffix(media, "+json") || strings.HasSuffix(media, "+xml")
}
