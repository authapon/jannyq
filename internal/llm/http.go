package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// retryBase is the initial backoff between retries (a var so tests can shrink it).
var retryBase = time.Second

const maxRetries = 2

// APIError is a non-2xx response from a provider.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("llm: http %d: %s", e.Status, e.Body)
}

func (e *APIError) retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// postJSON sends body as JSON and decodes the response into out, retrying on
// network errors, 429 and 5xx responses.
func postJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryBase << (attempt - 1)):
			}
		}
		lastErr = doPost(ctx, client, url, headers, payload, out)
		if lastErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ae *APIError
		if asAPIError(lastErr, &ae) && !ae.retryable() {
			return lastErr
		}
	}
	return lastErr
}

func asAPIError(err error, target **APIError) bool {
	ae, ok := err.(*APIError)
	if ok {
		*target = ae
	}
	return ok
}

func doPost(ctx context.Context, client *http.Client, url string, headers map[string]string, payload []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Status: resp.StatusCode, Body: truncateRunes(strings.TrimSpace(string(data)), 500)}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("llm: decode response: %w", err)
	}
	return nil
}
