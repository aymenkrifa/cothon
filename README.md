# cothon

> The *cothon* was Carthage's circular war harbour: the whole fleet moored in a
> ring, and from the admiral's tower on the island at its centre one could
> watch every ship and send it out. This is that tower for your runner.

Watch every job your self-hosted GitHub Actions runner executes — across all
repos — from one place: a web UI, a terminal UI, and desktop notifications.

It reads the runner's own diagnostic logs (`~/actions-runner/_diag`), so it
works offline, needs no GitHub admin rights, and shows a job the moment it
starts. Optionally it also polls your org for queued runs.

## Requirements

* **Linux** — cothon reads `/proc` and the runner's `_diag` logs, and the
  optional auto-start uses a systemd user service.
* A self-hosted runner installed from GitHub's tarball (`~/actions-runner` by
  default; set `runner_dir` otherwise).
* [Go](https://go.dev/dl/) 1.24+ to build it.
* The [GitHub CLI](https://cli.github.com), logged in (`gh auth login`), for
  rerun/cancel, the queue and the optional org view. Watching jobs works without it.

## Install

```sh
go install github.com/aymenkrifa/cothon/cmd/cothon@latest
```

or from a clone:

```sh
go build -o ~/.local/bin/cothon ./cmd/cothon
```

Then `cothon web` (or `cothon tui`).

## Use

| Command | What it does |
|---|---|
| `cothon web [path]` | Opens the web UI and gives your prompt back. Starts the server in the background if needed; it stops ~1 min after the last tab closes. `--browser firefox`, `--no-open` (print the link), `--foreground`. |
| `cothon start` / `stop` / `status` | Run the server in the background until `stop`; `status` shows the server, the runner, and CPU/memory. Log: `~/.local/state/cothon/daemon.log`. |
| `cothon link [path]` | Prints a fresh one-time sign-in link — paste it into any browser. |
| `cothon tui` | Terminal UI. Uses the server if one runs, otherwise reads the logs itself. |
| `cothon serve` | The server in the foreground (for systemd or debugging). |
| `cothon scan` | Prints recent jobs (`--json` for scripts). |
| `cothon config init` | Writes a commented config to `~/.config/cothon/config.toml`. |

### The runner itself

`cothon runner …` replaces running `~/actions-runner/run.sh` by hand:

| Command | What it does |
|---|---|
| `runner status` | Running? Busy? Who manages it (manual, service)? CPU and memory. |
| `runner start` | Starts `run.sh` detached — closing the terminal doesn't stop it. |
| `runner stop` / `restart` | Waits for the current job (`--force` cancels it instead). |
| `runner logs [-f] [-n N]` | What the runner prints, however it was started (`-f` follows, across restarts). |
| `runner enable [--at-boot]` | Installs a systemd user service: starts at login (or at boot with `--at-boot`) and restarts if it crashes. A runner already running keeps going; `runner restart` moves it under the service. |
| `runner disable` | Removes that service (a running runner keeps running). |

### Links you can bookmark

Filters live in the URL, so any view can be bookmarked or shared between
tabs (back/forward works too):

```
/grouped                                   grouped view, all repos
/list/my-org/my-repo                       list view, one repo
/grouped?status=failed&q=backend           status: running | failed | passed | canceled
```

`cothon web /list/OWNER/REPO?status=failed` opens a page directly.
Set `browser = "firefox"` (or `"none"` to only print links) in the config to
stop using the default browser.

### CPU and memory

The Runner card shows what the runner itself uses (listener + `run.sh`). While
a job runs, the Now panel shows its live CPU, memory, peak memory and process
count with a CPU graph — the whole process tree under `Runner.Worker`,
including short-lived compilers and test processes (containers started through
Docker run under the Docker daemon and aren't included). Jobs are measured while
cothon's server (or TUI) runs; jobs that ran with nothing open show "—". Finished jobs keep
their peak memory and average CPU (the "Peak mem" column; hover for details),
saved in `~/.local/state/cothon/usage.json`. `/proc` is read every 2 s
while a job runs, every 10 s otherwise.

### Queue

Jobs waiting for this runner show up in the **Now** panel (and the TUI's
Queue tab, with a count in its header). The runner's logs only know a job once
it starts, so cothon asks GitHub — only about the repos this runner worked
on in the last 7 days, every 30 s, with conditional requests (unchanged
answers are free). Turn it off with `[queue] enabled = false`.

### Views

* **Grouped** (default): repo → workflow run → jobs. Repos start open, runs
  start collapsed (click a row, or Enter/Space, to fold). Each repo shows its
  newest 5 runs ("Show N older runs" for the rest). Filters narrow what's
  listed but never fold or unfold anything for you.
* **List**: every job, newest first, 50 at a time.

Search and the status filter always cover all jobs.

TUI keys: `↑↓` move · `←→`/`enter` fold · `v` grouped/list · `o` open in browser ·
`r` rerun job · `f` rerun failed · `c` cancel run · `/` search · `F` status filter ·
`s`/`x`/`X`/`R` runner start/stop/stop-now/restart · `tab` org view · `q` quit.

## Resource use

Measured on a laptop runner with 349 log files (87 MB):

* daemon idle: ~0.03% of one core, ~20 MB RSS (inotify, no polling; a
  20-second liveness check);
* `scan` / TUI startup: ~0.2 s;
* nothing runs when no cothon command is open.

## Security

The API can rerun/cancel CI and stop the runner, and anything on this machine
can reach loopback. The measures below stop other websites in your browser,
other local users, and anything that can make loopback requests but can't read
your files. They do **not** stop code running as your own user — and that
includes the CI jobs of a runner started as you, which can read the session
file. To keep CI jobs out, run the runner as a separate user.

* the server listens on loopback only and rejects foreign `Host` headers (DNS rebinding);
* every request needs the session secret (`~/.local/state/cothon/session`, 0600):
  the TUI sends it as a bearer header; a browser only gets it as a
  `SameSite=Strict` HttpOnly cookie through a **one-time login link** that
  `cothon web` / `cothon link` print (valid 1 minute). Opening the URL by hand shows
  "run `cothon web`". The cookie lasts 30 days; delete the session file
  to sign every browser out;
* POSTs also need a custom header and a same-origin `Origin`/`Sec-Fetch-Site` (CSRF);
* actions only touch runs and jobs cothon is showing; `gh` is called with argv only;
* one daemon at a time (a `flock` in `$XDG_RUNTIME_DIR/cothon/`).

## Runner control notes

* `stop` sends SIGINT to `Runner.Listener` (exit 0 → `run.sh` exits too).
  Stopping while a job runs makes GitHub mark it canceled, hence "when idle".
* `start` refuses while a listener or `run.sh` is alive (run.sh restarts the
  listener during self-updates), and while a previous start is still coming
  up: a second listener only gets "A session for this runner already exists".
* `runner_start = "setsid"` (default) inherits your environment;
  `"systemd"` uses a transient `systemd-run --user` unit.

## License

[MIT](LICENSE)
