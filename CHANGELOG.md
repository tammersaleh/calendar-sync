# Changelog

## [1.1.1](https://github.com/tammersaleh/calendar-sync/compare/v1.1.0...v1.1.1) (2026-05-02)


### Bug Fixes

* skip status=cancelled tombstones in BuildInventory (B11) ([a0552d9](https://github.com/tammersaleh/calendar-sync/commit/a0552d9017b0842a36ca00f092242492508a0386))

## [1.1.0](https://github.com/tammersaleh/calendar-sync/compare/v1.0.0...v1.1.0) (2026-05-02)


### Features

* add --prune-horizon flag to mirror prune ([56cff11](https://github.com/tammersaleh/calendar-sync/commit/56cff116258513fff98f3e3015783b3a1424597a))
* WatchPaths config-reload via launchd plist ([ccbcaf8](https://github.com/tammersaleh/calendar-sync/commit/ccbcaf8211943dd4727397867e435d72d0cc1ac7))


### Bug Fixes

* dedupe source-tuples in runClassifyLoop (B2 cause B) ([e1e1f4d](https://github.com/tammersaleh/calendar-sync/commit/e1e1f4d757e6c6335f6c171516ed39530651ebe6))
* dryRunAPI EventsPatch merges into cached Insert resource (B2 cause A) ([f6d9f1c](https://github.com/tammersaleh/calendar-sync/commit/f6d9f1c659ed06078bed6d9e816a75692f49fe38))
* short-circuit kong --help / --version before subcommand dispatch ([9705671](https://github.com/tammersaleh/calendar-sync/commit/970567130765b65f8d519ff3e6ab3b1079981e6e))
* skip status=cancelled events in mirror prune candidate list ([c70ca48](https://github.com/tammersaleh/calendar-sync/commit/c70ca48ea9552f00559c4cfb77a5ed65c6acc400))
* surface joinError cause in partial_failure stderr envelope ([803317c](https://github.com/tammersaleh/calendar-sync/commit/803317c3574b7a9346754a894a14ec878b9fc448))
* surface underlying error in partial_failure stderr envelope ([5a412e6](https://github.com/tammersaleh/calendar-sync/commit/5a412e6c82334552826dbe2147c5b09b1d5cff97))
* wire [settings].dry_run to dryRunAPI wrapper in run / watch ([aa01edf](https://github.com/tammersaleh/calendar-sync/commit/aa01edf9af510b85c5d7e377b47323d28c45850f))

## 1.0.0 (2026-05-01)


### Features

* gate two-way sync behind propagate_target_edits setting ([828fe72](https://github.com/tammersaleh/calendar-sync/commit/828fe72293948149d4d99671e03082dc2554b7f3))
* ship cmd/ kong CLI with full subcommand surface ([7700545](https://github.com/tammersaleh/calendar-sync/commit/7700545f19e50ade0b7dd5807ae5128684ee8e3d))


### Bug Fixes

* **ci:** tolerate missing go.sum in tidy check ([bd2c271](https://github.com/tammersaleh/calendar-sync/commit/bd2c271cf0650c778039b27ba56b428177624c2d))

## Changelog
