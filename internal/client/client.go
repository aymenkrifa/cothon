// Package client finds a running cothon daemon and talks to it as an
// engine.Backend.
package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aymenkrifa/cothon/internal/engine"
)

// Runtime describes a running daemon. It is written to a 0600 file in a 0700
// directory under $XDG_RUNTIME_DIR, so only this user can read the token.
type Runtime struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
	Mode  string `json:"mode"` // serve | web
}

func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "cothon")
	}
	return filepath.Join(os.TempDir(), "cothon-"+strconv.Itoa(os.Getuid()))
}

func runtimePath() string { return filepath.Join(runtimeDir(), "daemon.json") }
func lockPath() string    { return filepath.Join(runtimeDir(), "daemon.lock") }

// ensureRuntimeDir creates the runtime dir 0700 and, since it may live in a
// shared /tmp, refuses one that is a symlink or owned by someone else.
func ensureRuntimeDir() error {
	dir := runtimeDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not a directory owned by you", dir)
	}
	return os.Chmod(dir, 0o700)
}

var ErrDaemonRunning = errors.New("a cothon daemon is already running")

func ownedByMe(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() || fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not owned by you", p)
	}
	return nil
}

// Lock takes the single-daemon lock for the life of the process. It is a
// flock, so it disappears with the process even after a crash (no stale PID
// files, no PID-reuse confusion).
func Lock() (release func(), err error) {
	if err := ensureRuntimeDir(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrDaemonRunning
		}
		return nil, err
	}
	// A daemon.json left by a crashed daemon must not be served to clients
	// while this one starts up.
	os.Remove(runtimePath())
	return func() { f.Close() }, nil
}

// daemonAlive reports whether some process holds the daemon lock.
func daemonAlive() bool {
	f, err := os.Open(lockPath())
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// SessionToken returns the persistent session secret (created on first use,
// 0600). It survives daemon restarts so browser sessions and bookmarks keep
// working; delete the file and restart the daemon to sign every browser out.
func SessionToken() (string, error) {
	dir := stateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "session")
	if tok, ok := readToken(p); ok {
		return tok, nil
	}
	// Missing, empty or damaged: write a fresh one atomically.
	tmp, err := os.CreateTemp(dir, ".session-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	tok := NewToken()
	if _, err := tmp.WriteString(tok + "\n"); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return "", err
	}
	return tok, nil
}

// readToken accepts only a well-formed secret (48 hex characters).
func readToken(p string) (string, bool) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	tok := strings.TrimSpace(string(b))
	if len(tok) != 48 {
		return "", false
	}
	if _, err := hex.DecodeString(tok); err != nil {
		return "", false
	}
	return tok, true
}

func stateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "cothon")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "cothon")
}

// NewToken returns a random secret.
func NewToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// WriteRuntime records the daemon for clients; the returned func removes it.
// Call it only while holding Lock.
func WriteRuntime(rt Runtime) (func(), error) {
	if err := ensureRuntimeDir(); err != nil {
		return nil, err
	}
	b, _ := json.Marshal(rt)
	tmp := runtimePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, runtimePath()); err != nil {
		return nil, err
	}
	return func() { os.Remove(runtimePath()) }, nil
}

// ReadRuntime returns the recorded daemon if one is running (holds the lock).
func ReadRuntime() (*Runtime, error) {
	// The fallback runtime dir lives in a shared /tmp: trust only our own.
	for _, p := range []string{runtimeDir(), runtimePath()} {
		if err := ownedByMe(p); err != nil {
			return nil, err
		}
	}
	if !daemonAlive() {
		return nil, errors.New("no daemon running")
	}
	b, err := os.ReadFile(runtimePath())
	if err != nil {
		return nil, err
	}
	var rt Runtime
	if err := json.Unmarshal(b, &rt); err != nil {
		return nil, err
	}
	return &rt, nil
}

// Discover returns a client for a live daemon, verifying it answers.
func Discover(ctx context.Context) (*Client, error) {
	rt, err := ReadRuntime()
	if err != nil {
		return nil, err
	}
	c := New(*rt)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := c.State(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// Client is a remote engine.Backend.
type Client struct {
	rt   Runtime
	base string
	http *http.Client
}

func New(rt Runtime) *Client {
	return &Client{rt: rt, base: "http://" + rt.Addr, http: &http.Client{}}
}

func (c *Client) Addr() string { return c.rt.Addr }

// LoginURL asks the daemon for a one-time browser login link to path.
func (c *Client) LoginURL(ctx context.Context, path string) (string, error) {
	res, err := c.req(ctx, http.MethodPost, "/api/login-code", nil)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var out struct {
		Code string `json:"code"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&out) != nil || out.Code == "" {
		return "", fmt.Errorf("daemon: %s", res.Status)
	}
	return LoginURL(c.rt.Addr, out.Code, path), nil
}

// LoginURL builds the browser link that exchanges a one-time code for a session.
func LoginURL(addr, code, path string) string {
	if path == "" {
		path = "/"
	}
	return "http://" + addr + "/login?code=" + url.QueryEscape(code) + "&next=" + url.QueryEscape(path)
}

func (c *Client) req(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.rt.Token)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Cothon", "1")
	}
	return c.http.Do(req)
}

func (c *Client) State(ctx context.Context) (*engine.State, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := c.req(ctx, http.MethodGet, "/api/state", nil)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon: %s", res.Status)
	}
	var st engine.State
	return &st, json.NewDecoder(res.Body).Decode(&st)
}

func (c *Client) Do(ctx context.Context, a engine.Action) (string, error) {
	b, _ := json.Marshal(a)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	res, err := c.req(ctx, http.MethodPost, "/api/action", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var out struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("daemon: %s", res.Status)
	}
	if !out.OK {
		return "", errors.New(out.Message)
	}
	return out.Message, nil
}

func (c *Client) Usage(ctx context.Context) (*engine.Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := c.req(ctx, http.MethodGet, "/api/usage", nil)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon: %s", res.Status)
	}
	var u engine.Usage
	return &u, json.NewDecoder(res.Body).Decode(&u)
}

// Watch follows the daemon's event stream. The channel closes when the
// daemon goes away or ctx ends.
func (c *Client) Watch(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{}, 1)
	go func() {
		defer close(ch)
		res, err := c.req(ctx, http.MethodGet, "/api/events", nil)
		if err != nil {
			return
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return
		}
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			line := sc.Text()
			if line == "event: bye" {
				return
			}
			if strings.HasPrefix(line, "data: ") {
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		}
	}()
	return ch
}
