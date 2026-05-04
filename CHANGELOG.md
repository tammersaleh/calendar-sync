# Changelog

## [2.1.7](https://github.com/tammersaleh/calendar-sync/compare/v2.1.6...v2.1.7) (2026-05-04)


### Bug Fixes

* revive cancelled mirror when source is syncable ([433b0b4](https://github.com/tammersaleh/calendar-sync/commit/433b0b4a712b277411733902dc8d0d37a06ef2c2))

## [2.1.6](https://github.com/tammersaleh/calendar-sync/compare/v2.1.5...v2.1.6) (2026-05-04)


### Bug Fixes

* carry on through 410/404 in classify deleteOrSkip path ([985ddc7](https://github.com/tammersaleh/calendar-sync/commit/985ddc7f1bf906208f898a162be67f907c400135))

## [2.1.5](https://github.com/tammersaleh/calendar-sync/compare/v2.1.4...v2.1.5) (2026-05-04)


### Bug Fixes

* tolerate transient per-event read errors in classify loop ([d66b12e](https://github.com/tammersaleh/calendar-sync/commit/d66b12e81da25550d4115a839a96a4d00dbd4a42))

## [2.1.4](https://github.com/tammersaleh/calendar-sync/compare/v2.1.3...v2.1.4) (2026-05-03)


### Bug Fixes

* skip inherited recurring-instance mirrors in BuildInventory ([9be10d7](https://github.com/tammersaleh/calendar-sync/commit/9be10d74a34eaafde1d8dfb9480c5b581ab40748))

## [2.1.3](https://github.com/tammersaleh/calendar-sync/compare/v2.1.2...v2.1.3) (2026-05-03)


### Bug Fixes

* route inherited recurring-instance mirrors through bootstrap source-wins ([93a68b6](https://github.com/tammersaleh/calendar-sync/commit/93a68b6df3c8c83ad4f9cc88bf4fb55f72e5a94c))

## [2.1.2](https://github.com/tammersaleh/calendar-sync/compare/v2.1.1...v2.1.2) (2026-05-03)


### Bug Fixes

* normalize transparency/visibility in ManagedFieldsFromEvent for stable checksums ([9143cc4](https://github.com/tammersaleh/calendar-sync/commit/9143cc45f3a4e733468f9148a2ff088784693427))
* route any drift during migration to source-wins, not propagate ([8e96a3d](https://github.com/tammersaleh/calendar-sync/commit/8e96a3dc325943271c41a0034c58605ec240653c))
* use DriftedFieldNames-based comparison for migration drift recompute ([3d612a0](https://github.com/tammersaleh/calendar-sync/commit/3d612a0f4256b11c1be774735790bc3e7c00df44))

## [2.1.1](https://github.com/tammersaleh/calendar-sync/compare/v2.1.0...v2.1.1) (2026-05-03)


### Bug Fixes

* detect recurrence drift on parent mirrors ([ca97784](https://github.com/tammersaleh/calendar-sync/commit/ca97784d6dafd9f1f07f121e41a28c358736e0c5))
* normalize default transparency/visibility in drift comparison ([180abab](https://github.com/tammersaleh/calendar-sync/commit/180abab23fb9b112df996032fd5a2d4452eb6dc3))

## [2.1.0](https://github.com/tammersaleh/calendar-sync/compare/v2.0.0...v2.1.0) (2026-05-02)


### Features

* include location in managed fields (v3 schema) ([02e0c89](https://github.com/tammersaleh/calendar-sync/commit/02e0c89cb959d494dfe42eb790cb843f94b895a6))

## [2.0.0](https://github.com/tammersaleh/calendar-sync/compare/v1.1.3...v2.0.0) (2026-05-02)


### ⚠ BREAKING CHANGES

* configs containing `direction = "..."` on any [[pairs]] entry now fail validation. Remove the field for the new default (source-to-target); for prior bidirectional pairs, declare a second [[pairs]] entry with source and target swapped.

### Features

* add per-pair horizon override ([fcf1bf3](https://github.com/tammersaleh/calendar-sync/commit/fcf1bf33783db79564456dd65ae64b21f7db716d))
* add per-pair propagate_target_edits override ([81d0a9d](https://github.com/tammersaleh/calendar-sync/commit/81d0a9db72934f5e98da58eb7769911384fbc7c1))
* drop direction field from pair config ([88d77d0](https://github.com/tammersaleh/calendar-sync/commit/88d77d040d985664907e1f1225af03120fc71425))


### Bug Fixes

* starter config no longer includes removed direction field ([4149180](https://github.com/tammersaleh/calendar-sync/commit/414918015782b82a6c5e4a8a8fd17a72dd8cd25a))

## [1.1.3](https://github.com/tammersaleh/calendar-sync/compare/v1.1.2...v1.1.3) (2026-05-02)


### Bug Fixes

* orphan walker swallows ErrAPIGone like ErrAPINotFound (B14) ([fa52923](https://github.com/tammersaleh/calendar-sync/commit/fa52923cc457986bae4ec9d7b8a7f30fd15b47eb))

## [1.1.2](https://github.com/tammersaleh/calendar-sync/compare/v1.1.1...v1.1.2) (2026-05-02)


### Bug Fixes

* parse gws API error envelope from stdout, not stderr (B13) ([7cc7c0a](https://github.com/tammersaleh/calendar-sync/commit/7cc7c0a928d01dfad3953e9b98e9da267a1275df))

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
