# Event-list timing measurements

Date: 2026-04-30. Measured against real Google calendars to back the architectural decision for the daemon vs. one-shot polling model. Reproduce with `scripts/measure-list-cost.py <calendar_id>...`.

## Setup

- 1-year horizon (`timeMin = now`, `timeMax = now + 365d`).
- `singleEvents = false` (recurring events return as parents, not expanded).
- `eventTypes = [default, outOfOffice, focusTime]` (matches SPEC.md filter).
- `maxResults = 250` per page.

## Results

| Calendar | accessRole | Events | Pages | Total ms | Avg ms/page |
|---|---|---|---|---|---|
| Personal Google account, primary | writer | 1268 | 6 | 3955 | 659 |
| Work Google Workspace, primary | owner | 788 | 4 | 2839 | 709 |
| TripIt iCal subscription | reader | 8 | 1 | 270 | 270 |
| Group calendar (light shared) | reader | 3 | 1 | 332 | 332 |
| Empty mirror-list (filter `calendar-sync:version=2` on a calendar with no mirrors) | - | 0 | 1 | 302 | 302 |

Per-page latency clusters around 660-710ms regardless of payload size. Empty/single-page calls land around 270-330ms; the marginal cost of one page is mostly fixed overhead.

## Implications for architecture

These numbers killed pure-stateless polling. Cost ceiling for a 4-pdir setup (work ↔ personal bidirectional, work → family, personal → family):

| Strategy | Source lists | Mirror lists | Total wall-clock per cycle |
|---|---|---|---|
| Naive (no dedup) | 4× full = ~13.6s | 4× target scans = ~6s | ~20s |
| Source dedup | 2× full = ~6.8s | 4× target scans = ~6s | ~13s |
| Source + target dedup | 2× full = ~6.8s | 3× full = ~6.1s | ~13s |

At 60s polling = 22% wall-clock busy. Tight.

The long-running daemon (`calendar-sync watch`) collapses steady-state to ~1s per cycle by keeping the source events, mirror inventories, and sync tokens in process memory. Per-cycle cost: 2-3 incremental `events.list` calls (~270ms each for empty deltas). Cold-start cost: ~13-15s, paid once at process launch.

Daily API call volume at 60s polling with the daemon: ~3 calls/cycle × 1440 cycles/day = ~4,300 calls/day. Google quota is 1M/day per project. ~0.4% utilization.

This is the data that drove the SPEC.md decision to ship `calendar-sync watch` as the primary deployment with `KeepAlive=true`, not a one-shot `run` driven by `StartInterval`.
