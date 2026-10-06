package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

// WebSearch queries a SearXNG instance via its JSON API.
type WebSearch struct {
	BaseURL    string
	Client     *http.Client
	MaxResults int
}

func (w *WebSearch) Name() string { return "web_search" }

func (w *WebSearch) Description() string {
	return "Search the web for current information, news, facts or documentation. " +
		"Returns a list of results with title, URL and snippet. " +
		"Use web_fetch afterwards to read a promising page in full."
}

func (w *WebSearch) Parameters() []byte {
	return []byte(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "The search query."},
    "max_results": {"type": "integer", "description": "Maximum number of results (default 8)."},
    "time_range": {"type": "string", "enum": ["day", "month", "year"], "description": "Only return results from this period."},
    "language": {"type": "string", "description": "Preferred result language code, e.g. en, th. Optional."}
  },
  "required": ["query"]
}`)
}

type searxResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

func (w *WebSearch) Execute(ctx context.Context, _ CallContext, raw []byte) (string, error) {
	var args struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
		TimeRange  string `json:"time_range"`
		Language   string `json:"language"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		return "", errors.New("query is required")
	}
	limit := w.MaxResults
	if limit <= 0 {
		limit = 8
	}
	if args.MaxResults > 0 && args.MaxResults < limit {
		limit = args.MaxResults
	}

	q := url.Values{}
	q.Set("q", args.Query)
	q.Set("format", "json")
	switch args.TimeRange {
	case "day", "month", "year":
		q.Set("time_range", args.TimeRange)
	}
	if args.Language != "" {
		q.Set("language", args.Language)
	}
	u := strings.TrimRight(w.BaseURL, "/") + "/search?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := w.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusForbidden {
		return "", errors.New("search backend returned 403 (is the json format enabled in SearXNG's search.formats?)")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("search backend returned status %d", resp.StatusCode)
	}
	var sr searxResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return "", fmt.Errorf("search backend returned invalid JSON: %w", err)
	}

	var sb strings.Builder
	seen := map[string]bool{}
	n := 0
	for _, r := range sr.Results {
		if r.URL == "" || seen[r.URL] {
			continue
		}
		seen[r.URL] = true
		n++
		fmt.Fprintf(&sb, "%d. %s\n   URL: %s\n", n, oneLine(r.Title, 200), r.URL)
		if c := oneLine(r.Content, 400); c != "" {
			fmt.Fprintf(&sb, "   %s\n", c)
		}
		if n >= limit {
			break
		}
	}
	if n == 0 {
		return "No results found.", nil
	}
	return fmt.Sprintf("Search results for %q:\n\n%s", args.Query, sb.String()), nil
}

// oneLine collapses whitespace and limits s to max runes.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max]) + "…"
	}
	return s
}
