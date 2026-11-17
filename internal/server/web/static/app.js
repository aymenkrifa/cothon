// cothon web UI: live refresh over server-sent events, filtering that
// lives in the URL, folding, and actions. No framework, no build step.
//
// URL scheme (shareable, bookmarkable, back/forward aware):
//   /grouped                         grouped view, all repos
//   /list/OWNER/REPO                 list view, one repo
//   ...?status=failed&q=backend      status: running|failed|passed|canceled
"use strict";

const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];

const store = {
  get(k, d) { try { return JSON.parse(localStorage.getItem("cothon:" + k)) ?? d; } catch { return d; } },
  set(k, v) { try { localStorage.setItem("cothon:" + k, JSON.stringify(v)); } catch {} },
};

// ---------- state <-> URL ----------
const VIEWS = ["grouped", "list"];
const STATUSES = ["running", "failed", "passed", "canceled"];
const state = { view: "grouped", repo: "", status: "", q: "" };

function readURL() {
  const seg = location.pathname.split("/").filter(Boolean).map((x) => {
    try { return decodeURIComponent(x); } catch { return x; } // malformed %-escape
  });
  const p = new URLSearchParams(location.search);
  state.view = VIEWS.includes(seg[0]) ? seg[0] : store.get("view", "grouped");
  state.repo = VIEWS.includes(seg[0]) && seg.length >= 3 ? seg[1] + "/" + seg[2] : "";
  state.status = STATUSES.includes(p.get("status")) ? p.get("status") : "";
  state.q = (p.get("q") || "").slice(0, 200);
}

function urlFor(s) {
  let path = "/" + s.view;
  if (s.repo) path += "/" + s.repo.split("/").map(encodeURIComponent).join("/");
  const p = new URLSearchParams();
  if (s.status) p.set("status", s.status);
  if (s.q.trim()) p.set("q", s.q.trim());
  const qs = p.toString();
  return path + (qs ? "?" + qs : "");
}

// push: discrete changes (view, repo, status) get a history entry; typing replaces.
function writeURL(push) {
  const url = urlFor(state);
  if (url === location.pathname + location.search) return;
  history[push ? "pushState" : "replaceState"](null, "", url);
}

function syncControls() {
  for (const b of $$("#f-view button")) setPressed(b, b.dataset.v === state.view);
  for (const b of $$("#f-status button")) setPressed(b, b.dataset.v === state.status);
  if ($("#q").value !== state.q) $("#q").value = state.q;
  ensureRepoOption(state.repo);
  $("#f-repo").value = state.repo;
  $("#f-clear").classList.toggle("inactive", !(state.repo || state.status || state.q));
  document.title = "cothon" + (state.repo ? " · " + state.repo.split("/").pop() : "") + (state.status ? " · " + state.status : "");
}

function setPressed(b, on) {
  b.classList.toggle("on", on);
  b.setAttribute("aria-pressed", on);
}

function clearFilters() {
  Object.assign(state, { repo: "", status: "", q: "" });
  listLimit = LIST_PAGE;
  writeURL(true); syncControls(); applyFilter();
  $("#q").focus();
}

// ---------- folding (grouped view) ----------
// Repos start open, runs start closed. The user's folding is kept for the
// session (also across live refreshes) and is never overridden by filters.
const folded = new Map(); // key -> open (bool)
const isOpen = (row) => folded.has(row.dataset.key) ? folded.get(row.dataset.key) : row.classList.contains("repo-row");
function toggle(row) { folded.set(row.dataset.key, !isOpen(row)); }

// ---------- filtering & paging ----------
// Not everything at once: the newest runs per repo (grouped) and pages of
// jobs (list). Filters always search every job.
const RUNS_PER_REPO = 5, LIST_PAGE = 50;
const showAllRepos = new Set();
let listLimit = LIST_PAGE;

function rowMatches(tr, q) {
  const o = tr.dataset.outcome;
  const status = state.status === "passed" ? "succeeded" : state.status;
  return (!q || tr.dataset.search.includes(q)) &&
    (!state.repo || tr.dataset.repo === state.repo) &&
    (!status || o === status || (status === "failed" && o === "abandoned"));
}

function applyFilter() {
  const q = state.q.trim().toLowerCase();
  // Repo totals ignore filters; mute them so they don't read as filtered counts.
  $("#jobs").classList.toggle("filtering", !!(q || state.status));
  const jobRows = $$("#jobs tr.job-row");
  const matchRun = new Map(), matchRepo = new Set(); // run key -> matching job count
  let shown = 0;
  for (const tr of jobRows) {
    tr._match = rowMatches(tr, q);
    if (!tr._match) continue;
    shown++;
    matchRepo.add(tr.dataset.repo);
    if (tr.dataset.run) matchRun.set(tr.dataset.run, (matchRun.get(tr.dataset.run) || 0) + 1);
  }

  const repoOpen = new Map();
  for (const tr of $$("#jobs tr.repo-row")) {
    const open = isOpen(tr);
    repoOpen.set(tr.dataset.key, open);
    tr.classList.toggle("open", open);
    tr.setAttribute("aria-expanded", open);
    tr.hidden = !matchRepo.has(tr.dataset.key);
  }

  const runShown = new Map(), runsPerRepo = new Map(), hiddenRuns = new Map();
  for (const tr of $$("#jobs tr.run-row")) {
    const key = tr.dataset.key, repo = tr.dataset.repo;
    const open = isOpen(tr);
    tr.classList.toggle("open", open);
    tr.setAttribute("aria-expanded", open);
    let visible = matchRun.has(key) && repoOpen.get(repo) !== false;
    if (visible) {
      const n = (runsPerRepo.get(repo) || 0) + 1;
      if (n > RUNS_PER_REPO && !showAllRepos.has(repo)) {
        visible = false;
        hiddenRuns.set(repo, (hiddenRuns.get(repo) || 0) + 1);
      } else {
        runsPerRepo.set(repo, n);
      }
    }
    tr.hidden = !visible;
    runShown.set(key, visible && open);
    // While filtering, say how many jobs of the run match.
    const hint = $(".match-hint", tr);
    if (hint) {
      const total = Number(tr.dataset.jobs), m = matchRun.get(key) || 0;
      hint.hidden = !(q || state.status) || m === total;
      hint.textContent = m + " of " + total + " match";
    }
  }

  for (const tr of $$("#jobs tr.more-row")) {
    const repo = tr.dataset.repo, n = hiddenRuns.get(repo) || 0;
    const expanded = showAllRepos.has(repo);
    tr.hidden = repoOpen.get(repo) === false || !matchRepo.has(repo) || (n === 0 && !expanded);
    $("button", tr).textContent = n > 0 ? "Show " + n + " older run" + (n > 1 ? "s" : "") : "Show fewer runs";
  }

  let listed = 0, listHidden = 0;
  for (const tr of jobRows) {
    let visible = tr._match;
    if (tr.dataset.run) {
      visible = visible && runShown.get(tr.dataset.run) === true;
    } else if (visible) {
      listed++;
      if (listed > listLimit) { visible = false; listHidden++; }
    }
    tr.hidden = !visible;
  }

  const more = $(".more-list");
  if (more) {
    more.hidden = listHidden === 0;
    $("#more-list").textContent = "Show " + Math.min(LIST_PAGE, listHidden) + " more (" + listHidden + " not shown)";
  }
  const none = $("#no-match");
  if (none) none.hidden = shown > 0 || jobRows.length === 0;
  const wrap = $("#jobs .table-wrap");
  if (wrap) wrap.hidden = shown === 0 && jobRows.length > 0; // no orphan column headers
}

function ensureRepoOption(repo) {
  const sel = $("#f-repo");
  if (repo && ![...sel.options].some((o) => o.value === repo)) sel.add(new Option(repo, repo));
}

function syncRepoOptions() {
  const sel = $("#f-repo");
  const repos = [...new Set($$("#jobs tr.job-row").map((tr) => tr.dataset.repo).filter(Boolean))].sort();
  const current = [...sel.options].slice(1).map((o) => o.value);
  if (repos.join("|") !== current.join("|")) {
    sel.length = 1;
    for (const r of repos) sel.add(new Option(r, r));
  }
  ensureRepoOption(state.repo); // a repo from the URL stays selectable even with no jobs
  sel.value = state.repo;
}

// ---------- controls ----------
function setView(v, push) {
  if (v === state.view) return;
  state.view = v;
  store.set("view", v); // what "/" opens next time
  document.cookie = "cothon_view=" + v + "; path=/; SameSite=Strict; max-age=31536000";
  writeURL(push);
  syncControls();
  refresh();
}

function initControls() {
  for (const b of $$("#f-view button")) b.addEventListener("click", () => setView(b.dataset.v, true));
  for (const b of $$("#f-status button")) {
    b.addEventListener("click", () => {
      state.status = b.dataset.v;
      listLimit = LIST_PAGE;
      writeURL(true); syncControls(); applyFilter();
    });
  }
  $("#f-repo").addEventListener("change", (e) => {
    state.repo = e.target.value;
    listLimit = LIST_PAGE;
    writeURL(true); syncControls(); applyFilter();
  });
  let typing, pushNext = true;
  // The first keystroke after a click (view/repo/status) gets its own history
  // entry so Back returns to it; further typing replaces that entry.
  for (const el of $$("#f-view button, #f-status button")) el.addEventListener("click", () => { pushNext = true; });
  $("#f-repo").addEventListener("change", () => { pushNext = true; });
  $("#q").addEventListener("input", (e) => {
    state.q = e.target.value;
    listLimit = LIST_PAGE;
    applyFilter();
    clearTimeout(typing);
    const push = pushNext;
    pushNext = false;
    typing = setTimeout(() => { writeURL(push); syncControls(); }, 250);
  });
  $("#f-clear").addEventListener("click", clearFilters);

  // Folding and "show more" (links and buttons inside rows keep working).
  $("#jobs").addEventListener("click", (e) => {
    const moreRuns = e.target.closest("button.more-runs");
    if (moreRuns) {
      const r = moreRuns.dataset.repo;
      showAllRepos.has(r) ? showAllRepos.delete(r) : showAllRepos.add(r);
      applyFilter();
      return;
    }
    if (e.target.closest("#more-list")) { listLimit += LIST_PAGE; applyFilter(); return; }
    if (e.target.closest("[data-clear]")) { clearFilters(); return; }
    const row = e.target.closest("tr.toggle");
    if (!row || e.target.closest("a, button")) return;
    toggle(row);
    applyFilter();
  });
  $("#jobs").addEventListener("keydown", (e) => {
    const row = e.target.closest("tr.toggle");
    if (row && (e.key === "Enter" || e.key === " ") && e.target === row) { e.preventDefault(); toggle(row); applyFilter(); }
  });

  document.addEventListener("keydown", (e) => {
    const typingInField = ["INPUT", "SELECT", "TEXTAREA"].includes(document.activeElement?.tagName);
    if (e.key === "/" && !typingInField) { e.preventDefault(); $("#q").focus(); }
    if (e.key === "Escape") {
      if (armed) disarm(armed);
      else if (document.activeElement === $("#q")) $("#q").blur();
    }
  });

  window.addEventListener("popstate", () => {
    const prevView = state.view;
    readURL();
    syncControls();
    if (state.view !== prevView) refresh(); else applyFilter();
  });
}

// ---------- relative times (tick only while needed) ----------
function fmtDur(ms) {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return s + "s";
  const m = Math.floor(s / 60);
  if (m < 60) return m + "m " + String(s % 60).padStart(2, "0") + "s";
  return Math.floor(m / 60) + "h " + String(m % 60).padStart(2, "0") + "m";
}
function fmtAgo(ms) {
  const s = Math.round(ms / 1000);
  if (s < 45) return "just now";
  const m = Math.round(s / 60);
  if (m < 60) return m + " min ago";
  const h = Math.round(m / 60);
  if (h < 24) return h + " h ago";
  const d = Math.round(h / 24);
  return d === 1 ? "yesterday" : d + " days ago";
}
function tickTimes() {
  const now = Date.now();
  for (const t of $$("time[data-elapsed]")) t.textContent = fmtDur(now - Date.parse(t.dataset.elapsed));
  for (const t of $$("time[data-ago]")) t.textContent = fmtAgo(now - Date.parse(t.dataset.ago));
  for (const t of $$("time[data-for]")) t.textContent = fmtAgo(now - Date.parse(t.dataset.for)).replace(/ ago$/, "").replace("just now", "a moment");
}
let ticker = null;
function scheduleTicks() {
  clearInterval(ticker);
  tickTimes();
  const fast = $$("time[data-elapsed]").length > 0; // 1s only while a job runs
  ticker = setInterval(tickTimes, fast ? 1000 : 30000);
}

// ---------- live updates ----------
let refreshing = false, again = false;
async function refresh() {
  if (refreshing) { again = true; return; }
  refreshing = true;
  const view = state.view;
  try {
    const res = await fetch("/fragment?view=" + view, { credentials: "same-origin", cache: "no-store" });
    if (res.status === 401) { location.reload(); return; }
    if (!res.ok) throw new Error(res.statusText);
    const html = await res.text();
    if (view !== state.view) { again = true; return; } // the view changed meanwhile: fetch again
    const doc = new DOMParser().parseFromString(html, "text/html");
    swap("#status", $('[data-part="status"]', doc));
    swap("#jobs", $('[data-part="jobs"]', doc));
    afterRender();
  } catch (e) {
    console.warn("refresh failed", e);
  } finally {
    refreshing = false;
    if (again) { again = false; refresh(); }
  }
}
function swap(sel, part) {
  const el = $(sel);
  if (!el || !part) return;
  // Don't replace a button the user is about to confirm: retry once it disarms.
  if (el.querySelector("button.armed")) {
    clearTimeout(swap.later);
    swap.later = setTimeout(refresh, 4500);
    return;
  }
  // Keep keyboard focus on the same row/button across the re-render.
  const focused = el.contains(document.activeElement) ? document.activeElement : null;
  const key = focused?.closest("[data-key]")?.dataset.key;
  const kind = focused?.dataset?.kind;
  el.innerHTML = part.innerHTML;
  if (key) {
    const row = el.querySelector(`[data-key="${CSS.escape(key)}"]`);
    const target = (kind && row?.querySelector(`[data-kind="${CSS.escape(kind)}"]`)) || row;
    target?.focus({ preventScroll: true });
  }
}
function afterRender() {
  syncRepoOptions();
  applyFilter();
  scheduleTicks();
}

function connect() {
  const conn = $("#conn");
  let version = document.body.dataset.version;
  const es = new EventSource("/api/events");
  es.addEventListener("state", (e) => {
    conn.classList.remove("down");
    $(".conn-label", conn).textContent = "live";
    if (e.data === version) return;
    version = e.data;
    refresh();
  });
  es.addEventListener("bye", () => {
    // The daemon is shutting down (or restarting): try again shortly.
    es.close();
    down("stopped — retrying");
    setTimeout(connect, 3000);
  });
  es.onerror = () => {
    if (es.readyState === EventSource.CLOSED) {
      // The browser gave up (e.g. 401 after a restart): refresh() reloads on 401.
      down("disconnected");
      refresh();
      setTimeout(connect, 5000);
    } else {
      down("reconnecting…");
    }
  };
  function down(label) {
    conn.classList.add("down");
    $(".conn-label", conn).textContent = label;
  }
}

// ---------- actions ----------
function toast(msg, ok = true, ms) {
  const t = $("#toast");
  t.textContent = msg;
  t.classList.toggle("err", !ok);
  t.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => (t.hidden = true), ms || (ok ? 4000 : 8000));
}

// Confirmation texts live here, not repeated on every row.
const CONFIRM = {
  "cancel-run": "Cancel the whole run? Its jobs on other runners stop too.",
  "rerun-job": "Rerun this job?",
  "rerun-failed": "Rerun the failed jobs of this run?",
  "runner-stop": "Stop the runner now? The running job will be canceled.",
};
const ARM_MS = 4000;
let armed = null;

function arm(b) {
  if (armed) disarm(armed);
  armed = b;
  b.dataset.html = b.innerHTML;
  b.classList.add("armed");
  const text = CONFIRM[b.dataset.kind] || "Are you sure?";
  b.setAttribute("aria-label", "Confirm: " + text);
  if (b.classList.contains("icon")) b.textContent = "?"; else b.textContent = "Confirm";
  toast(text + " Click again to confirm.", true, ARM_MS);
  clearTimeout(arm.timer);
  arm.timer = setTimeout(() => { if (armed === b && !b.disabled) disarm(b); }, ARM_MS);
}

function disarm(b) {
  b.classList.remove("armed");
  if (b.dataset.html) { b.innerHTML = b.dataset.html; delete b.dataset.html; }
  b.removeAttribute("aria-label");
  // Icon-only buttons are named by their title; text buttons by their text.
  if (b.classList.contains("icon") && b.title) b.setAttribute("aria-label", b.title);
  if (armed === b) {
    armed = null;
    const t = $("#toast");
    if (t.textContent.endsWith("Click again to confirm.")) t.hidden = true;
  }
}

document.addEventListener("click", async (e) => {
  const b = e.target.closest("button.act");
  if (armed && b !== armed) disarm(armed); // clicking elsewhere cancels a pending confirm
  if (!b || b.disabled) return;
  if (b.hasAttribute("data-confirm") && !b.classList.contains("armed")) {
    arm(b);
    return;
  }
  const body = { kind: b.dataset.kind };
  if (b.dataset.repo) body.repo = b.dataset.repo;
  if (b.dataset.run) body.run_id = b.dataset.run;
  if (b.dataset.job) body.job_id = b.dataset.job;
  if (b.dataset.force) body.force = true;
  b.disabled = true;
  try {
    const res = await fetch("/api/action", {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-Cothon": "1" },
      body: JSON.stringify(body),
    });
    const out = await res.json().catch(() => ({ ok: false, message: res.status + " " + res.statusText }));
    toast(out.message || (out.ok ? "done" : "failed"), out.ok);
  } catch (err) {
    toast("request failed: " + err.message, false);
  } finally {
    if (b.isConnected) { b.disabled = false; disarm(b); }
  }
});

// ---------- theme: auto -> light -> dark ----------
function initTheme() {
  const btn = $("#theme");
  const label = { auto: "Theme: follow system", light: "Theme: light", dark: "Theme: dark" };
  const paint = () => {
    const t = document.documentElement.dataset.theme || "auto";
    btn.setAttribute("aria-label", label[t] + " (click to change)");
    btn.title = label[t];
  };
  btn.addEventListener("click", () => {
    const cur = document.documentElement.dataset.theme || "auto";
    const next = { auto: "light", light: "dark", dark: "auto" }[cur];
    if (next === "auto") delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = next;
    store.set("theme", next);
    paint();
  });
  paint();
}

// ---------- live CPU / memory ----------
// Every 2s while a job runs, every 10s otherwise; nothing while the tab is
// hidden. The response is tiny; the job list is not re-fetched for it.
function fmtBytes(b) {
  if (!b) return "";
  if (b < 1 << 20) return Math.round(b / 1024) + " KB";
  if (b < 1 << 30) return Math.round(b / (1 << 20)) + " MB";
  return (b / (1 << 30)).toFixed(1) + " GB";
}
const fmtCPU = (p) => Math.round(p) + "%";

function renderUsage(u) {
  const set = (root, k, v) => { const el = root && $(`[data-u="${k}"]`, root); if (el) el.textContent = v; };
  const runner = $(".panel.runner");
  const up = u.runner && u.runner.procs;
  set(runner, "runner-cpu", up ? fmtCPU(u.runner.cpu) : "—");
  set(runner, "runner-rss", up ? fmtBytes(u.runner.rss) : "—");
  const card = $("#u-job");
  const j = u.job;
  if (card && j && card.dataset.job === j.job_id) {
    set(card, "cpu", fmtCPU(j.current.cpu));
    set(card, "rss", fmtBytes(j.current.rss));
    set(card, "avg", fmtCPU(j.summary.avg_cpu));
    set(card, "peak", fmtBytes(j.summary.peak_rss));
    set(card, "peakcpu", fmtCPU(j.summary.peak_cpu));
    set(card, "procs", j.current.procs);
    const pts = j.history || [];
    const top = Math.max(100, ...pts.map((p) => p.cpu));
    const n = pts.length, W = 120, H = 22;
    const line = pts.map((p, i) => ((n > 1 ? i / (n - 1) : 1) * W).toFixed(1) + "," + (H - (p.cpu / top) * (H - 2) - 1).toFixed(1)).join(" ");
    $("[data-u=spark] polyline", card)?.setAttribute("points", line);
    $("[data-u=spark] polygon", card)?.setAttribute("points", n ? `0,${H} ${line} ${W},${H}` : "");
  }
  if (j) {
    const id = CSS.escape(j.job_id);
    const c = $(`[data-cpu-job="${id}"]`), m = $(`[data-mem-job="${id}"]`);
    if (c) c.textContent = fmtCPU(j.summary.avg_cpu);
    if (m) m.textContent = fmtBytes(j.summary.peak_rss);
  }
  return !!j;
}

let usageTimer;
async function pollUsage() {
  clearTimeout(usageTimer);
  let busy = !!$("#u-job");
  if (document.visibilityState === "visible") {
    try {
      const res = await fetch("/api/usage", { credentials: "same-origin", cache: "no-store" });
      if (res.ok) busy = renderUsage(await res.json());
    } catch {}
  }
  usageTimer = setTimeout(pollUsage, busy ? 2000 : 10000);
}
document.addEventListener("visibilitychange", () => { if (document.visibilityState === "visible") pollUsage(); });

// ---------- boot ----------
readURL();
initTheme();
initControls();
const rendered = $("#jobs table")?.classList.contains("list") ? "list" : "grouped";
writeURL(false); // canonicalize "/" to "/grouped" etc. without a history entry
syncControls();
afterRender();
connect();
pollUsage();
if ($("#jobs table") && rendered !== state.view) refresh();
