package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
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
		return c.statusError(resp.StatusCode, data)
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

func fileURL(workspace, path string) string {
	return "/workspace/" + url.PathEscape(workspace) + "/file?path=" + url.QueryEscape(path)
}

// Put implements Files.
func (c *Client) Put(ctx context.Context, workspace, path string, r io.Reader, maxBytes int64) error {
	if maxBytes > 0 {
		r = io.LimitReader(r, maxBytes+1)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return ErrTooLarge
	}
	return c.raw(ctx, http.MethodPut, fileURL(workspace, path), data, 5*time.Minute, nil)
}

// Get implements Files.
func (c *Client) Get(ctx context.Context, workspace, path string, maxBytes int64) ([]byte, error) {
	u := fileURL(workspace, path)
	if maxBytes > 0 {
		u += "&max=" + strconv.FormatInt(maxBytes, 10)
	}
	var out []byte
	if err := c.raw(ctx, http.MethodGet, u, nil, 5*time.Minute, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Remove implements Files.
func (c *Client) Remove(ctx context.Context, workspace, path string) error {
	return c.raw(ctx, http.MethodDelete, fileURL(workspace, path), nil, 30*time.Second, nil)
}

// raw is like do, but with a binary request body and, when out is set, the
// raw response body.
func (c *Client) raw(ctx context.Context, method, path string, body []byte, timeout time.Duration, out *[]byte) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := c.client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return c.statusError(resp.StatusCode, data)
	}
	if out != nil {
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<30))
		if err != nil {
			return err
		}
		*out = data
	}
	return nil
}

// statusError turns an error response into one of the package's errors.
func (c *Client) statusError(status int, body []byte) error {
	var eb errorBody
	_ = json.Unmarshal(body, &eb)
	switch status {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusRequestEntityTooLarge:
		return ErrTooLarge
	case http.StatusInsufficientStorage:
		return ErrQuota
	case http.StatusTooManyRequests:
		return ErrBusy
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: the sandbox rejected the token", ErrUnavailable)
	case http.StatusBadRequest:
		if strings.Contains(eb.Error, "invalid file path") {
			return ErrBadPath
		}
	}
	if eb.Error == "" {
		eb.Error = fmt.Sprintf("http %d", status)
	}
	return fmt.Errorf("sandbox: %s", eb.Error)
}
