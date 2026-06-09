# Changelog

## [2.5.1](https://github.com/tammersaleh/calendar-sync/compare/v2.5.0...v2.5.1) (2026-06-09)


### Bug Fixes

* locate recurring mirror instances by id, not originalStart filter ([bfc4bb3](https://github.com/tammersaleh/calendar-sync/commit/bfc4bb3d3b35a1c3b23abb4d533cbe92f2bf2242))

## [2.5.0](https://github.com/tammersaleh/calendar-sync/compare/v2.4.0...v2.5.0) (2026-05-07)


### Features

* B17 Phase 2 propagate mirror-only recurring instance overrides ([0e38824](https://github.com/tammersaleh/calendar-sync/commit/0e38824a4a1f79eaa274dbc29f16d235cdf9a072))

## [2.4.0](https://github.com/tammersaleh/calendar-sync/compare/v2.3.0...v2.4.0) (2026-05-05)


### Features

* B17 target-syncToken for sub-tick target-edit propagation ([547032b](https://github.com/tammersaleh/calendar-sync/commit/547032b414c39e6bffdf423253d977d97c15cd4b))


### Bug Fixes

* don't advance target syncToken when inventory is missing ([4353e8d](https://github.com/tammersaleh/calendar-sync/commit/4353e8d7c29c5705ac2e35f1835129f19e563ed9))
* don't shadow recurring parent in inventory on inherited target-delta ([792ea07](https://github.com/tammersaleh/calendar-sync/commit/792ea078ddbeaacb51462f573b8d4494ea9a41c3))
* emit skip(source_orphan) for non-recurring target-delta 404 ([5853c8b](https://github.com/tammersaleh/calendar-sync/commit/5853c8b616e8adf31a5d04a0013b9722d89d3ac8))
* tolerate transient read errors in target-delta classify ([a25544f](https://github.com/tammersaleh/calendar-sync/commit/a25544f6e152af4359961dee7bff761f1ec15de5))
* trigger fast-track FullSync on target-token 410 GONE ([8868a25](https://github.com/tammersaleh/calendar-sync/commit/8868a25751a1ae87293bab997046b178d2c84a5f))

## [2.3.0](https://github.com/tammersaleh/calendar-sync/compare/v2.2.0...v2.3.0) (2026-05-05)


### Features

* PatchEvent type for explicit-clear merge patches ([f7db671](https://github.com/tammersaleh/calendar-sync/commit/f7db67133ec83f8a677af8bf12209f03bb0ef8dd))


### Bug Fixes

* --timeout bounds the full command, not just the run loop ([9f82a67](https://github.com/tammersaleh/calendar-sync/commit/9f82a673bef7f7e9c22a053cff044dcb00f9e901))
* address final-review findings on correctness pass ([507ce54](https://github.com/tammersaleh/calendar-sync/commit/507ce54ec1e8862e3434bd90705d7a05b8d24461))
* clear stale syncToken on FullSync when Google omits nextSyncToken ([083a170](https://github.com/tammersaleh/calendar-sync/commit/083a17083333a026dd3f560ad391c728efb0224a))
* degrade empty-fields propagate to stale_bookkeeping ([91337a5](https://github.com/tammersaleh/calendar-sync/commit/91337a571025b02f54099208d02d0c6bc78f178f))
* degrade empty-fields propagate to stale_bookkeeping in recurring handler ([8a1b6e5](https://github.com/tammersaleh/calendar-sync/commit/8a1b6e590f576a2e1603f3fe88edcba0073f96ff))
* install plist handles relative paths and XML metacharacters ([f4f7fc8](https://github.com/tammersaleh/calendar-sync/commit/f4f7fc8cc46cf2a23230a4c976dc5fdc82c9a1ba))
* respect [[pairs]].time_zone for all-day mirrored events ([e8f6d39](https://github.com/tammersaleh/calendar-sync/commit/e8f6d390a622907aa90791d7841d02f6d9937fac))
* retry rate-limited and backend gws calls per SPEC ([c13452f](https://github.com/tammersaleh/calendar-sync/commit/c13452f1d88e64c9bccf4c21d0a9e6367e5e2f3e))

## [2.2.0](https://github.com/tammersaleh/calendar-sync/compare/v2.1.9...v2.2.0) (2026-05-05)


### Features

* add CalendarRef type for string-or-table source/target ([ea238e0](https://github.com/tammersaleh/calendar-sync/commit/ea238e0cbf660554555a1bf0b9e59a197be3ecea))
* bounce launchd agent on brew upgrade via cask postflight ([66646af](https://github.com/tammersaleh/calendar-sync/commit/66646afc90347c9490b95c79d990b4fd2cf74b55))
* match calendar refs against summaryOverride and dataOwner ([a00ee0e](https://github.com/tammersaleh/calendar-sync/commit/a00ee0edb1961df64c1822f984dc582ee0cb5816))
* resolve summary-form calendar refs via CalendarListList ([fd3863f](https://github.com/tammersaleh/calendar-sync/commit/fd3863f691641fefb82f72307fd260573d2d0ec9))
* validate CalendarRef summary/account union rules ([6a7e55a](https://github.com/tammersaleh/calendar-sync/commit/6a7e55a5e79c7892e932c32a12fe7ba7eef4bb5e))


### Bug Fixes

* address F2 review findings ([23dc765](https://github.com/tammersaleh/calendar-sync/commit/23dc765d608a89345ae11ed614939a2c23352f02))
* clear stale union state in CalendarRef unmarshalers ([dcc9797](https://github.com/tammersaleh/calendar-sync/commit/dcc97978155347d758374490ef414d1325c67e07))
* honor account on single-summary-match for CalendarRef ([8baf219](https://github.com/tammersaleh/calendar-sync/commit/8baf2190b567038cda2dccc19693fd8ba20a07e8))
* type-assert inline-table fields in CalendarRef.UnmarshalTOML ([95b5b93](https://github.com/tammersaleh/calendar-sync/commit/95b5b936f03db15426870025fc058000cb284686))

## [2.1.9](https://github.com/tammersaleh/calendar-sync/compare/v2.1.8...v2.1.9) (2026-05-04)


### Bug Fixes

* preserve post-write mirror parent across recurring-handler errors ([3f4a28f](https://github.com/tammersaleh/calendar-sync/commit/3f4a28fec32d36b6717a0e1e4b1a1860bad47766))

## [2.1.8](https://github.com/tammersaleh/calendar-sync/compare/v2.1.7...v2.1.8) (2026-05-04)


### Bug Fixes

* detect source-mirror divergence with clean stored bookkeeping ([4cc79aa](https://github.com/tammersaleh/calendar-sync/commit/4cc79aab20e3380eb239d9c7d343599cbd0c64f2))

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
