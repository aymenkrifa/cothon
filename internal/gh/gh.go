// Package gh talks to GitHub: mutations go through the gh CLI (argv only, no
// shell); polling uses the REST API directly with ETag caching so unchanged
// responses come back as 304 and don't count against the rate limit.
package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	idRE   = regexp.MustCompile(`^[0-9]{1,20}$`)
)

// ValidRepo reports whether s is a plain owner/name.
func ValidRepo(s string) bool { return repoRE.MatchString(s) && !strings.Contains(s, "..") }

// ValidID reports whether s is a numeric GitHub id.
func ValidID(s string) bool { return idRE.MatchString(s) }

// CLI runs the gh binary.
type CLI struct{ Path string }

// Token returns the gh auth token (never logged or exposed by cothon).
func (c CLI) Token(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "auth", "token")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Post calls `gh api -X POST <endpoint>`.
func (c CLI) Post(ctx context.Context, endpoint string) error {
	_, err := c.run(ctx, "api", "-X", "POST", endpoint)
	return err
}

func (c CLI) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(firstLine(msg))
	}
	return stdout.String(), nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// HTTPError is a non-2xx/304 API response.
type HTTPError struct {
	Status     int
	Message    string
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// Client is a minimal GitHub REST client with an ETag cache.
type Client struct {
	BaseURL string // default https://api.github.com
	Token   func(context.Context) (string, error)
	HTTP    *http.Client

	mu        sync.Mutex
	token     string
	cache     map[string]cached
	Remaining int       // last X-RateLimit-Remaining seen, -1 if unknown
	Reset     time.Time // when the rate-limit window resets
}

// RateLimited reports whether err is GitHub refusing because of rate limits,
// and until when to back off.
func (c *Client) RateLimited(err error) (bool, time.Time) {
	var he *HTTPError
	if !errors.As(err, &he) {
		return false, time.Time{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	secondary := he.Status == http.StatusForbidden &&
		(he.RetryAfter > 0 || strings.Contains(strings.ToLower(he.Message), "rate limit"))
	if he.Status == http.StatusTooManyRequests || secondary || (he.Status == http.StatusForbidden && c.Remaining == 0) {
		until := c.Reset
		if he.RetryAfter > 0 {
			until = time.Now().Add(he.RetryAfter)
		}
		if until.Before(time.Now()) {
			until = time.Now().Add(time.Minute)
		}
		return true, until
	}
	return false, time.Time{}
}

type cached struct {
	etag string
	body []byte
}

func NewClient(token func(context.Context) (string, error)) *Client {
	return &Client{
		BaseURL:   "https://api.github.com",
		Token:     token,
		HTTP:      &http.Client{Timeout: 20 * time.Second},
		cache:     map[string]cached{},
		Remaining: -1,
	}
}

// Get fetches path (e.g. "/orgs/x/repos?per_page=30") into v.
func (c *Client) Get(ctx context.Context, path string, v any) error {
	body, err := c.get(ctx, path, true)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

func (c *Client) get(ctx context.Context, path string, retryAuth bool) ([]byte, error) {
	c.mu.Lock()
	if c.token == "" {
		c.mu.Unlock()
		tok, err := c.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("gh auth token: %w", err)
		}
		c.mu.Lock()
		c.token = tok
	}
	tok := c.token
	prev, hasPrev := c.cache[path]
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "cothon")
	if hasPrev {
		req.Header.Set("If-None-Match", prev.etag)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if r, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining")); err == nil {
		c.mu.Lock()
		c.Remaining = r
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			c.Reset = time.Unix(reset, 0)
		}
		c.mu.Unlock()
	}

	switch {
	case resp.StatusCode == http.StatusNotModified && hasPrev:
		return prev.body, nil
	case resp.StatusCode == http.StatusUnauthorized && retryAuth:
		c.mu.Lock()
		c.token = "" // token rotated (gh auth refresh); fetch it again once
		c.mu.Unlock()
		return c.get(ctx, path, false)
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if etag := resp.Header.Get("ETag"); etag != "" {
			c.mu.Lock()
			c.cache[path] = cached{etag: etag, body: body}
			c.mu.Unlock()
		}
		return body, nil
	default:
		var e struct {
			Message string `json:"message"`
		}
		json.Unmarshal(body, &e)
		he := &HTTPError{Status: resp.StatusCode, Message: firstLine(e.Message)}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			he.RetryAfter = time.Duration(s) * time.Second
		}
		return nil, he
	}
}

// Forget drops cached responses that keep rejects, bounding the cache to
// what is still being polled.
func (c *Client) Forget(keep func(path string) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for p := range c.cache {
		if !keep(p) {
			delete(c.cache, p)
		}
	}
}
