// Package cmapi is a minimal client for the MongoDB Cloud Manager and Ops
// Manager Public API. Both products expose the same Automation endpoints;
// only the base URL differs. The client authenticates with a programmatic
// API key (public/private key pair) using HTTP Digest authentication.
package cmapi

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

// Client talks to the Cloud Manager / Ops Manager Public API v1.0.
type Client struct {
	BaseURL    string // e.g. https://cloud.mongodb.com
	ProjectID  string
	PublicKey  string
	PrivateKey string
	HTTPClient *http.Client

	// MaxRetries controls how many times a request is retried after a 429
	// or 5xx response. Defaults to 4 when zero.
	MaxRetries int
}

// New returns a Client with sane defaults.
func New(baseURL, projectID, publicKey, privateKey string) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		ProjectID:  projectID,
		PublicKey:  publicKey,
		PrivateKey: privateKey,
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
		MaxRetries: 4,
	}
}

func (c *Client) url(path string) string {
	return fmt.Sprintf("%s/api/public/v1.0/groups/%s%s", c.BaseURL, c.ProjectID, path)
}

// do performs an authenticated request with Digest auth (RFC 7616), manual
// retry on a 401 challenge, and retry-with-backoff on 429/5xx.
func (c *Client) do(ctx context.Context, method, url string, body []byte) ([]byte, int, error) {
	maxRetries := c.MaxRetries
	if maxRetries == 0 {
		maxRetries = 4
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		respBody, status, err := c.doOnce(ctx, method, url, body)
		if err == nil && status < 500 && status != 429 {
			return respBody, status, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("status %d: %s", status, truncate(respBody, 500))
		}
		if attempt == maxRetries {
			break
		}
		select {
		case <-time.After(backoff(attempt)):
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
	return nil, 0, lastErr
}

func backoff(attempt int) time.Duration {
	d := time.Duration(1<<attempt) * 500 * time.Millisecond
	if d > 15*time.Second {
		d = 15 * time.Second
	}
	return d
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// doOnce performs a single Digest-authenticated request, including the
// initial 401 challenge round-trip that Digest auth requires.
func (c *Client) doOnce(ctx context.Context, method, url string, body []byte) ([]byte, int, error) {
	// First request without Authorization to obtain the WWW-Authenticate challenge.
	req, err := http.NewRequestWithContext(ctx, method, url, bytesReader(body))
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request %s %s: %w", method, url, err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return b, resp.StatusCode, err
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	resp.Body.Close()
	auth, err := buildDigestHeader(challenge, c.PublicKey, c.PrivateKey, method, mustPath(url), 1)
	if err != nil {
		return nil, 0, fmt.Errorf("digest auth: %w", err)
	}
	req2, err := http.NewRequestWithContext(ctx, method, url, bytesReader(body))
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req2.Header.Set("Content-Type", "application/json")
	}
	req2.Header.Set("Authorization", auth)
	resp2, err := c.HTTPClient.Do(req2)
	if err != nil {
		return nil, 0, fmt.Errorf("authenticated request %s %s: %w", method, url, err)
	}
	defer resp2.Body.Close()
	b, err := io.ReadAll(resp2.Body)
	return b, resp2.StatusCode, err
}

func bytesReader(b []byte) io.Reader {
	if b == nil {
		return nil
	}
	return bytes.NewReader(b)
}

// get fetches a JSON document and unmarshals it into out.
func (c *Client) get(ctx context.Context, path string, out interface{}) error {
	b, status, err := c.do(ctx, http.MethodGet, c.url(path), nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("GET %s: unexpected status %d: %s", path, status, truncate(b, 500))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// put sends a JSON document and unmarshals the response into out (if non-nil).
func (c *Client) put(ctx context.Context, path string, in, out interface{}) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	b, status, err := c.do(ctx, http.MethodPut, c.url(path), body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("PUT %s: unexpected status %d: %s", path, status, truncate(b, 500))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}
