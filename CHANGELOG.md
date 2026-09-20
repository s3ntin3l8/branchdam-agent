# Changelog

## [1.14.1](https://github.com/s3ntin3l8/branchdam-agent/compare/v1.14.0...v1.14.1) (2026-09-20)


### Bug Fixes

* repair macOS DMG build ([843c19b](https://github.com/s3ntin3l8/branchdam-agent/commit/843c19b03738a3427f570d7334e19ecd061fe18b))

## [1.14.0](https://github.com/s3ntin3l8/branchdam-agent/compare/v1.13.0...v1.14.0) (2026-09-20)


### Features

* polish tray icon, status text, settings UI, and DMG installer ([#247](https://github.com/s3ntin3l8/branchdam-agent/issues/247)) ([79b9ec6](https://github.com/s3ntin3l8/branchdam-agent/commit/79b9ec69acdd99b75b9a3e3b7d9ceb00804fe89f))

## [1.13.0](https://github.com/s3ntin3l8/branchdam-agent/compare/v1.12.1...v1.13.0) (2026-09-19)


### Features

* **agent:** branchdam-agent pair &lt;url&gt; persists paired credentials ([5d5f85f](https://github.com/s3ntin3l8/branchdam-agent/commit/5d5f85f02a556902c9686d923f60818a162e3055))
* **agent:** branchdam-agent pair &lt;url&gt; persists paired credentials ([#243](https://github.com/s3ntin3l8/branchdam-agent/issues/243)) ([64c1541](https://github.com/s3ntin3l8/branchdam-agent/commit/64c154129785b0e946ed0df66bdbcfbc2513e76b))

## [1.12.1](https://github.com/s3ntin3l8/branchdam-agent/compare/v1.12.0...v1.12.1) (2026-09-17)


### Bug Fixes

* **branchdam-agent:** agent silently drops the server's key-rotation handshake hint ([24d0b2d](https://github.com/s3ntin3l8/branchdam-agent/commit/24d0b2d8f7dbd86eb5a4239eae147f4cf11e3e7a))
* **branchdam,agent:** agent's handshake pathMappings field is dead code -- server never sends it ([dfacedb](https://github.com/s3ntin3l8/branchdam-agent/commit/dfacedba4c54ad5997d9afcc7376594d3a4d34c2))

## [1.12.0](https://github.com/s3ntin3l8/branchdam-agent/compare/v1.11.0...v1.12.0) (2026-09-17)


### Features

* **ui,tray:** add on-demand "Check for updates now" button ([#233](https://github.com/s3ntin3l8/branchdam-agent/issues/233)) ([e2f20be](https://github.com/s3ntin3l8/branchdam-agent/commit/e2f20bec8852faee406f10e92119d32efa0c00c1))
* **ui:** adopt server's semantic style tokens and fix field alignment ([#230](https://github.com/s3ntin3l8/branchdam-agent/issues/230)) ([f7baffb](https://github.com/s3ntin3l8/branchdam-agent/commit/f7baffb9c7c2ea3efc9da9da3c09f7d91943f1e4))
* **ui:** hide integration detail settings and status when disabled ([#232](https://github.com/s3ntin3l8/branchdam-agent/issues/232)) ([071db7c](https://github.com/s3ntin3l8/branchdam-agent/commit/071db7c635525029a46c5c2406e3afecf7fbec36))
* **ui:** structured watch-folders/extensions/path-mappings editors ([#231](https://github.com/s3ntin3l8/branchdam-agent/issues/231)) ([067e593](https://github.com/s3ntin3l8/branchdam-agent/commit/067e59312362cd4816c38da6c9b380adf6778152))

## [1.11.0](https://github.com/s3ntin3l8/branchdam-agent/compare/v1.10.0...v1.11.0) (2026-09-16)


### Features

* restructure the window into a nav-pane of paired settings/status categories ([#227](https://github.com/s3ntin3l8/branchdam-agent/issues/227)) ([009f0fc](https://github.com/s3ntin3l8/branchdam-agent/commit/009f0fc3c5ec6b4da4f6645cf04e9a3e2790f76c))
* **tray,ui:** surface missing setup fields and add server connection check ([#228](https://github.com/s3ntin3l8/branchdam-agent/issues/228)) ([add59f2](https://github.com/s3ntin3l8/branchdam-agent/commit/add59f229267ac9e028da7c1db60fcd18ee3475b))

## [1.10.0](https://github.com/s3ntin3l8/branchdam-agent/compare/v1.9.0...v1.10.0) (2026-09-16)


### Features

* rethink the tray and window settings UX ([#223](https://github.com/s3ntin3l8/branchdam-agent/issues/223)) ([39c0f75](https://github.com/s3ntin3l8/branchdam-agent/commit/39c0f75debcd13365f08de841dace9878241d801))


### Bug Fixes

* **ui:** render the integration's friendly Title in the Settings form ([#225](https://github.com/s3ntin3l8/branchdam-agent/issues/225)) ([3cfa354](https://github.com/s3ntin3l8/branchdam-agent/commit/3cfa354acdadc6dbbd05853ce85425dc00d791b8)), closes [#221](https://github.com/s3ntin3l8/branchdam-agent/issues/221)

## [1.9.0](https://github.com/s3ntin3l8/branchdam-agent/compare/v1.8.0...v1.9.0) (2026-09-16)


### Features

* **resolve:** reconcile complete database snapshots ([#213](https://github.com/s3ntin3l8/branchdam-agent/issues/213)) ([d200128](https://github.com/s3ntin3l8/branchdam-agent/commit/d200128ca98d88ab919ff7e410a66e13ef8232d1))


### Bug Fixes

* repair v1.8.0 Wails UI, tray duplicate menu entry, and release asset naming ([9b8ca6e](https://github.com/s3ntin3l8/branchdam-agent/commit/9b8ca6ed0607c2c232c9750a7e21d4f7c2163014))

## [1.8.0](https://github.com/s3ntin3l8/branchdam-agent/compare/v1.7.0...v1.8.0) (2026-09-16)


### Features

* **packaging:** ship the UI binary in both installers (Track 3f) ([#214](https://github.com/s3ntin3l8/branchdam-agent/issues/214)) ([30e18c3](https://github.com/s3ntin3l8/branchdam-agent/commit/30e18c3393511d20e581e550e0ce19e5dc842beb))
* **ui:** wire "Open branchDAM" and slim the tray's Settings/Integrations menus ([#211](https://github.com/s3ntin3l8/branchdam-agent/issues/211)) ([#218](https://github.com/s3ntin3l8/branchdam-agent/issues/218)) ([c8c3ebb](https://github.com/s3ntin3l8/branchdam-agent/commit/c8c3ebb4103a9f9c50cc7429c6cc417d810ad8cf))
* **ui:** wire Integrations/Hooks actions into the Wails window (Track 3e) ([#216](https://github.com/s3ntin3l8/branchdam-agent/issues/216)) ([5cbc618](https://github.com/s3ntin3l8/branchdam-agent/commit/5cbc618551c56fb7dc6cb7edfae5f5e934b24eac))

## [1.7.0](https://github.com/s3ntin3l8/branchdam-agent/compare/v1.6.0...v1.7.0) (2026-09-16)


### Features

* **tray:** add non-interactive Settings.SetString and integration-path/rewrite setters ([#209](https://github.com/s3ntin3l8/branchdam-agent/issues/209)) ([6cd1574](https://github.com/s3ntin3l8/branchdam-agent/commit/6cd157449f6f545ee4f620d434e643401422a0e0))
* **tray:** harden the loopback status-server API with actions and auth ([#206](https://github.com/s3ntin3l8/branchdam-agent/issues/206)) ([db4aa50](https://github.com/s3ntin3l8/branchdam-agent/commit/db4aa50c0e350f484c2b886fd455e531dd0ed81f))
* **ui:** add a Settings section to the Wails app UI ([#210](https://github.com/s3ntin3l8/branchdam-agent/issues/210)) ([330e9e6](https://github.com/s3ntin3l8/branchdam-agent/commit/330e9e6a42d9d21232b095308b9d24349c30833d))
* **ui:** add cmd/branchdam-agent-ui, a native status window (Wails v2) ([#207](https://github.com/s3ntin3l8/branchdam-agent/issues/207)) ([20f928a](https://github.com/s3ntin3l8/branchdam-agent/commit/20f928ab205f3e40a0edaf09b976f317953aa3da))


### Bug Fixes

* **macos:** ad-hoc sign the app bundle and ship a .dmg with an icon ([#200](https://github.com/s3ntin3l8/branchdam-agent/issues/200)) ([d65b1d9](https://github.com/s3ntin3l8/branchdam-agent/commit/d65b1d9d23bca346213ef1ff3b599438f9f9601d))
* **windows:** harden the NSIS installer ([#202](https://github.com/s3ntin3l8/branchdam-agent/issues/202)) ([70537c2](https://github.com/s3ntin3l8/branchdam-agent/commit/70537c21f8c00b0ce0555e5e2bbac8785c7e5b90))

## [1.6.0](https://github.com/s3ntin3l8/branchdam-agent/compare/v1.5.0...v1.6.0) (2026-09-15)


### Features

* **agent:** wire Resolve auto-discovery and sync persistence ([#198](https://github.com/s3ntin3l8/branchdam-agent/issues/198)) ([068de8f](https://github.com/s3ntin3l8/branchdam-agent/commit/068de8fd2abc9dedcdd7513581555b819816d1cd))
* DaVinci Resolve Project Server database watcher ([#187](https://github.com/s3ntin3l8/branchdam-agent/issues/187)) ([3245c42](https://github.com/s3ntin3l8/branchdam-agent/commit/3245c429b4e71942ac48e0196fb185ed6090f78f))
* **resolve:** add change-aware polling with delta detection ([#196](https://github.com/s3ntin3l8/branchdam-agent/issues/196)) ([b2c3c28](https://github.com/s3ntin3l8/branchdam-agent/commit/b2c3c288e2b3e2e25206f1a8e823d784192c74c1))
* **resolve:** add DaVinci Resolve database auto-discovery ([#194](https://github.com/s3ntin3l8/branchdam-agent/issues/194)) ([ee92454](https://github.com/s3ntin3l8/branchdam-agent/commit/ee92454760b837d9a78da657c5d3a0143b4a7f35))
* **resolve:** emit virtual nodes and PROJECT_SIDECAR edges (v2) ([451ee20](https://github.com/s3ntin3l8/branchdam-agent/commit/451ee20ae9bea866e614a43f8ab218c65821b341))
* **resolve:** emit virtual nodes and PROJECT_SIDECAR edges (v2) ([#190](https://github.com/s3ntin3l8/branchdam-agent/issues/190)) ([4610d26](https://github.com/s3ntin3l8/branchdam-agent/commit/4610d26f11c0cf45a2d1b899da8c39ee7ba4e437))
* **runtime:** add Resolve membership fields to persisted state ([#195](https://github.com/s3ntin3l8/branchdam-agent/issues/195)) ([55bb837](https://github.com/s3ntin3l8/branchdam-agent/commit/55bb8371d6dc38e18b026f4f18a7319b4e50ed61))
* **tray:** add sync save callback with data-race protection ([#197](https://github.com/s3ntin3l8/branchdam-agent/issues/197)) ([b6ed380](https://github.com/s3ntin3l8/branchdam-agent/commit/b6ed380540104e8cbecb3e7c462512903ef6863c))
* **tray:** add timeout submenu and path rewrites editor to integration menus ([#188](https://github.com/s3ntin3l8/branchdam-agent/issues/188)) ([d1c1649](https://github.com/s3ntin3l8/branchdam-agent/commit/d1c164967d31b74e2ce0dfbad13f7ebaba81462f))
* Windows NSIS installer and one-click tray setup ([#185](https://github.com/s3ntin3l8/branchdam-agent/issues/185)) ([215b299](https://github.com/s3ntin3l8/branchdam-agent/commit/215b299f25977e652da3e0f6faf4416d97bda0be))


### Bug Fixes

* make desktop releases deployable ([#189](https://github.com/s3ntin3l8/branchdam-agent/issues/189)) ([fee4fe6](https://github.com/s3ntin3l8/branchdam-agent/commit/fee4fe6d36f4703a5370096d97b1a0fd13f3148a))

## [1.5.0](https://github.com/s3ntin3l8/branchdam-agent/compare/v1.4.0...v1.5.0) (2026-09-11)


### Features

* **branchdam:** add EventUUID for transport-level idempotency ([#182](https://github.com/s3ntin3l8/branchdam-agent/issues/182)) ([234af29](https://github.com/s3ntin3l8/branchdam-agent/commit/234af297e9f4db660b38fd1334d22385dd5c0a07))
* **branchdam:** add scratch telemetry client and source path hash header ([#178](https://github.com/s3ntin3l8/branchdam-agent/issues/178)) ([e755544](https://github.com/s3ntin3l8/branchdam-agent/commit/e75554430407b205794392d7dbf44318e479b522))


### Bug Fixes

* **ci:** grant id-token to release-binaries caller, drop prerelease flag ([#169](https://github.com/s3ntin3l8/branchdam-agent/issues/169)) ([2c822ef](https://github.com/s3ntin3l8/branchdam-agent/commit/2c822ef3d683ea8ed089fbcff2820624c06f986e))
* **ci:** grant issues: write to release-please job for PR labeling ([#170](https://github.com/s3ntin3l8/branchdam-agent/issues/170)) ([ffe1380](https://github.com/s3ntin3l8/branchdam-agent/commit/ffe13807db42226f198140b8fd83a3fd547ef6b4))
* **ci:** skip github release for v1.4.0, create out-of-band ([#171](https://github.com/s3ntin3l8/branchdam-agent/issues/171)) ([db640a8](https://github.com/s3ntin3l8/branchdam-agent/commit/db640a807706c50cc509e4eab38a041389635265))
* exclude bot-authored PRs by author, not actor; correct guard comment ([#176](https://github.com/s3ntin3l8/branchdam-agent/issues/176)) ([547c5d2](https://github.com/s3ntin3l8/branchdam-agent/commit/547c5d2a6921025a3fcb4136e483233b1d3e6369))

## [1.4.0](https://github.com/s3ntin3l8/branchdam-agent/compare/v1.3.0...v1.4.0) (2026-09-02)


### Features

* **ingest:** skip OS metadata and apply AllowedExtensions filter ([#100](https://github.com/s3ntin3l8/branchdam-agent/issues/100)) ([#127](https://github.com/s3ntin3l8/branchdam-agent/issues/127)) ([dfe93da](https://github.com/s3ntin3l8/branchdam-agent/commit/dfe93dabef5b97ba9d750054437d307b720787ea))
* **queue:** add status-column index + schema migration seam ([#106](https://github.com/s3ntin3l8/branchdam-agent/issues/106)) ([#119](https://github.com/s3ntin3l8/branchdam-agent/issues/119)) ([0ebfba5](https://github.com/s3ntin3l8/branchdam-agent/commit/0ebfba58821d38390015a6e7b6418ff8e0415fe9))
* **tray/settings:** make ingest.cardRoots editable from Settings + live Detector restart ([#78](https://github.com/s3ntin3l8/branchdam-agent/issues/78)) ([#133](https://github.com/s3ntin3l8/branchdam-agent/issues/133)) ([0e3baee](https://github.com/s3ntin3l8/branchdam-agent/commit/0e3baeeeedfdf58498fd931ccc21e421eee74237))
* **tray:** expose missing config fields in settings menu ([#110](https://github.com/s3ntin3l8/branchdam-agent/issues/110)) ([#132](https://github.com/s3ntin3l8/branchdam-agent/issues/132)) ([9e09f61](https://github.com/s3ntin3l8/branchdam-agent/commit/9e09f6183d4ed561334ce080c85680adfc68a005))
* **tray:** surface handshake/in-flight status on status page ([#109](https://github.com/s3ntin3l8/branchdam-agent/issues/109)) ([#123](https://github.com/s3ntin3l8/branchdam-agent/issues/123)) ([22dd00c](https://github.com/s3ntin3l8/branchdam-agent/commit/22dd00c6b62c4b47e13684aad5700e4abef20d9f))


### Bug Fixes

* **autostart:** JSON-encode args to sidecar file to prevent injection ([#98](https://github.com/s3ntin3l8/branchdam-agent/issues/98)) ([#124](https://github.com/s3ntin3l8/branchdam-agent/issues/124)) ([92833fd](https://github.com/s3ntin3l8/branchdam-agent/commit/92833fddb2ed63e4326f92fc8e212dda2fea466a))
* **config:** validate Server.BaseURL scheme and loopback policy ([#96](https://github.com/s3ntin3l8/branchdam-agent/issues/96)) ([#125](https://github.com/s3ntin3l8/branchdam-agent/issues/125)) ([2a7d415](https://github.com/s3ntin3l8/branchdam-agent/commit/2a7d415ea9535b0e9e40875ad687f534d87e4bef))
* **config:** warn on world-readable config.yaml with apiKey ([#97](https://github.com/s3ntin3l8/branchdam-agent/issues/97)) ([#126](https://github.com/s3ntin3l8/branchdam-agent/issues/126)) ([5b13f7a](https://github.com/s3ntin3l8/branchdam-agent/commit/5b13f7ac1d102edcaee741c452f77ef8aecaab96))
* **ingest:** add FastHash budget and short-circuit to collision loop ([#105](https://github.com/s3ntin3l8/branchdam-agent/issues/105)) ([#129](https://github.com/s3ntin3l8/branchdam-agent/issues/129)) ([19712be](https://github.com/s3ntin3l8/branchdam-agent/commit/19712be6ac07e639ba8f492a72b7a0ff587e8ebe))
* **ingest:** fsync parent directories after DualWrite ([#117](https://github.com/s3ntin3l8/branchdam-agent/issues/117)) ([a7476bf](https://github.com/s3ntin3l8/branchdam-agent/commit/a7476bf3af7b43de17cd306a242930c15014b9ad)), closes [#101](https://github.com/s3ntin3l8/branchdam-agent/issues/101)
* **ingest:** log os.Chtimes failures instead of silently swallowing ([#103](https://github.com/s3ntin3l8/branchdam-agent/issues/103)) ([#130](https://github.com/s3ntin3l8/branchdam-agent/issues/130)) ([92ef22c](https://github.com/s3ntin3l8/branchdam-agent/commit/92ef22c2d8a359ddf79a72a0e24fe08cb02b6429))
* **ingest:** restrict verify fallback to EINVAL/EOPNOTSUPP only ([#102](https://github.com/s3ntin3l8/branchdam-agent/issues/102)) ([#122](https://github.com/s3ntin3l8/branchdam-agent/issues/122)) ([cb8213b](https://github.com/s3ntin3l8/branchdam-agent/commit/cb8213b13cc0d0572f4dd6d0ddcdb2a30f3dd353))
* **ingest:** strip .. and bare . in sanitizeSegment ([#99](https://github.com/s3ntin3l8/branchdam-agent/issues/99)) ([#128](https://github.com/s3ntin3l8/branchdam-agent/issues/128)) ([cc407db](https://github.com/s3ntin3l8/branchdam-agent/commit/cc407dbc4788083940adb628e0d790be53cce86d))
* **test:** skip parity test on dirty server checkout ([#116](https://github.com/s3ntin3l8/branchdam-agent/issues/116)) ([f4157af](https://github.com/s3ntin3l8/branchdam-agent/commit/f4157afc07a2985d71f84979d9a98de13b719b50)), closes [#113](https://github.com/s3ntin3l8/branchdam-agent/issues/113)

## [1.3.0](https://github.com/s3ntin3l8/branchdam-agent/compare/v1.2.0...v1.3.0) (2026-08-29)


### Features

* **ingest:** synchronize naming template from server handshake and support POST /api/v1/agent/upload ([#74](https://github.com/s3ntin3l8/branchdam-agent/issues/74)) ([e554055](https://github.com/s3ntin3l8/branchdam-agent/commit/e5540555e7dfc20309f5a93f3f9bbd4ff05a361b)), closes [#71](https://github.com/s3ntin3l8/branchdam-agent/issues/71)

## [1.2.0](https://github.com/s3ntin3l8/branchdam-agent/compare/v1.1.1...v1.2.0) (2026-08-29)


### Features

* **config:** add integrations config schema for tray-configurable catalog sync ([#62](https://github.com/s3ntin3l8/branchdam-agent/issues/62)) ([de79a41](https://github.com/s3ntin3l8/branchdam-agent/commit/de79a4126b7b036ab45d3e0972b321087a3a5fea))
* **luminar:** luminar-sync reads integrations config for -catalog/-node-index ([#63](https://github.com/s3ntin3l8/branchdam-agent/issues/63)) ([b133520](https://github.com/s3ntin3l8/branchdam-agent/commit/b133520dfa7ab9b8473e1ee299f912e2992689ff))
* **resolve:** render-hook installer (embed, detect, install) ([#67](https://github.com/s3ntin3l8/branchdam-agent/issues/67)) ([36696f7](https://github.com/s3ntin3l8/branchdam-agent/commit/36696f76bb42e3b3cee93917219a7d8a293226d5))
* **tray:** draw the b-node monogram as the tray icon ([#51](https://github.com/s3ntin3l8/branchdam-agent/issues/51)) ([f740d35](https://github.com/s3ntin3l8/branchdam-agent/commit/f740d35b246aa233dec844269b3415d873acebbd))
* **tray:** integration syncer execution seam (Runner, scheduler, no menu) ([#64](https://github.com/s3ntin3l8/branchdam-agent/issues/64)) ([22f2f53](https://github.com/s3ntin3l8/branchdam-agent/commit/22f2f530eaeb17e49356cf926cb3a64878f56543))
* **tray:** Integrations settings menu ([#66](https://github.com/s3ntin3l8/branchdam-agent/issues/66)) ([e98e516](https://github.com/s3ntin3l8/branchdam-agent/commit/e98e5167b3e88e3bb795bb52cc49a85d640cfa20))
* **tray:** wire Resolve hook Install/Reveal menu items ([#70](https://github.com/s3ntin3l8/branchdam-agent/issues/70)) ([9c061af](https://github.com/s3ntin3l8/branchdam-agent/commit/9c061afb64a3ac8b80a55d778af619d66828faee))


### Bug Fixes

* **tray:** dialog file picker + close validate*Change allowlist gap ([#65](https://github.com/s3ntin3l8/branchdam-agent/issues/65)) ([9b710ab](https://github.com/s3ntin3l8/branchdam-agent/commit/9b710ab17772fbc61518b8729c9da3502c82be4f))

## [1.1.1](https://github.com/s3ntin3l8/branchdam-agent/compare/v1.1.0...v1.1.1) (2026-08-28)


### Bug Fixes

* **luminar:** rebuild catalog reader against a real Luminar Neo schema ([#49](https://github.com/s3ntin3l8/branchdam-agent/issues/49)) ([df5b27a](https://github.com/s3ntin3l8/branchdam-agent/commit/df5b27a478c37501d82acbf47681096c33d287e4))

## [1.1.0](https://github.com/s3ntin3l8/branchdam-agent/compare/v1.0.1...v1.1.0) (2026-08-28)


### Features

* checksum-verified self-update, install-and-restart, macOS .app bundle ([#29](https://github.com/s3ntin3l8/branchdam-agent/issues/29)) ([12557bc](https://github.com/s3ntin3l8/branchdam-agent/commit/12557bc7dd763cad31c3f66f2873bd77dcebbd5b))
* **ci:** add Hermes automated PR review workflow ([#37](https://github.com/s3ntin3l8/branchdam-agent/issues/37)) ([4231e90](https://github.com/s3ntin3l8/branchdam-agent/commit/4231e90f3d952f2f7faa62bf69cbba1e297db8ac))
* **config:** surgical config write-back, path discovery, and validation ([#36](https://github.com/s3ntin3l8/branchdam-agent/issues/36)) ([d3ccf3f](https://github.com/s3ntin3l8/branchdam-agent/commit/d3ccf3f74ab93e2467b7ada58cf518be5a448a58))
* **queue,ingest:** add queue.Counts aggregate and byte-progress plumbing ([#39](https://github.com/s3ntin3l8/branchdam-agent/issues/39)) ([f70cd38](https://github.com/s3ntin3l8/branchdam-agent/commit/f70cd38f6429dd7cf62310795ffe769b22baa7b6))
* **selfupdate:** add rollback support, hardware-verification checklist ([#44](https://github.com/s3ntin3l8/branchdam-agent/issues/44)) ([d3704c5](https://github.com/s3ntin3l8/branchdam-agent/commit/d3704c52d0bf02eb9cc02decdb1b4033c366deab))
* **tray:** real native settings menu ([#41](https://github.com/s3ntin3l8/branchdam-agent/issues/41)) ([eeb9cc1](https://github.com/s3ntin3l8/branchdam-agent/commit/eeb9cc1844cbf5e7920b925c0e05ca0d913bc638))
* **tray:** real offline-queue status readout and drain/prune timers ([#42](https://github.com/s3ntin3l8/branchdam-agent/issues/42)) ([89841d0](https://github.com/s3ntin3l8/branchdam-agent/commit/89841d013f7f952cf145b19c0dd7fdb7fdb33a54))
* **tray:** work out of the box -- startup diagnostics, first-run setup ([#38](https://github.com/s3ntin3l8/branchdam-agent/issues/38)) ([dc7a80c](https://github.com/s3ntin3l8/branchdam-agent/commit/dc7a80c9ed9ac734038c81a6b4c9cf1e9bbfd2b8))

## [1.0.1](https://github.com/s3ntin3l8/branchdam-agent/compare/v1.0.0...v1.0.1) (2026-08-27)


### Bug Fixes

* **ci:** publish release binaries from the release-please run ([#26](https://github.com/s3ntin3l8/branchdam-agent/issues/26)) ([0620ce2](https://github.com/s3ntin3l8/branchdam-agent/commit/0620ce25ee2a23978e398f57a44b2b1edcbe8432))

## 1.0.0 (2026-08-22)


### Features

* **ingest:** SD-card dual-copy writer, verified hashing, headless ingest core ([#10](https://github.com/s3ntin3l8/branchdam-agent/issues/10)) ([ea5cbe0](https://github.com/s3ntin3l8/branchdam-agent/commit/ea5cbe0c43bcae83956ae7f0b68649801caa5625)), closes [#2](https://github.com/s3ntin3l8/branchdam-agent/issues/2)
* **ingest:** unbuffered verify (F_NOCACHE/FILE_FLAG_NO_BUFFERING) on macOS/Windows ([#22](https://github.com/s3ntin3l8/branchdam-agent/issues/22)) ([8b8dab7](https://github.com/s3ntin3l8/branchdam-agent/commit/8b8dab7601410b4ab6badfe57a4564e62e2f751b))
* **luminar:** read-only catalog.db reader emitting Tier-2 DERIVED_FROM edges ([#9](https://github.com/s3ntin3l8/branchdam-agent/issues/9)) ([920cdcd](https://github.com/s3ntin3l8/branchdam-agent/commit/920cdcda75ad883ec9c0fd1cae89257458f17984)), closes [#6](https://github.com/s3ntin3l8/branchdam-agent/issues/6)
* **prune,branchdam:** add `prune` -- delete a verified file's LocalEditRoot mirror ([#24](https://github.com/s3ntin3l8/branchdam-agent/issues/24)) ([84b8768](https://github.com/s3ntin3l8/branchdam-agent/commit/84b876816d83ffd6daa47df9898d6c1050df7cbd))
* **queue:** offline queue.db and Tier-0/Tier-3 rebase handoff ([#13](https://github.com/s3ntin3l8/branchdam-agent/issues/13)) ([52ba766](https://github.com/s3ntin3l8/branchdam-agent/commit/52ba76685d2c790b83f45c5b8030e97abdc1c53b)), closes [#4](https://github.com/s3ntin3l8/branchdam-agent/issues/4)
* repo scaffold + branchDAM REST client + preflight ([#8](https://github.com/s3ntin3l8/branchdam-agent/issues/8)) ([be094b1](https://github.com/s3ntin3l8/branchdam-agent/commit/be094b18be8b8a6038c507216009ad40a1b0b28a))
* **resolve:** DaVinci Resolve post-render .dam.json hook ([#7](https://github.com/s3ntin3l8/branchdam-agent/issues/7)) ([2541398](https://github.com/s3ntin3l8/branchdam-agent/commit/2541398e2dc0d7232ff46fc7ce124f7fe9d160c7))
* **tray:** system tray shell around the ingest core ([#12](https://github.com/s3ntin3l8/branchdam-agent/issues/12)) ([2789fa6](https://github.com/s3ntin3l8/branchdam-agent/commit/2789fa640f8594c49c24727b85ab4bc0ff62e28c))


### Bug Fixes

* **ingest:** naming collisions and verify failure cleanup ([#23](https://github.com/s3ntin3l8/branchdam-agent/issues/23)) ([bd373bf](https://github.com/s3ntin3l8/branchdam-agent/commit/bd373bfb6803e192536ef6d23a905b9b263166d8))

## Changelog

All notable changes to this project are tracked here by
[release-please](https://github.com/googleapis/release-please), cut from
[Conventional Commits](https://www.conventionalcommits.org/).
