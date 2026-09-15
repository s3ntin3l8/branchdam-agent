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
// mid-typing. A field re-renders only after ITS OWN save completes, using
// the fresh snapshot SetSetting/SetIntegrationPath/SetIntegrationRewrites
// already returns -- never a blanket re-fetch of every field at once.

// FREE_TEXT_FIELDS mirrors internal/tray/settingsmenu.go's own free-text
// items exactly, one row per PromptAndSet(FieldX) call there. `list: true`
// means the value round-trips as a JSON array of strings (SetStringSlice,
// the canonical wire shape for a list per statusapi.go's settingsPatchRequest
// doc comment), not a comma-separated string.
const FREE_TEXT_FIELDS = [
  { key: "server.baseUrl", label: "Server URL", get: (sv) => sv.ServerBaseURL },
  {
    key: "server.apiKey",
    label: "API key",
    password: true,
    get: () => "",
    placeholder: (sv) => (sv.ServerAPIKeySet ? "(configured — leave blank to keep)" : "(not set)"),
  },
  { key: "agentId", label: "Agent ID", get: (sv) => sv.AgentID },
  { key: "ingest.archiveRoot", label: "Archive root", get: (sv) => sv.ArchiveRoot, browseDir: true },
  { key: "ingest.localEditRoot", label: "Local edit root", get: (sv) => sv.LocalEditRoot, browseDir: true },
  {
    key: "ingest.cardRoots",
    label: "Watch folders",
    get: () => "",
    placeholder: () => "comma-separated -- current value not shown, enter to replace",
    list: true,
  },
  {
    key: "ingest.allowedExtensions",
    label: "Allowed extensions",
    get: (sv) => (sv.AllowedExtensions ?? []).join(", "),
    list: true,
  },
  { key: "ingest.pathTemplate", label: "Naming template", get: (sv) => sv.NamingTemplate },
  { key: "pathMappings", label: "Path mappings", get: (sv) => sv.PathMappings },
  { key: "integrations.nodeIndexPath", label: "Node index path", get: (sv) => sv.NodeIndexPath, browseFile: ["*.json"] },
];

const CHECKBOX_FIELDS = [
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
async function saveSetting(key, value, statusEl) {
  const app = getApp();
  if (!app) return;
  setFieldStatus(statusEl, "Saving…");
  try {
    await app.SetSetting(key, value);
    setFieldStatus(statusEl, "Saved", "saved");
  } catch (err) {
    setFieldStatus(statusEl, String(err), "error");
  }
}

function splitCommaList(s) {
  return s
    .split(",")
    .map((p) => p.trim())
    .filter((p) => p.length > 0);
}

function renderTextField(f, sv) {
  const row = document.createElement("div");
  row.className = "field-row";

  const label = document.createElement("label");
  label.textContent = f.label;
  row.appendChild(label);

  const input = document.createElement("input");
  input.type = f.password ? "password" : "text";
  input.value = f.get(sv) ?? "";
  if (f.placeholder) input.placeholder = f.placeholder(sv);
  row.appendChild(input);

  const status = document.createElement("span");
  status.className = "field-status";

  if (f.browseDir || f.browseFile) {
    const browse = document.createElement("button");
    browse.type = "button";
    browse.textContent = "Browse…";
    browse.addEventListener("click", async () => {
      const app = getApp();
      if (!app) return;
      try {
        const picked = f.browseDir ? await app.PickDirectory(f.label) : await app.PickFile(f.label, f.browseFile);
        if (picked) input.value = picked;
      } catch (err) {
        setFieldStatus(status, String(err), "error");
      }
    });
    row.appendChild(browse);
  }

  input.addEventListener("change", () => {
    if (f.password && input.value === "") return; // blank means "leave unchanged"
    const value = f.list ? splitCommaList(input.value) : input.value;
    saveSetting(f.key, value, status).then(() => {
      if (f.password) input.value = "";
    });
  });

  row.appendChild(status);
  return row;
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

  input.addEventListener("change", () => {
    saveSetting(f.key, input.checked, status);
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
  for (const [val, text] of options) {
    const opt = document.createElement("option");
    opt.value = val;
    opt.textContent = text;
    select.appendChild(opt);
  }
  select.value = String(current);
  row.appendChild(select);

  const status = document.createElement("span");
  status.className = "field-status";
  select.addEventListener("change", () => onChange(select.value, status));
  row.appendChild(status);

  return row;
}

// renderIntegrationBlock renders one Integrations() registry entry's own
// enabled/dry-run/path/rewrites/interval fields, mirroring
// internal/tray/integrationsmenu.go's own per-integration submenu.
// "resolvedb" (tray.IntegrationResolveDB) is the one integration with a
// database URL instead of a catalog file path, and the only one with
// path rewrites -- both special-cased here the same way
// PromptAndSetIntegrationPath/PromptAndSetIntegrationRewrites special-case
// it server-side.
function renderIntegrationBlock(iv) {
  const isResolve = iv.ID === "resolvedb";
  const block = document.createElement("div");
  block.className = "integration-block";

  const h3 = document.createElement("h3");
  h3.textContent = iv.ID;
  block.appendChild(h3);

  // renderCheckboxField's second argument is the "sv" a top-level
  // CHECKBOX_FIELDS descriptor's own get(sv) reads from; these two
  // descriptors close over iv directly instead (there is no top-level
  // settings snapshot to hand them), so {} is deliberately unused here.
  block.appendChild(renderCheckboxField({ key: `integrations.${iv.ID}.enabled`, label: "Enabled", get: () => iv.Enabled }, {}));
  block.appendChild(
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
  if (!isResolve) {
    const browse = document.createElement("button");
    browse.type = "button";
    browse.textContent = "Browse…";
    browse.addEventListener("click", async () => {
      const app = getApp();
      if (!app) return;
      try {
        // Deliberately unfiltered (no patterns): IntegrationBuilder.
        // CatalogFilePatterns (server-side only, e.g. Luminar's
        // "*.db"/"*.catalog"/"*") isn't part of the settings JSON payload
        // today, so this picker can't apply the same filter the tray's
        // PromptAndSetIntegrationPath dialog does. Follow-up if this is
        // worth closing before the tray-slimming PR removes the filtered
        // one.
        const picked = await app.PickFile(pathLabel.textContent, []);
        if (picked) pathInput.value = picked;
      } catch (err) {
        setFieldStatus(pathStatus, String(err), "error");
      }
    });
    pathRow.appendChild(browse);
  }
  pathInput.addEventListener("change", async () => {
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
  });
  pathRow.appendChild(pathStatus);
  block.appendChild(pathRow);

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
    block.appendChild(rewriteRow);
  }

  block.appendChild(
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

  return block;
}

function renderSettingsForm(sv) {
  const container = byId("settings-body");
  container.innerHTML = "";

  for (const f of FREE_TEXT_FIELDS) container.appendChild(renderTextField(f, sv));
  for (const f of CHECKBOX_FIELDS) container.appendChild(renderCheckboxField(f, sv));

  container.appendChild(
    renderCheckboxField({ key: "selfUpdate.enabled", label: "Enable self-update checks", get: (s) => s.SelfUpdateEnabled }, sv),
  );
  container.appendChild(
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

  const integrations = sv.Integrations ?? [];
  if (integrations.length) {
    const h3 = document.createElement("h3");
    h3.textContent = "Integrations";
    container.appendChild(h3);
    for (const iv of integrations) container.appendChild(renderIntegrationBlock(iv));
  }
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
    byId("settings-error").textContent = String(err);
  }
}

loadSettings();
