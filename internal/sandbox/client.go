package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client runs commands on a remote sandbox executor.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client // optional
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) do(ctx context.Context, method, path string, body any, timeout time.Duration, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		var eb errorBody
		_ = json.Unmarshal(data, &eb)
		switch resp.StatusCode {
		case http.StatusTooManyRequests:
			return ErrBusy
		case http.StatusUnauthorized:
			return fmt.Errorf("%w: the sandbox rejected the token", ErrUnavailable)
		}
		if eb.Error == "" {
			eb.Error = fmt.Sprintf("http %d", resp.StatusCode)
		}
		return fmt.Errorf("sandbox: %s", eb.Error)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("sandbox: invalid response: %w", err)
		}
	}
	return nil
}

// Run implements Runner.
func (c *Client) Run(ctx context.Context, req Request) (*Result, error) {
	// The server enforces the real limit; this only bounds the wait.
	wait := 5 * time.Minute
	if req.TimeoutSeconds > 0 {
		wait = time.Duration(req.TimeoutSeconds)*time.Second + 20*time.Second
	}
	var res Result
	if err := c.do(ctx, http.MethodPost, "/run", req, wait, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Reset implements Runner.
func (c *Client) Reset(ctx context.Context, workspace string) error {
	return c.do(ctx, http.MethodDelete, "/workspace/"+url.PathEscape(workspace), nil, 30*time.Second, nil)
}

// Info fetches the sandbox's capabilities.
func (c *Client) Info(ctx context.Context) (*Info, error) {
	var info Info
	if err := c.do(ctx, http.MethodGet, "/info", nil, 10*time.Second, &info); err != nil {
		return nil, err
	}
	return &info, nil
}
