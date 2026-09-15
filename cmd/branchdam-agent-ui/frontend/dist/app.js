// Vanilla JS, no bundler, no framework -- see the PR description for why:
// this window's only job today is a read-only status view (Track 3c of the
// distribution/UX plan), and Wails v2 injects the window.go.* binding glue
// itself at page-load time, so a build step buys nothing here.

const POLL_INTERVAL_MS = 5000;

function byId(id) {
  return document.getElementById(id);
}

// escapeHtml is the default for every value this file puts into innerHTML.
// Card/watch-directory paths and every *.Err message ultimately come from
// the local filesystem or from Go error strings -- neither is guaranteed
// free of '<' (a legal character in an APFS/ext4 filename, even though an
// exFAT card can't produce one), so nothing here is safe to interpolate
// unescaped. See raw() below for the few cases that intentionally contain
// already-safe, self-escaped HTML (pill() output) rather than plain text.
function escapeHtml(s) {
  return String(s)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#39;");
}

// raw marks a string as already-safe HTML (built exclusively from pill()
// calls and/or static text), so table()'s cell renderer passes it through
// instead of escaping it a second time.
function raw(html) {
  return { __raw: html };
}

function cell(v) {
  if (v && typeof v === "object" && "__raw" in v) return v.__raw;
  if (v === undefined || v === null) return "—";
  return escapeHtml(v);
}

function fmtBytes(n) {
  if (n === undefined || n === null) return "—";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v < 10 && i > 0 ? 1 : 0)} ${units[i]}`;
}

function fmtTime(iso) {
  if (!iso) return null;
  const d = new Date(iso);
  if (isNaN(d.getTime()) || d.getFullYear() <= 1) return null;
  return d.toLocaleString();
}

// pill escapes text itself -- every call site below passes plain text
// (an error message, a status word, a version string), never pre-built
// HTML, so this is the one place that needs to get escaping right for
// all of them.
function pill(text, kind) {
  return `<span class="pill ${kind}">${escapeHtml(text)}</span>`;
}

function renderServer(status) {
  const rows = [];
  if (!status.hasDrained) {
    rows.push(["Status", raw(pill("unknown — never drained", "neutral"))]);
  } else if (status.handshakeOk) {
    rows.push(["Status", raw(pill("reachable", "ok"))]);
  } else {
    rows.push(["Status", raw(pill("unreachable", "bad"))]);
  }
  const lastHandshake = fmtTime(status.lastHandshakeAt);
  if (lastHandshake) rows.push(["Last successful handshake", lastHandshake]);
  rows.push(["Paused", raw(status.paused ? pill("yes", "bad") : pill("no", "ok"))]);
  byId("server-body").innerHTML = table(rows);
}

function renderIngest(status) {
  if (status.busy) {
    const since = fmtTime(status.busySince);
    let html = `<p>${pill("running", "ok")} ${escapeHtml(status.busyCard ?? "")}${since ? " since " + escapeHtml(since) : ""}</p>`;
    const p = status.ingestProgress;
    if (p) {
      html += table([
        ["File", p.path],
        ["Phase", p.phase],
        ["Progress", p.totalBytes ? `${fmtBytes(p.bytesDone)} / ${fmtBytes(p.totalBytes)}` : fmtBytes(p.bytesDone)],
      ]);
    }
    byId("ingest-body").innerHTML = html;
    return;
  }
  const last = status.lastIngest;
  if (!last) {
    byId("ingest-body").innerHTML = `<p class="empty">No ingest has run this session.</p>`;
    return;
  }
  // IngestSummary has no json tags of its own (unlike Status's fields),
  // so it marshals with its Go field names as-is -- PascalCase, not
  // camelCase like everything else in this section.
  const rows = [
    ["Card", last.CardPath],
    ["Started", fmtTime(last.StartedAt) ?? "—"],
    ["Submitted / Skipped / Failed", `${last.Submitted ?? 0} / ${last.Skipped ?? 0} / ${last.Failed ?? 0}`],
  ];
  if (last.Offline) rows.push(["Mode", raw(pill("offline queue", "neutral"))]);
  if (last.Err) rows.push(["Error", raw(pill(last.Err, "bad"))]);
  byId("ingest-body").innerHTML = table(rows);
}

function renderQueue(status) {
  const q = status.queueStatus;
  if (!q || !q.Configured) {
    byId("queue-body").innerHTML = `<p class="empty">Offline queue is not configured.</p>`;
    return;
  }
  const c = q.Counts ?? {};
  const rows = [
    ["Awaiting upload", c.AwaitingUpload ?? 0],
    ["Awaiting rebase", c.AwaitingRebase ?? 0],
    ["Failed", c.Failed ?? 0],
    ["Done", c.Done ?? 0],
    ["Pending bytes", fmtBytes(c.PendingBytes)],
  ];
  if (q.Err) rows.push(["Error", raw(pill(q.Err, "bad"))]);
  let html = table(rows);
  if (status.inFlightDrain) html += `<p>${pill("drain running", "neutral")}</p>`;
  if (status.inFlightPrune) html += `<p>${pill("prune running", "neutral")}</p>`;
  byId("queue-body").innerHTML = html;
}

function renderWatch(status) {
  const dirs = status.watchDirs ?? [];
  let html = dirs.length
    ? `<ul>${dirs.map((d) => `<li>${escapeHtml(d)}</li>`).join("")}</ul>`
    : `<p class="empty">No watch directories configured.</p>`;
  if (status.scratchNote) html += `<p>${escapeHtml(status.scratchNote)}</p>`;
  byId("watch-body").innerHTML = html;
}

function renderIntegrations(status) {
  const list = status.integrations ?? [];
  if (!list.length) {
    byId("integrations-body").innerHTML = `<p class="empty">No integrations registered.</p>`;
    return;
  }
  const rows = list.map((i) => {
    const sync = i.LastSync;
    const label = i.Registered ? pill("registered", "ok") : pill("not configured", "neutral");
    let detail = "no sync run yet";
    if (sync) {
      const emitted = `${fmtTime(sync.At) ?? ""} — emitted ${sync.Emitted ?? 0}${sync.DryRun ? " (dry run)" : ""}`;
      detail = escapeHtml(emitted);
      if (sync.Err) detail += " — " + pill(sync.Err, "bad");
    } else {
      detail = escapeHtml(detail);
    }
    return [i.ID, raw(`${label} ${detail}`)];
  });
  byId("integrations-body").innerHTML = table(rows);
}

function renderHooks(status) {
  const list = status.hooks ?? [];
  if (!list.length) {
    byId("hooks-body").innerHTML = `<p class="empty">No hooks registered.</p>`;
    return;
  }
  const rows = list.map((h) => {
    const st = h.State;
    let label = pill("not installed", "neutral");
    if (st && st.Installed) label = st.UpToDate ? pill("up to date", "ok") : pill("installed, out of date", "bad");
    if (st && st.Err) label += " " + pill(st.Err, "bad");
    return [h.ID, raw(label)];
  });
  byId("hooks-body").innerHTML = table(rows);
}

function renderSelfUpdate(status) {
  const su = status.selfUpdate;
  if (!su) {
    byId("selfupdate-body").innerHTML = `<p class="empty">No status available.</p>`;
    return;
  }
  if (!su.Enabled) {
    byId("selfupdate-body").innerHTML = `<p>${pill("disabled", "neutral")}</p>`;
    return;
  }
  const rows = [["Current version", su.CurrentVersion]];
  if (su.Unavailable) {
    rows.push(["Status", raw(pill("unavailable (non-release build)", "neutral"))]);
  } else if (su.UpdateFound) {
    rows.push(["Status", raw(pill(`update available: ${su.LatestVersion}`, "bad"))]);
  } else if (su.Checked) {
    rows.push(["Status", raw(pill("up to date", "ok"))]);
  }
  if (su.Applied) rows.push(["Applied this session", su.Applied]);
  if (su.Err) rows.push(["Error", raw(pill(su.Err, "bad"))]);
  byId("selfupdate-body").innerHTML = table(rows);
}

function table(rows) {
  return `<table>${rows
    .map(([label, value]) => `<tr><td class="label">${cell(label)}</td><td>${cell(value)}</td></tr>`)
    .join("")}</table>`;
}

function render(view) {
  byId("version").textContent = view.version ? `v${view.version}` : "";
  const status = view.status ?? {};
  renderServer(status);
  renderIngest(status);
  renderQueue(status);
  renderWatch(status);
  renderIntegrations(status);
  renderHooks(status);
  renderSelfUpdate(status);
}

function showError(message) {
  const el = byId("error-banner");
  el.textContent = message;
  el.classList.add("visible");
}

function clearError() {
  byId("error-banner").classList.remove("visible");
}

function getApp() {
  return window.go && window.go.main && window.go.main.App;
}

async function poll() {
  const app = getApp();
  if (!app) {
    showError("Connecting...");
    return;
  }
  try {
    const json = await app.StatusJSON();
    clearError();
    render(JSON.parse(json));
  } catch (err) {
    showError(String(err));
  }
}

poll();
setInterval(poll, POLL_INTERVAL_MS);
