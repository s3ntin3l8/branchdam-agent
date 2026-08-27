# Changelog

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
