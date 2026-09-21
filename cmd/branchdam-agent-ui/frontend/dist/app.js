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

// FIELD_LABELS gives every dotted config key missingRequiredFields
// (cmd/branchdam-agent/settings.go) can produce a human label, for
// renderSetupBanner below and for the "required" markers on the Settings
// form fields further down this file. Kept as one map, not scattered
// across SERVER_IDENTITY_FIELDS/STORAGE_NAMING_FIELDS' own `label`s, since
// renderSetupBanner needs it before those field descriptors are declared.
const FIELD_LABELS = {
  "server.baseUrl": "Server URL",
  "server.apiKey": "API key", // pragma: allowlist secret -- a UI label, not a credential
  agentId: "Agent ID",
  "ingest.localEditRoot": "Local edit root",
  "ingest.archiveRoot": "Archive root",
  pathMappings: "Path mappings",
};

// renderSetupBanner names exactly which config fields are still missing --
// status.missingFields is the same list cmd/branchdam-agent/tray.go's
// startup log line already prints to agent.log, surfaced here instead of
// making the operator go find that file. Rebuilt on every 5s poll like
// every other render* function below (NOT "-config": see
// TestStatusPollNeverRebuildsSettingsContainers, which pins that render()
// must never touch a settings container the once-only loadSettings() owns).
function renderSetupBanner(status) {
  const el = byId("setup-banner");
  if (!status.configIncomplete || !(status.missingFields ?? []).length) {
    el.innerHTML = "";
    el.classList.remove("visible");
    return;
  }
  const items = status.missingFields
    .map((f) => `<li>${escapeHtml(FIELD_LABELS[f] ?? f)} <code>${escapeHtml(f)}</code></li>`)
    .join("");
  el.innerHTML = `<p>Setup isn't finished yet — the sections below still need:</p><ul>${items}</ul>`;
  el.classList.add("visible");
}

// renderServer keys the Status pill on status.serverProbe -- an on-demand
// (and, per cmd/branchdam-agent's startup/60s-timer wiring, automatic)
// POST /api/v1/agent/hello check -- rather than the OLD status.hasDrained/
// handshakeOk pair, which could only ever become true once
// offline.queueDbPath was ALSO configured (a real drain pass is the only
// thing that ever set them). That's the "unknown — never drained" bug this
// exists to fix: a config with server.baseUrl/apiKey/agentId set and no
// offline queue at all used to show a permanently-unknown Server card.
//
// Deliberately NOT gated on status.configIncomplete the same way
// TriggerServerProbe itself deliberately isn't (see that method's own doc
// comment): an operator who has filled in the server fields but not yet
// ingest.localEditRoot/archiveRoot is still configIncomplete=true, and
// hiding a real "reachable" behind a blanket "not configured" here would
// undo the exact fix TriggerServerProbe's independence from the offline
// queue was for.
function renderServer(status) {
  const rows = [];
  const probe = status.serverProbe;
  if (!probe) {
    const needsServerFields = (status.missingFields ?? []).some((f) => f.startsWith("server.") || f === "agentId");
    const hint = needsServerFields ? "not checked — set Server URL, API key and Agent ID" : "not checked yet";
    rows.push(["Status", raw(pill(hint, "neutral"))]);
  } else if (probe.ok) {
    rows.push(["Status", raw(pill("reachable", "ok"))]);
    if (probe.version) rows.push(["Server version", probe.version]);
  } else {
    rows.push(["Status", raw(pill("unreachable", "bad"))]);
    if (probe.err) rows.push(["Error", raw(pill(probe.err, "bad"))]);
  }
  const lastHandshake = fmtTime(status.lastHandshakeAt);
  if (lastHandshake) rows.push(["Last successful handshake", lastHandshake]);
  rows.push(["Paused", raw(status.paused ? pill("yes", "bad") : pill("no", "ok"))]);

  const container = byId("server-body");
  container.innerHTML = table(rows);
  container.appendChild(
    actionButtonRow("Server actions", "", [
      {
        label: "Test connection",
        busyText: "Checking…",
        busy: inFlightActions.has("testConnection"),
        run: async (statusEl) => {
          const app = getApp();
          if (!app) return;
          // Same inFlightActions bookkeeping as renderIntegrations' own
          // "Sync now" -- cleared before branching, not in a finally
          // around the whole handler, so the success path's poll() below
          // never sees this action as still in flight.
          inFlightActions.add("testConnection");
          let result;
          try {
            result = JSON.parse(await app.TestConnection());
          } finally {
            inFlightActions.delete("testConnection");
          }
          if (!result.ran) {
            // No ServerProbe wired at all (server.baseUrl/apiKey/agentId
            // still incomplete) -- poll() would rebuild this row from the
            // same nil status.serverProbe and erase this message with
            // nothing to replace it, so leave it standing.
            setFieldStatus(statusEl, "Set Server URL, API key and Agent ID first", "error");
            return;
          }
          if (!result.ok) {
            setFieldStatus(statusEl, result.err || "unreachable", "error");
            await poll();
            return;
          }
          setFieldStatus(statusEl, result.version ? `Reachable (v${result.version})` : "Reachable", "saved");
          await poll(); // refresh the Status pill above immediately, rather than waiting up to POLL_INTERVAL_MS
        },
      },
      {
        label: "Pair with Server…",
        busyText: "Pairing…",
        busy: inFlightActions.has("pairServer"),
        run: async (statusEl) => {
          const rawURL = window.prompt(
            "Paste the branchdam:// pairing URL from branchDAM's Companion Pairing page:",
          );
          if (!rawURL || !rawURL.trim()) return;

          const app = getApp();
          if (!app) return;

          inFlightActions.add("pairServer");
          let result;
          try {
            result = JSON.parse(await app.Pair(rawURL.trim()));
          } finally {
            inFlightActions.delete("pairServer");
          }

          if (!result.ok) {
            setFieldStatus(statusEl, result.err || "Pairing failed", "error");
            return;
          }
          setFieldStatus(statusEl, `Paired with ${result.server}`, "saved");
          await poll();
          await loadSettings();
        },
      },
    ]),
  );

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
  const rows = [
    ["Card", last.cardPath],
    ["Started", fmtTime(last.startedAt) ?? "—"],
    ["Submitted / Skipped / Failed", `${last.submitted ?? 0} / ${last.skipped ?? 0} / ${last.failed ?? 0}`],
  ];
  if (last.offline) rows.push(["Mode", raw(pill("offline queue", "neutral"))]);
  if (last.err) rows.push(["Error", raw(pill(last.err, "bad"))]);
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

// inFlightActions holds "kind:id" keys (e.g. "sync:luminar") for every
// TriggerSync/TriggerHookInstall call still awaiting its response --
// module-level and shared across renders, not per-row state, because
// renderIntegrations/renderHooks fully rebuild their container's DOM on
// EVERY 5s status poll: without this, a sync slower than 5s would show a
// fresh, clickable "Sync now" button on the next poll tick while the
// original click's request is still in flight server-side (a Hermes
// review finding on PR #216 -- correctness survives either way, since the
// server itself serializes and reports "Skipped -- already running" for a
// concurrent second call, but the busy indicator vanishing was misleading).
const inFlightActions = new Set();

// enabledByID mirrors each integration's CONFIG-side Enabled flag
// (SettingsView.Integrations[].Enabled) for the live-status side
// (renderIntegrations) to consult -- status.integrations carries
// Registered, never Enabled, and plumbing Enabled into Runner.Status()
// would mean threading a config.Config into a struct that's deliberately
// built from nothing but Runner's own runtime maps (tray.go's own
// Status() doc comment). Seeded once in renderSettingsForm from the
// initial SettingsJSON() snapshot, then kept fresh by saveSetting, which
// already receives an up-to-date SettingsView back from every settings
// save and used to discard it. Module-level and never cleared, matching
// inFlightActions' own reasoning above: renderIntegrations rebuilds its
// container on every 5s poll, so this can't be per-render state. Absent
// key (map not yet seeded, e.g. before the first loadSettings() resolves)
// must read as "show the row" -- fail-open, never hide a row because a
// lookup missed.
const enabledByID = new Map();

// refreshEnabledByID updates enabledByID from any SettingsView-shaped
// object that carries an Integrations array -- called from saveSetting
// (every settings save response) and renderSettingsForm (the initial
// load), so the two never drift out of sync with each other.
function refreshEnabledByID(sv) {
  for (const iv of sv?.Integrations ?? []) enabledByID.set(iv.ID, iv.Enabled);
}

// actionButtonRow builds one label/detail/button/status line as real DOM
// (not an innerHTML string, unlike this file's other render* status
// functions) -- the "Sync now"/"Install"/"Reveal" buttons below need a real
// addEventListener, and Wails' default CSP blocks inline onclick handlers,
// matching the settings form's own renderTextField/renderCheckboxField
// precedent for exactly the same reason. A button whose `busy` field is
// already true when the row is (re)built (see inFlightActions above)
// starts disabled and shows its own busyText immediately, rather than
// only reacting to a click on this particular DOM instance.
function actionButtonRow(id, detailHtml, buttons) {
  const row = document.createElement("div");
  // .action-row is a grid-column template distinct from the settings
  // form's .field-row shape (label/marker/input/browse/status) -- this
  // row is label/detail/actions/status instead. See style.css's own doc
  // comment on both classes for why they can't share one template.
  row.className = "field-row action-row";

  const label = document.createElement("label");
  label.textContent = id;
  row.appendChild(label);

  const detail = document.createElement("span");
  detail.className = "row-detail";
  detail.innerHTML = detailHtml;
  row.appendChild(detail);

  const status = document.createElement("span");
  status.className = "field-status";
  const alreadyBusy = buttons.find((b) => b.busy);
  if (alreadyBusy) setFieldStatus(status, alreadyBusy.busyText ?? "Working…");

  // buttons are wrapped in one .row-actions element, occupying a single
  // grid cell -- renderHooks passes TWO buttons (Install, Reveal) into
  // one call here, and a grid can't place multiple same-tag siblings into
  // one named column without a common wrapper. .row-actions also gives
  // the first button (the row's main action) the primary/brand styling
  // in style.css, with no styling logic needed here.
  const actions = document.createElement("span");
  actions.className = "row-actions";
  for (const b of buttons) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.textContent = b.label;
    if (b.disabled || b.busy) btn.disabled = true;
    btn.addEventListener("click", async () => {
      btn.disabled = true;
      setFieldStatus(status, b.busyText ?? "Working…");
      try {
        await b.run(status);
      } catch (err) {
        setFieldStatus(status, String(err), "error");
      } finally {
        btn.disabled = !!b.disabled;
      }
    });
    actions.appendChild(btn);
  }
  row.appendChild(actions);
  row.appendChild(status);
  return row;
}

// renderIntegrations renders live sync status plus a "Sync now" button per
// Integrations() registry entry -- the config fields live in the Settings
// section's renderIntegrationBlock instead. This window is now the ONLY
// place these actions live; the tray's own per-integration submenu
// (internal/tray/integrationsmenu.go) was deleted once this covered
// everything it did except Sync timeout, which renderIntegrationBlock
// gained instead (see its own doc comment). Re-rendered on every 5s status
// poll, same as every other render* function in this file except the
// settings form.
function renderIntegrations(status) {
  const container = byId("integrations-body");
  container.innerHTML = "";
  // enabledByID.get(id) === false is the only case that hides a row --
  // undefined (map not yet seeded) reads as "show it," matching that
  // map's own fail-open doc comment.
  const list = (status.integrations ?? []).filter((i) => enabledByID.get(i.ID) !== false);
  if (!list.length) {
    container.innerHTML = `<p class="empty">No integrations registered.</p>`;
    return;
  }
  for (const i of list) {
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
    const syncKey = `sync:${i.ID}`;
    container.appendChild(
      actionButtonRow(i.Title || i.ID, `${label} ${detail}`, [
        {
          label: "Sync now",
          busyText: "Syncing…",
          disabled: !i.Registered,
          busy: inFlightActions.has(syncKey),
          run: async (statusEl) => {
            const app = getApp();
            if (!app) return;
            // inFlightActions is cleared as soon as the request itself
            // resolves (success, "skipped", or a real error), NOT after
            // the branching below -- the `await poll()` on the success
            // path must see the key already gone, or the rebuild it
            // triggers would still find this action "in flight" and
            // render the fresh row as busy/disabled right after the sync
            // that just finished.
            inFlightActions.add(syncKey);
            let result;
            try {
              result = JSON.parse(await app.TriggerSync(i.ID));
            } finally {
              inFlightActions.delete(syncKey);
            }
            if (!result.ran) {
              // poll() would rebuild this row from LastSync, which a
              // skipped pass never updates -- the message would vanish
              // with nothing to show in its place. Leave it standing
              // until the next natural poll tick instead.
              setFieldStatus(statusEl, "Skipped — already running", "error");
              return;
            }
            if (result.err) {
              // Same reasoning: an errored pass DOES update LastSync.Err,
              // but only the row's small "bad" pill reflects it after a
              // rebuild, not this more prominent status message -- worth
              // leaving standing too, for the same reason.
              setFieldStatus(statusEl, result.err, "error");
              return;
            }
            setFieldStatus(statusEl, `Emitted ${result.emitted ?? 0}`, "saved");
            await poll(); // refresh this row (and everything else) from the new status immediately, rather than waiting up to POLL_INTERVAL_MS
          },
        },
      ]),
    );
  }
}

// renderHooks renders live hook install status plus "Install"/"Reveal"
// buttons per HookDescriptors() registry entry. This window is now the
// ONLY place these actions live; the tray's own per-hook submenu
// (internal/tray/hooksmenu.go) was deleted once this covered everything
// it did.
function renderHooks(status) {
  const container = byId("hooks-body");
  container.innerHTML = "";
  const list = status.hooks ?? [];
  if (!list.length) {
    container.innerHTML = `<p class="empty">No hooks registered.</p>`;
    return;
  }
  for (const h of list) {
    const st = h.State;
    let label = pill("not installed", "neutral");
    if (st && st.Installed) label = st.UpToDate ? pill("up to date", "ok") : pill("installed, out of date", "bad");
    if (st && st.Err) label += " " + pill(st.Err, "bad");
    const installKey = `hookInstall:${h.ID}`;
    container.appendChild(
      actionButtonRow(h.Title || h.ID, label, [
        {
          label: "Install",
          busyText: "Installing…",
          busy: inFlightActions.has(installKey),
          run: async (statusEl) => {
            const app = getApp();
            if (!app) return;
            // See renderIntegrations' own "Sync now" for why the key is
            // cleared before branching, not in a finally around the
            // whole handler -- the success path's poll() must not see
            // this action as still "in flight".
            inFlightActions.add(installKey);
            let result;
            try {
              result = JSON.parse(await app.TriggerHookInstall(h.ID));
            } finally {
              inFlightActions.delete(installKey);
            }
            if (!result.ran) {
              // Same reasoning as renderIntegrations' own "Sync now" --
              // poll() rebuilds this row and would erase a message a
              // skipped/errored pass has nothing to replace it with.
              setFieldStatus(statusEl, "Skipped — already running", "error");
              return;
            }
            if (result.err) {
              setFieldStatus(statusEl, result.err, "error");
              return;
            }
            setFieldStatus(statusEl, "Installed", "saved");
            await poll();
          },
        },
        {
          label: "Reveal",
          busyText: "Opening…",
          run: async (statusEl) => {
            const app = getApp();
            if (!app) return;
            // RevealHook never mutates hook state (Runner.RevealHook's own
            // doc comment), so unlike Install there is no reason to poll()
            // afterward -- nothing in the status view would change.
            await app.RevealHook(h.ID);
            setFieldStatus(statusEl, "Opened", "saved");
          },
        },
      ]),
    );
  }
}

function renderSelfUpdate(status, settings) {
  const su = status.selfUpdate;
  const container = byId("selfupdate-body");
  if (!su) {
    container.innerHTML = `<p class="empty">No status available.</p>`;
    return;
  }
  if (!su.Enabled) {
    container.innerHTML = `<p>${pill("disabled", "neutral")}</p>`;
    return;
  }
  const rows = [["Current version", su.CurrentVersion]];
  if (su.Phase && !["idle", "available"].includes(su.Phase)) {
    rows.push(["Status", raw(pill(su.Phase, su.Phase === "failed" ? "bad" : "neutral"))]);
  } else if (su.Unavailable) {
    rows.push(["Status", raw(pill("unavailable (non-release build)", "neutral"))]);
  } else if (su.UpdateFound) {
    rows.push(["Status", raw(pill(`update available: ${su.LatestVersion}`, "bad"))]);
  } else if (su.Checked) {
    rows.push(["Status", raw(pill("up to date", "ok"))]);
  }
  if (su.Applied) rows.push(["Applied this session", su.Applied]);
  if (su.Err) rows.push(["Error", raw(pill(su.Err, "bad"))]);
  container.innerHTML = table(rows);

  // Unknown non-terminal phases fail closed too, so a newly added Go phase
  // cannot accidentally re-enable the destructive button in an older UI.
  const applyInFlight = !!su.Phase && !["idle", "available", "failed"].includes(su.Phase);
  container.appendChild(
    actionButtonRow("Update check", "", [
      {
        label: "Check now",
        busyText: "Checking…",
        disabled: !!su.Unavailable,
        busy: inFlightActions.has("checkUpdate"),
        run: async (statusEl) => {
          const app = getApp();
          if (!app) return;
          // Same inFlightActions bookkeeping as renderServer's own "Test
          // connection" -- cleared before branching, not in a finally
          // around the whole handler, so the success path's poll() below
          // never sees this action as still in flight.
          inFlightActions.add("checkUpdate");
          let result;
          try {
            result = JSON.parse(await app.CheckForUpdate());
          } finally {
            inFlightActions.delete("checkUpdate");
          }
          if (!result.ran) {
            // Disabled, a non-semver build, or a check already in flight
            // (the interval ticker firing at the same moment) -- none are
            // errors, and poll() would just rebuild this row from the
            // unchanged status with nothing new to show, so leave this
            // message standing instead.
            setFieldStatus(statusEl, "Check already running or self-update is off", "error");
            return;
          }
          const s = result.status;
          if (s.Err) {
            setFieldStatus(statusEl, s.Err, "error");
          } else if (s.UpdateFound) {
            setFieldStatus(statusEl, `Update available: ${s.LatestVersion}`, "saved");
          } else {
            setFieldStatus(statusEl, "Up to date", "saved");
          }
          await poll(); // refresh the table above immediately, rather than waiting up to POLL_INTERVAL_MS
        },
      },
      {
        label: "Install and restart",
        busyText: "Installing…",
        disabled: !su.UpdateFound || !!su.Unavailable || applyInFlight,
        busy: inFlightActions.has("applyUpdate"),
        run: async (statusEl) => {
          const app = getApp();
          if (!app) return;
          // Match the tray menu's destructive-action setting. If settings
          // are unavailable, keep the confirmation (fail closed). Wails' Go
          // binding uses the native question dialog; browser confirm() is
          // not reliable in the macOS WKWebView.
          if (!settings || settings.ConfirmDestructive !== false) {
            let confirmed;
            try {
              confirmed = await app.ConfirmApplyUpdate(su.LatestVersion);
            } catch (err) {
              setFieldStatus(statusEl, String(err), "error");
              return;
            }
            if (!confirmed) return;
          }
          inFlightActions.add("applyUpdate");
          let result;
          try {
            result = JSON.parse(await app.ApplyUpdate());
          } finally {
            inFlightActions.delete("applyUpdate");
          }
          if (!result.started) {
            setFieldStatus(statusEl, result.status?.Err || "Update could not be started", "error");
            return;
          }
          setFieldStatus(statusEl, "Update started — watch status above", "saved");
          await poll();
        },
      },
    ]),
  );
}

function table(rows) {
  return `<table>${rows
    .map(([label, value]) => `<tr><td class="label">${cell(label)}</td><td>${cell(value)}</td></tr>`)
    .join("")}</table>`;
}

// uiVersion is this window's OWN binary version (App.Version), distinct
// from view.version below (the AGENT's own reported version) -- fetched
// once at startup, not on the status poll, since it can't change during a
// window's lifetime. The two normally match (they ship together), but can
// briefly disagree if a self-update fails partway -- see main.go's
// version var doc comment.
let uiVersion = null;

async function loadUIVersion() {
  const app = getApp();
  if (!app) {
    setTimeout(loadUIVersion, 500);
    return;
  }
  try {
    uiVersion = await app.Version();
  } catch {
    // Cosmetic only -- an older UI binary predating this method, or any
    // other failure, shouldn't block the rest of the page from rendering.
  }
}
loadUIVersion();

function render(view) {
  const agentVersion = view.version ? `agent v${view.version}` : "";
  const shownUIVersion = uiVersion && uiVersion !== view.version ? `UI v${uiVersion}` : "";
  byId("version").textContent = [agentVersion, shownUIVersion].filter(Boolean).join(" · ");
  const status = view.status ?? {};
  renderSetupBanner(status);
  renderServer(status);
  renderIngest(status);
  renderQueue(status);
  renderWatch(status);
  renderIntegrations(status);
  renderHooks(status);
  renderSelfUpdate(status, view.settings);
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

// --- Category navigation ---------------------------------------------
//
// The nav shows one category panel at a time by toggling a CSS class.
// Every panel stays in the DOM at all times, hidden or not -- that is
// load-bearing, not incidental: render(view) (5s poll, above) and
// renderSettingsForm (once, at load, below) keep writing into their
// containers exactly as before, with no awareness of which panel is on
// screen.
//
// Nothing here may ever re-render a panel's contents. "Only render the
// visible panel" would mean calling renderSettingsForm on every category
// switch -- precisely the mid-typing-clobber bug
// TestStatusPollNeverRebuildsSettingsContainers exists to prevent,
// reintroduced through a second door. TestCategorySwitchingNeverRerenders
// keeps that door shut too.
function showCategory(panelId) {
  const panel = byId(panelId);
  if (!panel) return; // a nav button naming a nonexistent panel must not blank the window
  for (const p of document.querySelectorAll("#content .panel")) {
    p.classList.toggle("active", p === panel);
  }
  for (const b of document.querySelectorAll("#nav .nav-item")) {
    const on = b.dataset.panel === panelId;
    b.classList.toggle("active", on);
    if (on) b.setAttribute("aria-current", "page");
    else b.removeAttribute("aria-current");
  }
  byId("content").scrollTop = 0;
}

function initNav() {
  // Real addEventListener, not inline onclick: Wails' default CSP blocks
  // inline handlers -- same reason actionButtonRow/renderTextField build
  // real DOM nodes instead.
  for (const b of document.querySelectorAll("#nav .nav-item")) {
    b.addEventListener("click", () => showCategory(b.dataset.panel));
  }
  // No persistence across window reopens -- always defaults to whichever
  // panel index.html marks "active" (Server). Operator's own call: simpler,
  // and keeps steering a first-run operator back to setup rather than
  // wherever they last clicked.
}
initNav();

// --- Settings form ---------------------------------------------------
//
// Track 3d: every free-text field the tray's zenity-backed PromptAndSet
// dialog used to own becomes a plain form field here, saved via
// app.SetSetting/SetIntegrationPath/SetIntegrationRewrites -- see this
// package's app.go for why those are bound Go methods rather than plain
// fetch() calls (the agent's session token never reaches this JS context
// at all).
//
// Loaded once at startup, not on the 5s status poll: re-rendering the
// whole form on every poll tick would blow away whatever an operator is
// mid-typing. No field re-fetches from the agent on save -- a text field
// keeps whatever was typed either way, so a rejected save can be corrected
// and resubmitted without retyping; a checkbox or <select>, which has no
// "keep editing" state, instead reverts to its pre-change value on a
// failed save (renderCheckboxField/renderSelectField) rather than staying
// visibly flipped until the window reloads.

// The settings form used to be one flat list under a single "Settings"
// section, dead last in the window (issue: "settings menu is flat...
// confusing" / "settings order also confusing"). It's now split into
// named groups, each its own top-level section ahead of live status --
// see index.html's config zone. The field descriptors themselves are
// unchanged; only their grouping and target containers moved.

// SERVER_IDENTITY_FIELDS: how this agent reaches the branchDAM server and
// identifies itself.
const SERVER_IDENTITY_FIELDS = [
  { key: "server.baseUrl", label: "Server URL", get: (sv) => sv.ServerBaseURL, required: true },
  {
    key: "server.apiKey",
    label: "API key",
    password: true,
    get: () => "",
    placeholder: (sv) => (sv.ServerAPIKeySet ? "(configured — leave blank to keep)" : "(not set)"),
    required: true,
  },
  { key: "agentId", label: "Agent ID", get: (sv) => sv.AgentID, required: true },
];

// STORAGE_NAMING_FIELDS: where files land and how they're named. `list:
// true` means the value round-trips as a JSON array of strings
// (SetStringSlice, the canonical wire shape for a list per statusapi.go's
// settingsPatchRequest doc comment), not a comma-separated string.

// requiredUnlessDirectUpload documents the one conditional requirement
// missingRequiredFields (cmd/branchdam-agent/settings.go) actually applies:
// ingest.archiveRoot and pathMappings are only required when
// ingest.uploadStream is false, or when it's true but an offline queue is
// also configured -- see config.example.yaml's own ingest.uploadStream
// comment. Neither of those two conditions is visible to this window today
// (uploadStream/offline.queueDbPath are both hand-edit-only config keys,
// per docs/tray-settings-inventory.md), so the marker states the rule
// rather than evaluating it live.
const requiredUnlessDirectUpload = "required unless direct-upload mode is on with no offline queue";

const STORAGE_NAMING_FIELDS = [
  { key: "ingest.archiveRoot", label: "Archive root", get: (sv) => sv.ArchiveRoot, browseDir: true, requiredNote: requiredUnlessDirectUpload },
  { key: "ingest.localEditRoot", label: "Local edit root", get: (sv) => sv.LocalEditRoot, browseDir: true, required: true },
  {
    key: "ingest.cardRoots",
    label: "Watch folders",
    kind: "folderList",
    get: (sv) => sv.CardRoots ?? [],
    emptyNote: "No watch folders — card auto-detection is off.",
  },
  {
    key: "ingest.allowedExtensions",
    label: "Allowed extensions",
    kind: "chipList",
    get: (sv) => sv.AllowedExtensions ?? [],
  },
  {
    key: "ingest.pathTemplate",
    label: "Naming template",
    kind: "readonly",
    get: (sv) => sv.NamingTemplate,
    // AGENTS.md invariant 17(d): overwritten from the server's handshake
    // response at tray startup and on every reload whenever the server
    // returns a non-empty value, which -- per the server's own agent-
    // handshake handler -- it always does. The overwrite is in-memory
    // only (an operator's own edit survives in config.yaml but is
    // shadowed on the very next reload), so editing this field here would
    // silently "succeed" and then revert.
    note: "Synced from the branchDAM server on every handshake — local edits do not persist.",
  },
  {
    key: "pathMappings",
    label: "Path mappings",
    kind: "pathMappings",
    get: (sv) => sv.PathMappingEntries ?? [],
    requiredNote: requiredUnlessDirectUpload,
    note:
      "Translates paths this agent writes into the container paths branchDAM sees, before an event is sent. " +
      "Separate from the server's own Operator Path Rewrites, which resolve references inside project files. " +
      "Set locally only -- the server never supplies these on handshake.",
  },
  { key: "integrations.nodeIndexPath", label: "Node index path", get: (sv) => sv.NodeIndexPath, browseFile: ["*.json"] },
];

// BEHAVIOR_FIELDS: toggles that change how the agent acts, not what it
// connects to or where it writes.
const BEHAVIOR_FIELDS = [
  { key: "tray.startOnLogin", label: "Start on login", get: (sv) => sv.StartOnLogin },
  { key: "tray.confirmDestructive", label: "Confirm destructive actions", get: (sv) => sv.ConfirmDestructive },
  { key: "ingest.requireUnbuffered", label: "Require unbuffered writes", get: (sv) => sv.RequireUnbuffered },
  { key: "ingest.requireDCIM", label: "Require DCIM folder", get: (sv) => sv.RequireDCIM },
  { key: "ingest.pauseUploadOnMetered", label: "Pause upload on metered connection", get: (sv) => sv.PauseUploadOnMetered },
  { key: "ingest.autoEject", label: "Auto-eject after successful ingest", get: (sv) => sv.AutoEject },
];

function setFieldStatus(el, text, kind) {
  el.textContent = text;
  el.className = "field-status" + (kind ? " " + kind : "");
  if (kind === "saved") {
    setTimeout(() => {
      if (el.textContent === text) {
        el.textContent = "";
        el.className = "field-status";
      }
    }, 2000);
  }
}

// saveSetting drives the common "save one key, re-render nothing but this
// field's own status" path every plain (non-integration) field uses.
// saveSetting returns whether the save actually succeeded -- callers that
// need to react differently on failure (clearing a password field only on
// success, reverting a checkbox/select to its prior value on failure)
// check this instead of assuming the call always "took."
async function saveSetting(key, value, statusEl) {
  const app = getApp();
  if (!app) return false;
  setFieldStatus(statusEl, "Saving…");
  try {
    const body = await app.SetSetting(key, value);
    // Every settings route already returns the freshly recomputed
    // SettingsView (statusapi.go ends each handler with
    // s.Settings.Snapshot()) -- every caller used to just discard it.
    // Parsing it here, once, lets enabledByID stay current after ANY
    // save (not just an Enabled checkbox toggle) with no per-call-site
    // plumbing; a body that doesn't parse as JSON is defensive-only
    // (SetSetting's own contract guarantees valid JSON on success) and
    // silently skipped rather than surfaced as a save failure.
    try {
      refreshEnabledByID(JSON.parse(body));
    } catch {
      /* see comment above */
    }
    setFieldStatus(statusEl, "Saved", "saved");
    return true;
  } catch (err) {
    setFieldStatus(statusEl, String(err), "error");
    return false;
  }
}

function renderTextField(f, sv) {
  const row = document.createElement("div");
  row.className = "field-row";

  const label = document.createElement("label");
  label.textContent = f.label;
  row.appendChild(label);

  // f.required (always) vs. f.requiredNote (conditionally, e.g.
  // ingest.archiveRoot's "unless direct-upload mode..." -- see
  // requiredUnlessDirectUpload above) are both surfaced the same way: a
  // "*" marker whose title carries the exact condition, so a static
  // render (this form loads once via loadSettings(), see its own doc
  // comment) still communicates the conditional case without evaluating
  // uploadStream/offline.queueDbPath live.
  if (f.required || f.requiredNote) {
    const marker = document.createElement("span");
    marker.className = "req";
    marker.textContent = "*";
    marker.title = f.requiredNote ?? "required";
    row.appendChild(marker);
  }

  const input = document.createElement("input");
  input.type = f.password ? "password" : "text";
  input.value = f.get(sv) ?? "";
  if (f.placeholder) input.placeholder = f.placeholder(sv);
  row.appendChild(input);

  const status = document.createElement("span");
  status.className = "field-status";

  // commit is the one save path, called both from a manual edit's "change"
  // event and from a successful Browse pick -- assigning input.value
  // programmatically does NOT fire "change" on its own (and clears the
  // element's dirty flag), so without this explicit call, a Browse pick
  // used to fill the box visually and then silently never reach
  // config.yaml unless the operator also typed into the field by hand.
  function commit() {
    if (f.password && input.value === "") return; // blank means "leave unchanged"
    saveSetting(f.key, input.value, status).then((ok) => {
      // Only clear a typed secret once it's actually saved -- a rejected
      // save must leave it in the field, or retrying means retyping the
      // whole key from scratch (Hermes review finding on this PR).
      if (f.password && ok) {
        input.value = "";
        // The placeholder was computed once at render time from the
        // load-time snapshot; after a successful save the "set" state it
        // reflects is stale until the window reloads (Hermes review
        // finding on this PR). A password field that just saved
        // successfully has necessarily transitioned to "set" -- recompute
        // from a minimal synthetic snapshot instead of a full reload.
        if (f.placeholder) input.placeholder = f.placeholder({ ServerAPIKeySet: true });
      }
    });
  }

  if (f.browseDir || f.browseFile) {
    const browse = document.createElement("button");
    browse.type = "button";
    browse.textContent = "Browse…";
    browse.addEventListener("click", async () => {
      const app = getApp();
      if (!app) return;
      try {
        const picked = f.browseDir ? await app.PickDirectory(f.label) : await app.PickFile(f.label, f.browseFile);
        if (picked) {
          input.value = picked;
          commit();
        }
      } catch (err) {
        setFieldStatus(status, String(err), "error");
      }
    });
    row.appendChild(browse);
  }

  input.addEventListener("change", commit);

  row.appendChild(status);
  return row;
}

// fieldNote renders f.note (if present) as a small muted line under a
// field row -- shared by renderReadOnlyField and renderPathMappingField,
// both of which carry static explanatory text a bare label can't.
function fieldNote(text) {
  const note = document.createElement("p");
  note.className = "field-note";
  note.textContent = text;
  return note;
}

// renderReadOnlyField renders a field the operator can see but not edit
// here -- ingest.pathTemplate's own descriptor is the only caller today
// (see its "note" field for why). readOnly rather than disabled: a
// disabled input drops out of the accessibility tree and can't be
// selected/copied, neither of which is true of a value that's merely not
// editABLE from this window.
function renderReadOnlyField(f, sv) {
  const wrap = document.createElement("div");

  const row = document.createElement("div");
  row.className = "field-row";
  const label = document.createElement("label");
  label.textContent = f.label;
  row.appendChild(label);
  const input = document.createElement("input");
  input.type = "text";
  input.readOnly = true;
  input.className = "readonly";
  input.value = f.get(sv) ?? "";
  row.appendChild(input);
  wrap.appendChild(row);

  if (f.note) wrap.appendChild(fieldNote(f.note));
  return wrap;
}

// renderFolderListField backs "Watch folders" (ingest.cardRoots): a
// repeatable single-directory picker rather than a comma-separated text
// box with no way to see what's configured. Wails v2 has no multi-select
// directory dialog, so "pick one folder, repeat" is the design here, not
// a fallback -- each row is still a plain text input too (not read-only),
// since cardRoots also supports ${VAR} expansion (e.g. "/media/${USER}"),
// a value a directory picker can never produce.
function renderFolderListField(f, sv) {
  const wrap = document.createElement("div");
  const rows = document.createElement("div");
  wrap.appendChild(rows);

  let values = [...(f.get(sv) ?? [])];
  const status = document.createElement("span");
  status.className = "field-status";

  function commit() {
    saveSetting(
      f.key,
      values.map((v) => v.trim()).filter((v) => v.length > 0),
      status,
    );
  }

  function render() {
    rows.innerHTML = "";

    if (values.length === 0) {
      const empty = document.createElement("p");
      empty.className = "empty";
      empty.textContent = f.emptyNote ?? "";
      rows.appendChild(empty);
    }

    values.forEach((val, i) => {
      const row = document.createElement("div");
      row.className = "field-row";
      const label = document.createElement("label");
      label.textContent = i === 0 ? f.label : "";
      row.appendChild(label);

      const input = document.createElement("input");
      input.type = "text";
      input.value = val;
      input.addEventListener("change", () => {
        values[i] = input.value;
        commit();
      });
      row.appendChild(input);

      const remove = document.createElement("button");
      remove.type = "button";
      remove.textContent = "Remove";
      remove.addEventListener("click", () => {
        values.splice(i, 1);
        render();
        commit();
      });
      row.appendChild(remove);

      if (i === values.length - 1) row.appendChild(status);
      rows.appendChild(row);
    });

    const addRow = document.createElement("div");
    addRow.className = "field-row";
    const addLabel = document.createElement("label");
    addLabel.textContent = values.length === 0 ? f.label : "";
    addRow.appendChild(addLabel);
    const addBtn = document.createElement("button");
    addBtn.type = "button";
    addBtn.textContent = "Add folder…";
    addBtn.addEventListener("click", async () => {
      const app = getApp();
      if (!app) return;
      try {
        const picked = await app.PickDirectory(f.label);
        if (picked) {
          values.push(picked);
          render();
          commit();
        }
      } catch (err) {
        setFieldStatus(status, String(err), "error");
      }
    });
    addRow.appendChild(addBtn);
    if (values.length === 0) addRow.appendChild(status);
    rows.appendChild(addRow);
  }

  render();
  return wrap;
}

// renderChipListField backs "Allowed extensions": removable chips plus a
// free-text input that commits a token on Enter, comma, or blur, saving
// through the same SetStringSlice wire path the old comma-separated
// input used. Client-side normalization (lowercased, leading dot added if
// missing) mirrors the server-side check in
// validateStringSliceChange/splitCommaExtensions.
function renderChipListField(f, sv) {
  const row = document.createElement("div");
  row.className = "field-row";

  const label = document.createElement("label");
  label.textContent = f.label;
  row.appendChild(label);

  const chipBox = document.createElement("div");
  chipBox.className = "chip-box";
  row.appendChild(chipBox);

  const status = document.createElement("span");
  status.className = "field-status";
  row.appendChild(status);

  // normalizeExtension mirrors the server-side rule (splitCommaExtensions
  // / validateStringSliceChange's own leading-dot check): lowercase, with
  // a leading dot added if missing. Applied to the LOADED values too, not
  // just newly-typed ones (Hermes review finding on the PR that added
  // this editor) -- a dotless extension hand-written into config.yaml
  // would otherwise render as a chip, then fail the very next save (the
  // whole array re-sent through SetStringSlice, which rejects it) with no
  // way to tell from the chip alone which entry was the problem.
  function normalizeExtension(v) {
    v = v.trim().toLowerCase();
    if (v && !v.startsWith(".")) v = "." + v;
    return v;
  }

  let values = [...new Set((f.get(sv) ?? []).map(normalizeExtension).filter(Boolean))];

  function commit() {
    saveSetting(f.key, values, status);
  }

  function render() {
    chipBox.innerHTML = "";
    values.forEach((val, i) => {
      const chip = document.createElement("span");
      chip.className = "chip";
      chip.textContent = val;
      const remove = document.createElement("button");
      remove.type = "button";
      remove.textContent = "×";
      remove.setAttribute("aria-label", `Remove ${val}`);
      remove.addEventListener("click", () => {
        values.splice(i, 1);
        render();
        commit();
      });
      chip.appendChild(remove);
      chipBox.appendChild(chip);
    });

    const input = document.createElement("input");
    input.type = "text";
    input.placeholder = values.length === 0 ? "e.g. .jpg" : "Add…";
    function commitToken() {
      const v = normalizeExtension(input.value);
      input.value = "";
      if (!v) return;
      if (!values.includes(v)) {
        values.push(v);
        render();
        commit();
      }
    }
    input.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === ",") {
        e.preventDefault();
        commitToken();
      }
    });
    input.addEventListener("blur", commitToken);
    chipBox.appendChild(input);
  }

  render();
  return row;
}

// renderPathMappingField backs "Path mappings": a structured from->to row
// editor saving through the dedicated POST /api/settings/path-mappings
// route (App.SetPathMappings). The old comma/colon string format was
// retired (issue #236) because it was lossy for any path containing a
// comma -- exactly the kind of path a structured editor makes easier to
// produce.
function renderPathMappingField(f, sv) {
  const wrap = document.createElement("div");
  const rows = document.createElement("div");
  wrap.appendChild(rows);

  // PathMappingEntry (internal/tray/settings.go) carries explicit lowercase
  // camelCase json tags -- unlike most of SettingsView, which is
  // untagged and serializes under its exact (capitalized) Go field
  // names -- because this same shape doubles as the path-mappings route's
  // request body. Read/write "workstationPath"/"containerPath" here to
  // match the wire shape exactly, not "WorkstationPath"/"ContainerPath"
  // the way every other field on this page does.
  let entries = (f.get(sv) ?? []).map((e) => ({ ws: e.workstationPath, cp: e.containerPath }));
  const status = document.createElement("span");
  status.className = "field-status";

  async function commit() {
    const app = getApp();
    if (!app) return;
    const payload = entries
      .map((e) => ({ workstationPath: e.ws.trim(), containerPath: e.cp.trim() }))
      .filter((e) => e.workstationPath && e.containerPath);
    setFieldStatus(status, "Saving…");
    try {
      const sv2 = JSON.parse(await app.SetPathMappings(payload));
      setFieldStatus(status, "Saved", "saved");
      // Reconcile against whatever the reload-after-save actually
      // persisted, in case it diverges from what was sent (e.g. the
      // server-side trim of each field). The server itself never
      // populates pathMappings -- issue #234, HandshakeResponse has no
      // such field -- so in practice this rebuilds from an identical
      // list. Comparing against payload (not the raw, possibly-still-
      // being-typed entries) matters two ways: an in-progress row with
      // one side still blank was filtered OUT of payload, so comparing
      // against unfiltered entries would ALWAYS read as "changed" and
      // wipe that row out from under the operator on every keystroke's
      // blur; and a no-op save (nothing to reconcile) must leave the DOM
      // alone so a mid-edit neighboring row never loses focus.
      const returned = (sv2.PathMappingEntries ?? []).map((e) => ({ workstationPath: e.workstationPath, containerPath: e.containerPath }));
      if (JSON.stringify(returned) !== JSON.stringify(payload)) {
        entries = returned.map((e) => ({ ws: e.workstationPath, cp: e.containerPath }));
        render();
        setFieldStatus(status, "Saved (adjusted)", "saved");
      }
    } catch (err) {
      setFieldStatus(status, String(err), "error");
    }
  }

  function render() {
    rows.innerHTML = "";

    entries.forEach((entry, i) => {
      const row = document.createElement("div");
      row.className = "field-row";
      const label = document.createElement("label");
      label.textContent = i === 0 ? f.label : "";
      row.appendChild(label);
      if (i === 0 && (f.required || f.requiredNote)) {
        const marker = document.createElement("span");
        marker.className = "req";
        marker.title = f.requiredNote ?? "required";
        marker.textContent = "*";
        row.appendChild(marker);
      }

      const pair = document.createElement("div");
      pair.className = "mapping-pair";
      const ws = document.createElement("input");
      ws.type = "text";
      ws.placeholder = "Workstation path";
      ws.value = entry.ws;
      ws.addEventListener("change", () => {
        entry.ws = ws.value;
        commit();
      });
      const cp = document.createElement("input");
      cp.type = "text";
      cp.placeholder = "Container path";
      cp.value = entry.cp;
      cp.addEventListener("change", () => {
        entry.cp = cp.value;
        commit();
      });
      const remove = document.createElement("button");
      remove.type = "button";
      remove.textContent = "Remove";
      remove.addEventListener("click", () => {
        entries.splice(i, 1);
        render();
        commit();
      });
      pair.append(ws, cp, remove);
      row.appendChild(pair);

      if (i === entries.length - 1) row.appendChild(status);
      rows.appendChild(row);
    });

    const addRow = document.createElement("div");
    addRow.className = "field-row";
    const addLabel = document.createElement("label");
    addLabel.textContent = entries.length === 0 ? f.label : "";
    addRow.appendChild(addLabel);
    if (entries.length === 0 && (f.required || f.requiredNote)) {
      const marker = document.createElement("span");
      marker.className = "req";
      marker.title = f.requiredNote ?? "required";
      marker.textContent = "*";
      addRow.appendChild(marker);
    }
    const addBtn = document.createElement("button");
    addBtn.type = "button";
    addBtn.textContent = "Add mapping";
    addBtn.addEventListener("click", () => {
      entries.push({ ws: "", cp: "" });
      render();
    });
    addRow.appendChild(addBtn);
    if (entries.length === 0) addRow.appendChild(status);
    rows.appendChild(addRow);
  }

  render();
  if (f.note) wrap.appendChild(fieldNote(f.note));
  return wrap;
}

// renderStorageField dispatches STORAGE_NAMING_FIELDS entries by f.kind --
// most fields have no kind and fall through to the plain renderTextField,
// same as before this function existed.
function renderStorageField(f, sv) {
  switch (f.kind) {
    case "folderList":
      return renderFolderListField(f, sv);
    case "chipList":
      return renderChipListField(f, sv);
    case "pathMappings":
      return renderPathMappingField(f, sv);
    case "readonly":
      return renderReadOnlyField(f, sv);
    default:
      return renderTextField(f, sv);
  }
}

function renderCheckboxField(f, sv) {
  const row = document.createElement("div");
  row.className = "field-row checkbox-row";

  const input = document.createElement("input");
  input.type = "checkbox";
  input.checked = !!f.get(sv);

  const label = document.createElement("label");
  label.textContent = f.label;

  const status = document.createElement("span");
  status.className = "field-status";

  input.addEventListener("change", async () => {
    // A checkbox has no "keep editing" state the way a text field does --
    // a failed save must revert the visible toggle, or it stays flipped
    // and misrepresents the real config until the window reloads (Hermes
    // review finding on this PR).
    const previous = !input.checked;
    const ok = await saveSetting(f.key, input.checked, status);
    if (!ok) input.checked = previous;
    // f.onSaved (renderIntegrationBlock's Enabled checkbox only) fires
    // AFTER the revert above so it observes post-revert truth -- a
    // rejected save must leave whatever it controls (the detail block's
    // visibility) matching the checkbox's reverted state, not the
    // optimistic click.
    if (f.onSaved) f.onSaved(ok, input.checked);
  });

  row.appendChild(input);
  row.appendChild(label);
  row.appendChild(status);
  return row;
}

function renderSelectField(label, options, current, onChange) {
  const row = document.createElement("div");
  row.className = "field-row";

  const labelEl = document.createElement("label");
  labelEl.textContent = label;
  row.appendChild(labelEl);

  const select = document.createElement("select");
  const currentStr = String(current);
  // A hand-edited config.yaml can set a value none of the fixed options
  // represent (e.g. syncIntervalMinutes: 30) -- setting select.value to a
  // non-matching string leaves nothing truly selected, but the control
  // still visibly displays the first option, misrepresenting the real
  // value (Hermes review finding on this PR). A synthetic leading option
  // carrying the raw value makes that state visible instead of hidden.
  const knownValues = options.map(([v]) => v);
  const optionList = knownValues.includes(currentStr) ? options : [[currentStr, `Current: ${current} (hand-configured)`], ...options];
  for (const [val, text] of optionList) {
    const opt = document.createElement("option");
    opt.value = val;
    opt.textContent = text;
    select.appendChild(opt);
  }
  select.value = currentStr;
  row.appendChild(select);

  const status = document.createElement("span");
  status.className = "field-status";
  let lastGood = select.value;
  select.addEventListener("change", async () => {
    // A select has no "keep editing" state -- a failed save must revert
    // the visible choice, the same reasoning as renderCheckboxField's own
    // revert-on-failure (Hermes review finding on this PR).
    const ok = await onChange(select.value, status);
    if (ok) {
      lastGood = select.value;
    } else {
      select.value = lastGood;
    }
  });
  row.appendChild(status);

  return row;
}

// renderIntegrationBlock renders one Integrations() registry entry's own
// enabled/dry-run/path/rewrites/interval/timeout fields -- the full set of
// per-integration config the tray's own submenu (internal/tray/
// integrationsmenu.go, deleted once this block covered everything it did)
// used to own.
// "resolvedb" (tray.IntegrationResolveDB) is the one integration with a
// database URL instead of a catalog file path, and the only one with
// path rewrites -- both special-cased here the same way
// SetIntegrationPath/SetIntegrationRewrites special-case it server-side.
function renderIntegrationBlock(iv) {
  const isResolve = iv.ID === "resolvedb";
  const block = document.createElement("div");
  block.className = "integration-block";

  const h3 = document.createElement("h3");
  // iv.Title || iv.ID: same fallback renderIntegrations/renderHooks use
  // for the live-status side (i.Title || i.ID / h.Title || h.ID below) --
  // untagged Go field, so a Title-less snapshot (older agent, test
  // literal) still renders the raw ID instead of going blank (issue #221).
  h3.textContent = iv.Title || iv.ID;
  block.appendChild(h3);

  // detail holds every field below Enabled -- hidden whenever the
  // integration is disabled, since Dry run/Catalog path/Path rewrites/
  // Sync interval/Sync timeout are all noise for something that isn't
  // running. Toggled via renderCheckboxField's onSaved hook below, not
  // rebuilt: renderSettingsForm runs once, and the Enabled checkbox's own
  // change handler is the only re-entry point into this block afterward,
  // so flipping `hidden` in place is simpler and keeps every field's live
  // value in the DOM rather than discarding and re-creating it. This
  // element itself carries no display rule of its own, so the browser's
  // default `[hidden] { display: none }` already hides it without help;
  // #230's `[hidden] { display: none !important; }` override exists for
  // an element that DOES have a competing display rule (a .field-row,
  // whose own display:grid would otherwise win the cascade) -- harmless
  // here, not load-bearing for this specific element.
  const detail = document.createElement("div");
  detail.className = "integration-detail";
  detail.hidden = !iv.Enabled;

  // renderCheckboxField's second argument is the "sv" a top-level
  // BEHAVIOR_FIELDS descriptor's own get(sv) reads from; these two
  // descriptors close over iv directly instead (there is no top-level
  // settings snapshot to hand them), so {} is deliberately unused here.
  block.appendChild(
    renderCheckboxField(
      {
        key: `integrations.${iv.ID}.enabled`,
        label: "Enabled",
        get: () => iv.Enabled,
        // Fires after renderCheckboxField's own revert-on-failure, so a
        // rejected save leaves `detail` matching the reverted checkbox
        // state, never the optimistic click.
        onSaved: (_ok, checked) => {
          detail.hidden = !checked;
        },
      },
      {},
    ),
  );
  detail.appendChild(
    renderCheckboxField({ key: `integrations.${iv.ID}.dryRun`, label: "Dry run (log only)", get: () => iv.DryRun }, {}),
  );

  const pathRow = document.createElement("div");
  pathRow.className = "field-row";
  const pathLabel = document.createElement("label");
  pathLabel.textContent = isResolve ? "Database URL" : "Catalog path";
  pathRow.appendChild(pathLabel);
  const pathInput = document.createElement("input");
  pathInput.type = isResolve ? "password" : "text";
  pathInput.value = isResolve ? "" : (iv.CatalogPath ?? "");
  if (isResolve) pathInput.placeholder = iv.CatalogPathSet ? "(configured — leave blank to keep)" : "(not set)";
  pathRow.appendChild(pathInput);
  const pathStatus = document.createElement("span");
  pathStatus.className = "field-status";
  // commit is shared between a manual edit's "change" event and a
  // successful Browse pick, same reasoning as renderTextField's own
  // commit() -- assigning pathInput.value programmatically doesn't fire
  // "change" on its own, so a Browse pick used to fill the box visually
  // and never actually save.
  async function commit() {
    if (isResolve && pathInput.value === "") return; // blank means "leave unchanged"
    const app = getApp();
    if (!app) return;
    setFieldStatus(pathStatus, "Saving…");
    try {
      await app.SetIntegrationPath(iv.ID, pathInput.value);
      setFieldStatus(pathStatus, "Saved", "saved");
      if (isResolve) pathInput.value = "";
    } catch (err) {
      setFieldStatus(pathStatus, String(err), "error");
    }
  }
  if (!isResolve) {
    const browse = document.createElement("button");
    browse.type = "button";
    browse.textContent = "Browse…";
    browse.addEventListener("click", async () => {
      const app = getApp();
      if (!app) return;
      try {
        // Deliberately unfiltered (no patterns): the per-integration
        // catalog-file filter patterns (e.g. Luminar's
        // "*.db"/"*.catalog"/"*") never crossed into the settings JSON
        // payload, and the tray's own filtered file-picker dialog they
        // once backed (IntegrationBuilder.CatalogFilePatterns,
        // PromptAndSetIntegrationPath) is gone as of issue #217 -- there
        // is no filtered picker left to match.
        const picked = await app.PickFile(pathLabel.textContent, []);
        if (picked) {
          pathInput.value = picked;
          await commit();
        }
      } catch (err) {
        setFieldStatus(pathStatus, String(err), "error");
      }
    });
    pathRow.appendChild(browse);
  }
  pathInput.addEventListener("change", commit);
  pathRow.appendChild(pathStatus);
  detail.appendChild(pathRow);

  if (isResolve) {
    const rewriteRow = document.createElement("div");
    rewriteRow.className = "field-row";
    const rwLabel = document.createElement("label");
    rwLabel.textContent = "Path rewrites";
    rewriteRow.appendChild(rwLabel);
    const rwInput = document.createElement("input");
    rwInput.type = "text";
    rwInput.value = iv.PathRewrites ?? "";
    rewriteRow.appendChild(rwInput);
    const rwStatus = document.createElement("span");
    rwStatus.className = "field-status";
    rwInput.addEventListener("change", async () => {
      const app = getApp();
      if (!app) return;
      setFieldStatus(rwStatus, "Saving…");
      try {
        await app.SetIntegrationRewrites(iv.ID, rwInput.value);
        setFieldStatus(rwStatus, "Saved", "saved");
      } catch (err) {
        setFieldStatus(rwStatus, String(err), "error");
      }
    });
    rewriteRow.appendChild(rwStatus);
    detail.appendChild(rewriteRow);
  }

  detail.appendChild(
    renderSelectField(
      "Sync every",
      [
        ["15", "15 minutes"],
        ["60", "60 minutes (default)"],
        ["-1", "Never (manual only)"],
      ],
      iv.SyncIntervalMinutes || 60,
      (val, status) => saveSetting(`integrations.${iv.ID}.syncIntervalMinutes`, Number(val), status),
    ),
  );

  // Sync timeout used to be the one field only the tray's own per-integration
  // submenu could edit (internal/tray/integrationsmenu.go, now deleted); this
  // is that field's new home, so nothing was lost when that submenu went
  // away. renderSelectField's own "hand-configured" fallback reproduces the
  // tray's "Sync timeout (currently: %ds, hand-configured)" note for a value
  // outside the fixed set, same as the "Sync every" field above.
  detail.appendChild(
    renderSelectField(
      "Sync timeout",
      [
        ["30", "30 seconds (default)"],
        ["60", "1 minute"],
        ["300", "5 minutes"],
        ["600", "10 minutes"],
      ],
      iv.TimeoutSecs || 30,
      (val, status) => saveSetting(`integrations.${iv.ID}.timeoutSecs`, Number(val), status),
    ),
  );

  block.appendChild(detail);
  return block;
}

// renderSettingsForm fills the five config-zone containers (see index.html)
// from one SettingsView snapshot. Called ONLY from loadSettings(), once --
// never from the 5s status poll (render(), below) -- or an operator's
// mid-typing edit would be wiped out from under them. Every container id
// this function writes into ends "-config"; TestStatusPollNeverRebuildsSettingsContainers
// (app_test.go) pins that render()'s call graph never touches one.
function renderSettingsForm(sv) {
  refreshEnabledByID(sv);

  const serverContainer = byId("settings-server-config");
  serverContainer.innerHTML = "";
  for (const f of SERVER_IDENTITY_FIELDS) serverContainer.appendChild(renderTextField(f, sv));

  const storageContainer = byId("settings-storage-config");
  storageContainer.innerHTML = "";
  for (const f of STORAGE_NAMING_FIELDS) storageContainer.appendChild(renderStorageField(f, sv));

  const behaviorContainer = byId("settings-behavior-config");
  behaviorContainer.innerHTML = "";
  for (const f of BEHAVIOR_FIELDS) behaviorContainer.appendChild(renderCheckboxField(f, sv));

  const selfUpdateContainer = byId("settings-selfupdate-config");
  selfUpdateContainer.innerHTML = "";
  selfUpdateContainer.appendChild(
    renderCheckboxField({ key: "selfUpdate.enabled", label: "Enable self-update checks", get: (s) => s.SelfUpdateEnabled }, sv),
  );
  selfUpdateContainer.appendChild(
    renderSelectField(
      "Check for updates",
      [
        ["1", "Every hour"],
        ["24", "Every 24 hours (default)"],
        ["-1", "Never (check once at startup only)"],
      ],
      sv.SelfUpdateCheckIntervalHrs || 24,
      (val, status) => saveSetting("selfUpdate.checkIntervalHours", Number(val), status),
    ),
  );

  const integrationsContainer = byId("settings-integrations-config");
  integrationsContainer.innerHTML = "";
  const integrations = sv.Integrations ?? [];
  if (!integrations.length) {
    integrationsContainer.innerHTML = `<p class="empty">No integrations registered.</p>`;
    return;
  }
  for (const iv of integrations) integrationsContainer.appendChild(renderIntegrationBlock(iv));
}

async function loadSettings() {
  const app = getApp();
  if (!app) {
    setTimeout(loadSettings, 500);
    return;
  }
  try {
    const sv = JSON.parse(await app.SettingsJSON());
    byId("settings-error").textContent = "";
    renderSettingsForm(sv);
  } catch (err) {
    // Retry at the same cadence as the status poll rather than leaving a
    // dead error: the agent may not be running yet when this window
    // opens, or may still be restarting -- both normal cases (see
    // app.go's StatusJSON doc comment), and the status section already
    // self-heals the same way. Once a load succeeds, this stops
    // rescheduling itself -- the settings form is still loaded once, not
    // on a timer, so an in-progress edit is never overwritten.
    byId("settings-error").textContent = String(err);
    setTimeout(loadSettings, POLL_INTERVAL_MS);
  }
}

loadSettings();
