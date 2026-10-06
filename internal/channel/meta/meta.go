// Package meta is the small Graph API client shared by the Messenger and
// WhatsApp channels, and the webhook plumbing they have in common.
package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/authapon/jannyq/internal/channel"
)

// DefaultGraphAPI is the Graph API base URL used unless configured otherwise.
const DefaultGraphAPI = "https://graph.facebook.com/v21.0"

// Client calls the Graph API with an access token (sent as a bearer header,
// never in the URL, so it cannot end up in logs or error messages).
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// Error is an error answer of the Graph API.
type Error struct {
	Status  int
	Code    int
	Subcode int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("graph api: HTTP %d, code %d: %s", e.Status, e.Code, e.Message)
}

func (c *Client) base() string { return strings.TrimRight(c.Base, "/") }

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// Do performs a request and decodes a JSON answer into out (if not nil). A
// server error (5xx) is tried once more.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
		var status int
		status, err = c.once(ctx, method, path, payload, out)
		if err == nil || status < 500 {
			return err
		}
	}
	return err
}

func (c *Client) once(ctx context.Context, method, path string, payload []byte, out any) (int, error) {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
				Code    int    `json:"code"`
				Subcode int    `json:"error_subcode"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return resp.StatusCode, &Error{Status: resp.StatusCode, Code: e.Error.Code, Subcode: e.Error.Subcode, Message: e.Error.Message}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("graph api: unreadable answer: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// Download fetches a file of at most max bytes. The token is sent only when
// withToken is set, for URLs that came from the Graph API itself (WhatsApp
// media); the links of Messenger attachments are public and must not receive it.
func (c *Client) Download(ctx context.Context, url string, max int64, withToken bool) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	if withToken {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("downloading a file failed (HTTP %d)", resp.StatusCode)
	}
	if resp.ContentLength > max {
		return nil, "", channel.ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > max {
		return nil, "", channel.ErrTooLarge
	}
	return data, resp.Header.Get("Content-Type"), nil
}
