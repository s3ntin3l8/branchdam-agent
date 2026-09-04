# Changelog

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
