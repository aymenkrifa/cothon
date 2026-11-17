// Command cothon watches the jobs of a local GitHub Actions self-hosted runner.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/aymenkrifa/cothon/internal/browser"
	"github.com/aymenkrifa/cothon/internal/client"
	"github.com/aymenkrifa/cothon/internal/config"
	"github.com/aymenkrifa/cothon/internal/engine"
	"github.com/aymenkrifa/cothon/internal/fleet"
	"github.com/aymenkrifa/cothon/internal/runnerctl"
	"github.com/aymenkrifa/cothon/internal/server"
	"github.com/aymenkrifa/cothon/internal/tui"
)

const usageText = `cothon — watch the jobs of your self-hosted GitHub Actions runner

usage: cothon <command> [flags]

commands:
  web       open the web UI. Starts the server in the background if needed (it
            exits ~1 min after the last tab closes) and gives your prompt back.
            Optional page: web /list/OWNER/REPO?status=failed
  link      print a one-time sign-in link for another browser
  start     run the server in the background until "cothon stop"
  stop      stop the background server
  status    server and runner status, with CPU and memory
  serve     run the server in the foreground (for systemd, or to watch its logs)
  tui       terminal UI (uses the server if running, otherwise runs standalone)
  scan      print recent jobs and exit
  runner    status | start | stop | restart [--force] | logs [-f] | enable [--at-boot] | disable
  config    path | show | init

Run "cothon <command> -h" for flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	migrateLegacyDirs()
	var err error
	switch cmd {
	case "web":
		err = cmdWeb(args)
	case "serve":
		err = cmdServe(args)
	case "tui":
		err = cmdTUI(args)
	case "link":
		err = cmdLink(args)
	case "start":
		err = cmdStart(args)
	case "stop":
		err = cmdStop(args)
	case "status":
		err = cmdStatus(args)
	case "scan":
		err = cmdScan(args)
	case "runner":
		err = cmdRunner(args)
	case "config":
		err = cmdConfig(args)
	case "-h", "--help", "help":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usageText)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cothon:", err)
		os.Exit(1)
	}
}

func loadConfig() (config.Config, error) {
	return config.Load()
}

func logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// ---------- serve / web ----------

type daemonOpts struct {
	listen    string
	mode      string // serve | web
	notify    bool
	openPage  bool
	listenSet bool          // --listen given explicitly: don't fall back to another port
	browser   string        // command to open the link with ("" = default browser, "none" = print only)
	openPath  string        // page to open, e.g. /list/OWNER/REPO?status=failed
	idleExit  time.Duration // web mode: exit when no client for this long (0 = never)
}

func cmdServe(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", cfg.Listen, "address to listen on (loopback only)")
	noNotify := fs.Bool("no-notify", false, "disable desktop notifications")
	open := fs.Bool("open", false, "open the web UI in a browser (see --browser)")
	browser := fs.String("browser", cfg.Browser, `browser command for --open, e.g. "firefox"`)
	idle := fs.Duration("idle-exit", 0, "exit when no browser has been connected this long (0 = never)")
	mode := fs.String("mode", "serve", "serve | web (how clients describe this server)")
	fs.Parse(args)
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return runDaemon(cfg, daemonOpts{listen: *listen, listenSet: set["listen"], mode: *mode, notify: !*noNotify,
		openPage: *open, browser: *browser, idleExit: *idle})
}

func cmdWeb(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("web", flag.ExitOnError)
	listen := fs.String("listen", cfg.Listen, "address to listen on (loopback only)")
	idle := fs.Duration("idle-exit", time.Minute, "exit after the last browser tab has been closed this long")
	noNotify := fs.Bool("no-notify", false, "disable desktop notifications while open")
	browser := fs.String("browser", cfg.Browser, `browser command to open the link with, e.g. "firefox" (default: your default browser)`)
	noOpen := fs.Bool("no-open", false, "don't open a browser; just print the sign-in link")
	foreground := fs.Bool("foreground", false, "run the server in this terminal instead of the background")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: cothon web [--browser CMD | --no-open] [flags] [path]")
		fmt.Fprintln(os.Stderr, "   e.g. cothon web --browser firefox /list/OWNER/REPO?status=failed")
		fs.PrintDefaults()
	}
	fs.Parse(flagsFirst(args))
	if *noOpen {
		*browser = "none"
	}
	path, err := pagePath(fs)
	if err != nil {
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	if c, err := client.Discover(context.Background()); err == nil {
		link, err := c.LoginURL(context.Background(), path)
		if err != nil {
			return err
		}
		fmt.Println("cothon is already running at http://" + c.Addr() + "/")
		for _, f := range []string{"listen", "idle-exit", "no-notify"} {
			if set[f] {
				fmt.Fprintf(os.Stderr, "cothon: --%s ignored: it applies when web starts the server\n", f)
			}
		}
		return showLink(link, *browser)
	}
	if *foreground {
		return runDaemon(cfg, daemonOpts{listen: *listen, listenSet: set["listen"], mode: "web", notify: !*noNotify,
			openPage: true, browser: *browser, openPath: path, idleExit: *idle})
	}
	args = []string{"serve", "--mode", "web", "--listen", *listen, "--idle-exit", idle.String()}
	if *noNotify {
		args = append(args, "--no-notify")
	}
	c, err := startBackground(args, set["listen"])
	if err != nil {
		return err
	}
	link, err := c.LoginURL(context.Background(), path)
	if err != nil {
		return err
	}
	fmt.Printf("cothon is running in the background at http://%s/ (it stops %s after the last tab closes; \"cothon stop\" stops it now)\n", c.Addr(), humanDur(*idle))
	return showLink(link, *browser)
}

// cmdStart runs the server in the background until "cothon stop".
func cmdStart(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	listen := fs.String("listen", cfg.Listen, "address to listen on (loopback only)")
	noNotify := fs.Bool("no-notify", false, "disable desktop notifications")
	fs.Parse(args)
	if c, err := client.Discover(context.Background()); err == nil {
		fmt.Printf("cothon is already running at http://%s/\n", c.Addr())
		return nil
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	bg := []string{"serve", "--listen", *listen}
	if *noNotify {
		bg = append(bg, "--no-notify")
	}
	c, err := startBackground(bg, set["listen"])
	if err != nil {
		return err
	}
	fmt.Printf("cothon is running in the background at http://%s/\nopen it with \"cothon web\"; stop it with \"cothon stop\"; log: %s\n", c.Addr(), daemonLog())
	return nil
}

// cmdStop stops the background server.
func cmdStop(args []string) error {
	rt, err := client.ReadRuntime()
	if err != nil {
		fmt.Println("cothon is not running")
		return nil
	}
	if err := syscall.Kill(rt.PID, syscall.SIGTERM); err != nil {
		return err
	}
	for i := 0; i < 50; i++ {
		if _, err := client.ReadRuntime(); err != nil {
			fmt.Println("cothon stopped")
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("pid %d did not stop within 10s", rt.PID)
}

// cmdStatus prints the server and runner status, with live usage.
func cmdStatus(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	var u *engine.Usage
	if c, err := client.Discover(context.Background()); err == nil {
		mode, pid := "running until \"cothon stop\"", 0
		if rt, err := client.ReadRuntime(); err == nil {
			pid = rt.PID
			if rt.Mode == "web" {
				mode = "stops ~1 min after the last browser tab closes"
			}
		}
		fmt.Printf("cothon: http://%s/ (pid %d, %s)\n", c.Addr(), pid, mode)
		u, _ = c.Usage(context.Background())
	} else {
		fmt.Println("cothon: not running (\"cothon start\" or \"cothon web\")")
	}
	fmt.Println()
	return printRunnerStatus(cfg, u)
}

// startBackground starts "cothon <args>" detached from this terminal
// (own session, output to the daemon log) and waits until it answers.
func startBackground(args []string, listenSet bool) (*client.Client, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logPath := daemonLog()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > 5<<20 {
		os.Rename(logPath, logPath+".1")
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	start, _ := logf.Seek(0, io.SeekEnd)
	cmd := exec.Command(exe, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return nil, fmt.Errorf("the server exited at startup:\n%s", logTail(logPath, start))
		case <-time.After(200 * time.Millisecond):
		}
		if c, err := client.Discover(context.Background()); err == nil {
			return c, nil
		}
	}
	return nil, fmt.Errorf("the server did not answer within 15s; see %s", logPath)
}

func daemonLog() string { return filepath.Join(stateHome(), "cothon", "daemon.log") }

func logTail(path string, from int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	f.Seek(from, io.SeekStart)
	b, _ := io.ReadAll(io.LimitReader(f, 4096))
	return strings.TrimSpace(string(b))
}

// pagePath validates the optional page argument ("/list/OWNER/REPO?...").
func pagePath(fs *flag.FlagSet) (string, error) {
	if fs.NArg() > 1 {
		return "", fmt.Errorf("one page path at most (got %q)", fs.Args())
	}
	p := fs.Arg(0)
	if p != "" && !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p, nil
}

// cmdLink prints a fresh sign-in link from the running daemon, for opening
// cothon in another browser.
func cmdLink(args []string) error {
	fs := flag.NewFlagSet("link", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: cothon link [path]   prints a one-time sign-in link (valid 1 minute)")
	}
	fs.Parse(args)
	c, err := client.Discover(context.Background())
	if err != nil {
		return errors.New("no cothon daemon is running (start one with `cothon web` or `cothon serve`)")
	}
	path, err := pagePath(fs)
	if err != nil {
		return err
	}
	link, err := c.LoginURL(context.Background(), path)
	if err != nil {
		return err
	}
	fmt.Println(link)
	return nil
}

// flagsFirst lets flags follow the positional path ("web /list --no-open").
func flagsFirst(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			rest = append(rest, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "-"):
			flags = append(flags, a)
			// Value flags given as "--browser firefox".
			if !strings.Contains(a, "=") && i+1 < len(args) && takesValue(a) {
				flags = append(flags, args[i+1])
				i++
			}
		default:
			rest = append(rest, a)
		}
	}
	// "--" stops flag parsing: a dangling value flag fails loudly instead of
	// swallowing the path, and positionals stay positional.
	return append(append(flags, "--"), rest...)
}

func takesValue(flag string) bool {
	switch strings.TrimLeft(flag, "-") {
	case "browser", "listen", "idle-exit":
		return true
	}
	return false
}

// showLink prints the one-time sign-in link and opens it with the chosen
// browser: "" = the desktop's default, "none" = print only, otherwise a
// command such as "firefox" or "google-chrome --incognito".
func showLink(link, browser string) error {
	fmt.Println("Sign-in link (one use, valid 1 minute; run `cothon link` for another):")
	fmt.Println("  " + link)
	if browser == "none" {
		return nil
	}
	if err := openBrowser(link, browser); err != nil {
		fmt.Fprintf(os.Stderr, "cothon: could not start the browser (%v); open the link above yourself\n", err)
	}
	return nil
}

func runDaemon(cfg config.Config, o daemonOpts) error {
	if err := server.CheckLoopback(o.listen); err != nil {
		return err
	}
	unlock, err := client.Lock()
	if errors.Is(err, client.ErrDaemonRunning) {
		if rt, err := client.ReadRuntime(); err == nil {
			return fmt.Errorf("a cothon daemon is already running (pid %d, http://%s); use `cothon web` to open it", rt.PID, rt.Addr)
		}
		return errors.New("a cothon daemon is already starting; use `cothon web` to open it")
	} else if err != nil {
		return err
	}
	defer unlock()
	token, err := client.SessionToken()
	if err != nil {
		return fmt.Errorf("session secret: %w", err)
	}

	log := logger()
	ctx, stop := signalContext()
	defer stop()

	ln, err := net.Listen("tcp", o.listen)
	if err != nil && o.mode == "web" && !o.listenSet {
		// Port taken by something else: pick a free one.
		host, _, _ := net.SplitHostPort(o.listen)
		ln, err = net.Listen("tcp", net.JoinHostPort(host, "0"))
	}
	if err != nil {
		return err
	}
	addr := ln.Addr().String()

	eng := engine.New(cfg, engine.Options{Notify: o.notify, Org: true, Log: log})
	engCtx, engCancel := context.WithCancel(ctx)
	defer engCancel()
	ready := make(chan struct{})
	engErr := make(chan error, 1)
	go func() { engErr <- eng.Run(engCtx, ready) }()
	select {
	case <-ready:
	case err := <-engErr:
		return err
	}

	srv, err := server.New(eng, token, addr)
	if err != nil {
		return err
	}
	srv.Log = log
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	cleanup, err := client.WriteRuntime(client.Runtime{Addr: addr, Token: token, PID: os.Getpid(), Mode: o.mode})
	if err != nil {
		return err
	}
	defer cleanup()

	srvErr := make(chan error, 1)
	go func() { srvErr <- httpSrv.Serve(ln) }()
	log.Info("cothon listening", "url", "http://"+addr+"/", "runner_dir", cfg.RunnerDir, "org", cfg.Org.Enabled, "mode", o.mode)
	if o.openPage {
		if err := showLink(client.LoginURL(addr, srv.NewLoginCode(), o.openPath), o.browser); err != nil {
			log.Warn("browser", "err", err)
		}
	} else {
		log.Info("open it with `cothon web` (or print a sign-in link with `cothon link`)")
	}

	var idle <-chan time.Time
	if o.idleExit > 0 {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		idle = t.C
	}
	finish := func(reason string) error {
		log.Info("shutting down", "reason", reason)
		stop() // a second Ctrl-C now kills the process
		err := shutdown(httpSrv, engCancel)
		select { // let the engine close its streams and watchers
		case <-engErr:
		case <-time.After(3 * time.Second):
		}
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return finish("signal")
		case err := <-srvErr:
			engCancel()
			return err
		case err := <-engErr:
			shutdown(httpSrv, func() {})
			if err == nil {
				err = errors.New("engine stopped")
			}
			return err
		case <-idle:
			if srv.Idle(o.idleExit, 2*time.Minute) {
				return finish("no browser connected")
			}
		}
	}
}

func shutdown(s *http.Server, stopEngine func()) error {
	stopEngine() // closes event streams so Shutdown doesn't wait on them
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func openBrowser(url, command string) error { return browser.Open(url, command) }

// ---------- tui ----------

func cmdTUI(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	local := fs.Bool("local", false, "don't use a running daemon; read the runner logs in-process")
	notify := fs.Bool("notify", false, "standalone mode: send desktop notifications while open")
	fs.Parse(args)

	ctx, stop := signalContext()
	defer stop()

	source := "standalone"
	if !*local {
		c, err := client.Discover(ctx)
		if err == nil {
			return tui.Run(ctx, c, "daemon "+c.Addr(), tui.Options{Browser: cfg.Browser})
		}
		if _, rerr := client.ReadRuntime(); rerr == nil {
			// A daemon is recorded but doesn't answer: say so (also in the
			// TUI header) instead of silently running a second engine.
			fmt.Fprintf(os.Stderr, "cothon: daemon not reachable (%v); running standalone\n", err)
			source = "standalone (daemon unreachable)"
		}
	}
	// Standalone: quiet logs (they would draw over the TUI).
	eng := engine.New(cfg, engine.Options{Notify: *notify, Org: true, Log: slog.New(slog.NewTextHandler(discard{}, nil))})
	ready := make(chan struct{})
	engErr := make(chan error, 1)
	engCtx, engCancel := context.WithCancel(ctx)
	defer engCancel()
	go func() { engErr <- eng.Run(engCtx, ready) }()
	select {
	case <-ready:
	case err := <-engErr:
		return err
	}
	// If the engine dies, Run closes its subscriptions and the TUI shows it.
	err = tui.Run(ctx, eng, source, tui.Options{Browser: cfg.Browser})
	engCancel()
	return err
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// ---------- scan ----------

func cmdScan(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	dir := fs.String("runner-dir", cfg.RunnerDir, "self-hosted runner directory")
	limit := fs.Int("n", 20, "number of jobs to show (0 = all)")
	asJSON := fs.Bool("json", false, "print the snapshot as JSON")
	fs.Parse(args)

	sc := fleet.NewScanner(*dir)
	sc.Procs = func() fleet.Processes { return fleet.FindProcesses(*dir) }
	snap, err := sc.Scan()
	if err != nil {
		return fmt.Errorf("%w\n(is runner_dir right? set it with --runner-dir or in %s)", err, config.Path())
	}
	if *limit > 0 && len(snap.Jobs) > *limit {
		snap.Jobs = snap.Jobs[:*limit]
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(snap)
	}

	name, url := "?", ""
	if snap.Runner != nil {
		name, url = snap.Runner.Name, snap.Runner.GitHubURL
	}
	fmt.Printf("runner %s (%s) — listener %s", name, url, snap.Listener.Event)
	if !snap.Listener.Since.IsZero() {
		fmt.Printf(" since %s", snap.Listener.Since.Local().Format("Jan 02 15:04"))
	}
	if snap.Processes != nil && !snap.Processes.Running() {
		fmt.Print(" (no listener process)")
	}
	fmt.Print("\n\n")

	now := time.Now()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STARTED\tSTATUS\tREPO\tWORKFLOW\tJOB\tRUN\tDURATION")
	for _, j := range snap.Jobs {
		state := string(j.Status)
		if j.Status == fleet.StatusCompleted {
			state = j.Result
		}
		run := j.RunNumber
		if j.RunAttempt != "" && j.RunAttempt != "1" {
			run += " (attempt " + j.RunAttempt + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t#%s\t%s\n",
			j.Started.Local().Format("Jan 02 15:04"), state, j.Repository, j.Workflow, j.Name, run,
			j.Duration(now).Round(time.Second))
	}
	tw.Flush()
	for _, e := range snap.Errors {
		fmt.Fprintln(os.Stderr, "warning:", e)
	}
	return nil
}

// ---------- runner ----------

func cmdRunner(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("runner", flag.ExitOnError)
	force := fs.Bool("force", false, "stop/restart even if a job is running (the job is canceled)")
	follow := fs.Bool("f", false, "logs: keep following new output")
	lines := fs.Int("n", 40, "logs: lines of past output to show")
	atBoot := fs.Bool("at-boot", false, "enable: start at boot, without logging in (enables lingering)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: cothon runner <command>
  status                 is it running, who manages it, CPU and memory
  start                  start run.sh detached (or the service, if enabled)
  stop [--force]         stop after the current job (--force: now, canceling it)
  restart [--force]      stop, then start again
  logs [-f] [-n N]       what the runner prints (from its diag log)
  enable [--at-boot]     start at login (or boot) and restart if it crashes
  disable                undo enable`)
		fs.PrintDefaults()
	}
	// Accept flags before or after the subcommand.
	var sub string
	var rest []string
	for _, a := range args {
		if sub == "" && !strings.HasPrefix(a, "-") {
			sub = a
		} else {
			rest = append(rest, a)
		}
	}
	fs.Parse(rest)
	if sub == "" {
		sub = "status"
	}

	ctl := &runnerctl.Controller{
		RunnerDir: cfg.RunnerDir,
		Mode:      cfg.RunnerStart,
		LogFile:   filepath.Join(stateHome(), "cothon", "runner.out"),
	}
	procs := ctl.Procs()

	switch sub {
	case "status":
		var u *engine.Usage
		if c, err := client.Discover(context.Background()); err == nil {
			u, _ = c.Usage(context.Background())
		}
		return printRunnerStatus(cfg, u)
	case "logs":
		return runnerLogs(cfg, *follow, *lines)
	case "enable":
		msg, err := ctl.Enable(*atBoot)
		if err == nil {
			fmt.Println(msg)
		}
		return err
	case "disable":
		msg, err := ctl.Disable()
		if err == nil {
			fmt.Println(msg)
		}
		return err
	}

	// Prefer the daemon so its UI reflects (and can track) the operation.
	kind := map[string]string{"start": engine.ActRunnerStart, "stop": engine.ActRunnerStop, "restart": engine.ActRunnerRestart}[sub]
	if kind == "" {
		fs.Usage()
		return fmt.Errorf("unknown runner command %q", sub)
	}
	if c, err := client.Discover(context.Background()); err == nil {
		msg, err := c.Do(context.Background(), engine.Action{Kind: kind, Force: *force})
		if err != nil {
			return err
		}
		fmt.Println(msg)
		return nil
	}

	ctx, stop := signalContext()
	defer stop()
	switch sub {
	case "start":
		msg, err := ctl.Start()
		if err != nil {
			return err
		}
		fmt.Println(msg)
		return nil
	case "stop", "restart":
		if !procs.Alive() {
			if sub == "restart" {
				msg, err := ctl.Start()
				if err == nil {
					fmt.Println(msg)
				}
				return err
			}
			return errors.New("runner is not running")
		}
		if !*force {
			if ctl.Procs().Busy() {
				fmt.Println("a job is running; waiting for it to finish (Ctrl-C to give up, --force to cancel it)…")
			}
			for ctl.Procs().Busy() {
				select {
				case <-ctx.Done():
					return errors.New("gave up waiting")
				case <-time.After(2 * time.Second):
				}
			}
		}
		// A job can still be assigned in the instant before SIGINT lands; the
		// runner then reports it as canceled.
		if err := ctl.Interrupt(); err != nil {
			return err
		}
		if !ctl.WaitStopped(ctx, 90*time.Second) {
			return errors.New("the runner did not exit within 90s (or you interrupted the wait)")
		}
		fmt.Println("runner stopped")
		if sub == "restart" {
			msg, err := ctl.Start()
			if err != nil {
				return err
			}
			fmt.Println(msg)
		}
		return nil
	}
	return nil
}

func stateHome() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state")
}

// ---------- config ----------

func cmdConfig(args []string) error {
	sub := "show"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "path":
		fmt.Println(config.Path())
	case "init":
		p := config.Path()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists", p)
		} else if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.WriteString(config.Example); err != nil {
			return err
		}
		fmt.Println("wrote", p)
	case "show":
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		fmt.Printf("# %s\n", config.Path())
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(cfg)
	default:
		return fmt.Errorf("unknown config command %q (path|show|init)", sub)
	}
	return nil
}

// printRunnerStatus prints the runner's state, who manages it, and usage.
func printRunnerStatus(cfg config.Config, u *engine.Usage) error {
	ctl := &runnerctl.Controller{RunnerDir: cfg.RunnerDir}
	procs := ctl.Procs()
	rc, _ := fleet.ReadRunnerConfig(cfg.RunnerDir)
	if rc != nil {
		fmt.Printf("runner:   %s (%s)\n", rc.Name, rc.GitHubURL)
	}
	fmt.Printf("dir:      %s\n", cfg.RunnerDir)
	switch {
	case !procs.Alive():
		fmt.Println("status:   stopped")
	case !procs.Running():
		fmt.Printf("status:   restarting (run.sh pid %d)\n", procs.SupervisorPID)
	case procs.Busy():
		fmt.Printf("status:   busy (listener pid %d, worker pid %d)\n", procs.ListenerPID, procs.WorkerPID)
	default:
		fmt.Printf("status:   idle (listener pid %d)\n", procs.ListenerPID)
	}
	fmt.Printf("managed:  %s\n", ctl.Manager())
	if procs.Listeners > 1 {
		fmt.Printf("warning:  %d listeners are running (session conflict)\n", procs.Listeners)
	}
	if procs.Alive() {
		if u == nil { // no server: measure once here
			a := fleet.SampleProcs(procs.SupervisorPID, procs.ListenerPID)
			ja := fleet.SampleTree(procs.WorkerPID)
			time.Sleep(time.Second)
			b := fleet.SampleProcs(procs.SupervisorPID, procs.ListenerPID)
			jb := fleet.SampleTree(procs.WorkerPID)
			u = &engine.Usage{Runner: engine.ProcUsage{CPU: ticksPct(a, b), RSS: b.RSS, Procs: b.Procs}}
			if procs.Busy() {
				u.Job = &engine.JobUsage{Current: engine.ProcUsage{CPU: ticksPct(ja, jb), RSS: jb.RSS, Procs: jb.Procs}}
			}
		}
		fmt.Printf("usage:    runner %.0f%% CPU, %s\n", u.Runner.CPU, humanBytes(u.Runner.RSS))
		if u.Job != nil {
			fmt.Printf("job:      %.0f%% CPU, %s, %d processes", u.Job.Current.CPU, humanBytes(u.Job.Current.RSS), u.Job.Current.Procs)
			if u.Job.Summary.PeakRSS > 0 {
				fmt.Printf(" (peak %s, avg %.0f%% CPU)", humanBytes(u.Job.Summary.PeakRSS), u.Job.Summary.AvgCPU)
			}
			fmt.Println()
		}
	}
	return nil
}

func ticksPct(a, b fleet.Sample) float64 {
	if b.Ticks < a.Ticks {
		return 0
	}
	return float64(b.Ticks-a.Ticks) / fleet.ClockTicks * 100 // over one second
}

func humanBytes(b uint64) string {
	switch {
	case b < 1<<20:
		return fmt.Sprintf("%d KB", b>>10)
	case b < 1<<30:
		return fmt.Sprintf("%d MB", b>>20)
	default:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	}
}

// runnerLogs prints what the runner printed (its console output, mirrored in
// its diag log), optionally following it across restarts.
func runnerLogs(cfg config.Config, follow bool, n int) error {
	diag := filepath.Join(cfg.RunnerDir, "_diag")
	current := func() string {
		if p := fleet.FindProcesses(cfg.RunnerDir).ListenerLog; p != "" {
			return p
		}
		var newest string
		var t time.Time
		files, _ := filepath.Glob(filepath.Join(diag, "Runner_*.log"))
		for _, f := range files {
			if fi, err := os.Stat(f); err == nil && fi.ModTime().After(t) {
				newest, t = f, fi.ModTime()
			}
		}
		return newest
	}
	path := current()
	if path == "" {
		return fmt.Errorf("no runner logs in %s", diag)
	}
	print := func(ls []fleet.ConsoleLine) {
		for _, l := range ls {
			if l.Error {
				fmt.Println("ERROR " + l.Text)
			} else {
				fmt.Println(l.Text)
			}
		}
	}
	lines, off, err := fleet.ReadConsole(path, 0)
	if err != nil {
		return err
	}
	if n >= 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	print(lines)
	if !follow {
		return nil
	}
	ctx, stop := signalContext()
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
		if p := current(); p != "" && p != path {
			fmt.Printf("--- runner restarted: %s ---\n", filepath.Base(p))
			path, off = p, 0
		}
		var ls []fleet.ConsoleLine
		ls, off, err = fleet.ReadConsole(path, off)
		if err == nil {
			print(ls)
		}
	}
}

func humanDur(d time.Duration) string {
	switch {
	case d%time.Hour == 0 && d >= time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	case d%time.Minute == 0 && d >= time.Minute:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	default:
		return d.String()
	}
}

// migrateLegacyDirs moves data from the project's former name (githubfleet)
// once: the session secret (so browser sign-ins survive), usage history and
// logs, and the config.
func migrateLegacyDirs() {
	home, _ := os.UserHomeDir()
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	for _, base := range []string{stateHome(), configHome} {
		old, cur := filepath.Join(base, "githubfleet"), filepath.Join(base, "cothon")
		if _, err := os.Stat(cur); err == nil {
			continue
		}
		if fi, err := os.Lstat(old); err == nil && fi.IsDir() {
			os.Rename(old, cur)
		}
	}
}
