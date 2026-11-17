// Package server exposes an engine.Backend over HTTP on loopback: a small
// server-rendered web UI, a JSON API and a server-sent-events stream.
//
// Security model (the API can rerun/cancel CI and stop the runner, and other
// local processes — including CI jobs running on this very runner — can reach
// loopback):
//   - listen on loopback only, and reject any Host header but our own
//     address (DNS rebinding);
//   - every request except static files and the login exchange needs the
//     session secret: as a bearer header (TUI, scripts; the secret is in a
//     0600 file) or as a SameSite=Strict HttpOnly cookie;
//   - the cookie is only issued by GET /login in exchange for a one-time,
//     short-lived code that cothon itself puts in the URL it opens;
//   - POSTs additionally need a custom header (forces a CORS preflight we
//     never approve) and, when the browser sends them, a same-origin
//     Origin / Sec-Fetch-Site.
package server

import (
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aymenkrifa/cothon/internal/engine"
	"github.com/aymenkrifa/cothon/internal/gh"
)

//go:embed web
var webFS embed.FS

const (
	viewCookie    = "cothon_view" // UI preference only, set by the page script
	actionHeader  = "X-Cothon"
	loginCodeTTL  = time.Minute
	sessionMaxAge = 30 * 24 * time.Hour
	maxStreams    = 32
)

// viewMode picks the jobs view: the path (/grouped, /list), then ?view=
// (fragment requests), then the remembered preference for "/".
func viewMode(r *http.Request) string {
	if seg, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/"); seg == "grouped" || seg == "list" {
		return seg
	}
	if v := r.URL.Query().Get("view"); v == "grouped" || v == "list" {
		return v
	}
	if c, err := r.Cookie(viewCookie); err == nil && (c.Value == "grouped" || c.Value == "list") {
		return c.Value
	}
	return "grouped"
}

// urlFilter is the filter state carried by the page URL.
type urlFilter struct {
	Repo   string // owner/name from the path
	Status string // failed | passed | canceled | running
	Q      string
}

func filterFromRequest(r *http.Request) urlFilter {
	var f urlFilter
	if owner, name := r.PathValue("owner"), r.PathValue("name"); owner != "" && name != "" {
		if repo := owner + "/" + name; gh.ValidRepo(repo) {
			f.Repo = repo
		}
	}
	switch st := r.URL.Query().Get("status"); st {
	case "failed", "passed", "canceled", "running":
		f.Status = st
	}
	if q := strings.TrimSpace(r.URL.Query().Get("q")); len(q) <= 200 {
		f.Q = q
	}
	return f
}

type Server struct {
	B     engine.Backend
	Token string
	// Port is the listening port; Host headers must name loopback + this port.
	Port string
	Log  *slog.Logger

	tmpl *template.Template

	mu         sync.Mutex
	clients    int
	everServed bool
	lastLeft   time.Time
	codes      map[string]time.Time // one-time login codes -> expiry
	graceUntil time.Time            // latest code expiry: don't exit idle before it
}

func New(b engine.Backend, token, listenAddr string) (*Server, error) {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return nil, err
	}
	t, err := template.New("").Funcs(funcs).ParseFS(webFS, "web/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{B: b, Token: token, Port: port, Log: slog.Default(), tmpl: t,
		lastLeft: time.Now(), codes: map[string]time.Time{}}, nil
}

// cookieName is per port: cookies are shared across ports of one host, and
// two cothon instances must not overwrite each other's session.
func (s *Server) cookieName() string { return "cothon_session_" + s.Port }

// CheckLoopback refuses to listen on anything but loopback.
func CheckLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("refusing to listen on %q: loopback addresses only", addr)
	}
	return nil
}

func (s *Server) Handler() http.Handler {
	static, _ := fs.Sub(webFS, "web/static")
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticFiles(http.FileServerFS(static))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /login", s.login)
	// Pages: the view and repo live in the path, other filters in the query:
	//   /grouped/my-org/my-repo?status=failed&q=backend
	mux.HandleFunc("GET /{$}", s.page)
	for _, v := range []string{"grouped", "list"} {
		mux.HandleFunc("GET /"+v, s.page)
		mux.HandleFunc("GET /"+v+"/{owner}/{name}", s.page)
	}
	mux.Handle("GET /fragment", s.auth(http.HandlerFunc(s.fragment)))
	mux.Handle("GET /api/state", s.auth(http.HandlerFunc(s.apiState)))
	mux.Handle("GET /api/usage", s.auth(http.HandlerFunc(s.apiUsage)))
	mux.Handle("GET /api/events", s.auth(http.HandlerFunc(s.events)))
	mux.Handle("POST /api/action", s.auth(s.sameOrigin(http.HandlerFunc(s.action))))
	mux.Handle("POST /api/login-code", s.bearerOnly(s.sameOrigin(http.HandlerFunc(s.loginCode))))
	return s.checkHost(securityHeaders(gzipped(mux)))
}

func (s *Server) checkHost(next http.Handler) http.Handler {
	allowed := map[string]bool{
		"127.0.0.1:" + s.Port: true,
		"localhost:" + s.Port: true,
		"[::1]:" + s.Port:     true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[strings.ToLower(r.Host)] {
			http.Error(w, "forbidden host", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		next.ServeHTTP(w, r)
	})
}

// staticFiles serves embedded assets; no directory listings.
func staticFiles(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// gzipped compresses successful GET responses for clients that accept it (the
// jobs fragment shrinks ~25x). Whether to compress is decided when the status
// is known, so errors, HEAD, ranges and the event stream pass through as-is.
func gzipped(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if r.URL.Path == "/api/events" || r.Method != http.MethodGet || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		r.Header.Del("Range") // byte ranges of a compressed body would be wrong
		g := &gzipWriter{ResponseWriter: w}
		defer g.close()
		next.ServeHTTP(g, r)
	})
}

// acceptsGzip parses Accept-Encoding enough to honor "gzip;q=0".
func acceptsGzip(h string) bool {
	for _, part := range strings.Split(h, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.TrimSpace(name) != "gzip" {
			continue
		}
		q := strings.ReplaceAll(strings.TrimSpace(params), " ", "")
		return q != "q=0" && q != "q=0.0" && q != "q=0.00" && q != "q=0.000"
	}
	return false
}

type gzipWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	started bool
}

func (g *gzipWriter) WriteHeader(code int) {
	if !g.started {
		g.started = true
		if code == http.StatusOK && g.Header().Get("Content-Encoding") == "" {
			g.Header().Set("Content-Encoding", "gzip")
			g.Header().Del("Content-Length")
			g.Header().Del("Accept-Ranges")
			g.gz = gzip.NewWriter(g.ResponseWriter)
		}
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if !g.started {
		g.WriteHeader(http.StatusOK)
	}
	if g.gz != nil {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *gzipWriter) close() {
	if g.gz != nil {
		g.gz.Close()
	}
}

// browserSecret is what the browser cookie holds: derived from the session
// secret, so a cookie (or a redeemed login link) never yields the bearer
// token that the CLI uses to mint login links.
func (s *Server) browserSecret() string {
	m := hmac.New(sha256.New, []byte(s.Token))
	m.Write([]byte("cothon browser session v1"))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Server) tokenOK(r *http.Request) bool {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, "Bearer ")), []byte(s.Token)) == 1
	}
	if c, err := r.Cookie(s.cookieName()); err == nil && c.Value != "" {
		return subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.browserSecret())) == 1
	}
	return false
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.tokenOK(r) {
			http.Error(w, "unauthorized: open cothon with `cothon web`", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerOnly accepts only the secret from the runtime file, not the cookie:
// minting login codes is for the CLI.
func (s *Server) bearerOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, "Bearer ")), []byte(s.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(actionHeader) != "1" {
			http.Error(w, "missing "+actionHeader+" header", http.StatusForbidden)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			http.Error(w, "cross-site request", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
			http.Error(w, "bad origin", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// NewLoginCode mints a one-time code for GET /login (valid two minutes).
func (s *Server) NewLoginCode() string {
	code := randomHex(16)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for c, exp := range s.codes {
		if now.After(exp) {
			delete(s.codes, c)
		}
	}
	s.codes[code] = now.Add(loginCodeTTL)
	if exp := now.Add(loginCodeTTL); exp.After(s.graceUntil) {
		s.graceUntil = exp
	}
	return code
}

func (s *Server) loginCode(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"code": s.NewLoginCode()})
}

// login exchanges a one-time code for the session cookie, then redirects to
// the requested page (same-site paths only).
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	s.mu.Lock()
	exp, ok := s.codes[code]
	delete(s.codes, code)
	s.mu.Unlock()
	if code == "" || !ok || time.Now().After(exp) {
		s.loginPage(w, http.StatusUnauthorized, "This login link has expired or was already used.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: s.browserSecret(), Path: "/",
		MaxAge: int(sessionMaxAge.Seconds()), HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
}

// safeNext keeps redirects on this site: a local absolute path, no scheme,
// no "//host", no backslashes.
func safeNext(next string) string {
	u, err := url.Parse(next)
	if err != nil || next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") ||
		strings.Contains(next, "\\") || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return next
}

func (s *Server) loginPage(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, status, "login.html", map[string]string{"Msg": msg})
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	if !s.tokenOK(r) {
		s.loginPage(w, http.StatusUnauthorized, "")
		return
	}
	st, err := s.B.State(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	v := view(st, viewMode(r))
	v.F = filterFromRequest(r)
	v.U, _ = s.B.Usage(r.Context())
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, http.StatusOK, "page.html", v)
}

func (s *Server) fragment(w http.ResponseWriter, r *http.Request) {
	st, err := s.B.State(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	v := view(st, viewMode(r))
	v.U, _ = s.B.Usage(r.Context())
	s.render(w, http.StatusOK, "main", v)
}

// render executes a template into a buffer first, so a failure can't leave a
// half-written page with an error appended.
func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.Log.Error("render", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.Log.Error("backend", "err", err)
	http.Error(w, "backend unavailable", http.StatusBadGateway)
}

func (s *Server) apiState(w http.ResponseWriter, r *http.Request) {
	st, err := s.B.State(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, st)
}

// apiUsage is the small live CPU/memory snapshot the page polls while open.
func (s *Server) apiUsage(w http.ResponseWriter, r *http.Request) {
	u, err := s.B.Usage(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, u)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// events streams "state" events carrying the new version number.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if !s.clientJoined() {
		http.Error(w, "too many open streams", http.StatusServiceUnavailable)
		return
	}
	defer s.clientLeft()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	ctx := r.Context()
	updates := s.B.Watch(ctx)
	// Long-lived: lift the server's read/write deadlines for this response.
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Time{})
	rc.SetReadDeadline(time.Time{})

	send := func() bool {
		st, err := s.B.State(ctx)
		if err != nil {
			return false
		}
		_, err = fmt.Fprintf(w, "event: state\ndata: %d\n\n", st.Version)
		flusher.Flush()
		return err == nil
	}
	if !send() {
		return
	}
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-updates:
			if !ok {
				fmt.Fprint(w, "event: bye\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			if !send() {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	var a engine.Action
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&a); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	msg, err := s.B.Do(r.Context(), a)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg})
}

func (s *Server) clientJoined() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clients >= maxStreams {
		return false
	}
	s.clients++
	s.everServed = true
	return true
}

func (s *Server) clientLeft() {
	s.mu.Lock()
	s.clients--
	if s.clients == 0 {
		s.lastLeft = time.Now()
	}
	s.mu.Unlock()
}

// Idle reports whether no browser/TUI has been connected for at least d.
// Before the first client connects, startGrace applies instead.
func (s *Server) Idle(d, startGrace time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clients > 0 || time.Now().Before(s.graceUntil) {
		return false // someone may still be opening a sign-in link
	}
	if !s.everServed {
		return time.Since(s.lastLeft) > startGrace
	}
	return time.Since(s.lastLeft) > d
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
