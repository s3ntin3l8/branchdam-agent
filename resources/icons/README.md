# branchDAM Agent icons

Every binary icon asset this repo ships (macOS `.icns`, Windows `.ico`) is generated in pure Go at
build time, not committed here and not hand-authored via ImageMagick/iconutil. `icon.svg` in this
directory is the design reference the geometry was drawn from — it is not itself consumed by any
build step.

## Where the real pipeline lives

- **macOS `.icns`** — `internal/appicon.ICNS()`, invoked by `go run ./tools/mkbundle` (see
  `docs/platform-support.md`'s macOS `.app` bundle section). Produces every size
  Finder/Dock/the DMG window need, packaged as a real `.icns` container.
- **Windows `.ico`** — `internal/appicon.ICO()`, invoked by `go run ./tools/mkicon dist/icon.ico`
  (`make build-windows`, `.github/workflows/release-binaries.yml`'s `build-windows` job). A
  multi-resolution (16/32/48/256) `.ico`, embedded into both `branchdam-agent.exe` and
  `branchdam-agent-tray.exe` via `github.com/josephspurrier/goversioninfo` generating a
  `resource.syso` ahead of `go build` (Go auto-links a `resource.syso` present in a package
  directory into that package's Windows build; no `//go:generate` or build-tag machinery needed),
  and wired into the NSIS installer via `MUI_ICON`/`MUI_UNICON`
  (`installer/windows/branchdam-agent.nsi`). Neither `dist/icon.ico` nor `resource.syso` is
  committed — both are regenerated on every build and gitignored.
- **Tray icon** (the small systray glyph, distinct from the app icon above) —
  `internal/tray/icon.go`, rendered directly into memory at tray startup, never written to disk at
  all.

All three render the same b-node monogram geometry, hand-duplicated across the three files rather
than shared (each file's own doc comment explains why: none of the call sites benefit enough from a
shared abstraction to be worth the indirection). A geometry change needs to be mirrored by hand in
whichever of the three actually need it.

## Why no ImageMagick/iconutil

This directory used to document a manual `convert`/`sips`/`iconutil` recipe for generating these
assets by hand. That recipe was never actually run — no binary icon asset was ever committed from
it — which is exactly the kind of drift a documented-but-unexecuted process invites. Generating
icons in Go instead means: no external tool dependency for contributors, no binary asset to keep in
sync with `icon.svg`, and the icon pipeline is exercised by the same `make build-windows`/
`build-darwin-app` a contributor already runs, so it can't silently rot the way the old recipe did.

That old recipe's doc also listed an NSIS installer header/sidebar bitmap
(`installer-header.bmp`/`installer-sidebar.bmp`) as a "required file." Neither was ever generated or
committed either; `installer/windows/branchdam-agent.nsi` intentionally leaves `MUI_HEADERIMAGE`/
`MUI_WELCOMEFINISHPAGE_BITMAP` unset and uses the MUI2 defaults, so there is nothing missing here —
just a scope this repo hasn't picked up.

## Design reference

- **Foreground**: branchDAM teal (`#2ba69a`) — see `internal/appicon.Foreground` /
  `internal/tray/icon.go`'s own color constant, which must stay in sync with it.
- The monogram should stay recognizable at 16×16 (Windows taskbar / macOS Dock at the smallest
  practical size) through 1024×1024 (a Retina Dock/Finder icon).
