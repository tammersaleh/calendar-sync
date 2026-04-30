#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# ///
"""Measure the API cost of a full events.list per calendar over a 1-year horizon.

Mirrors the parameters Phase 1 of SPEC.md would use for the source-side full
list: singleEvents=false, showDeleted=true, eventTypes=[default, outOfOffice,
focusTime], maxResults=250. Pages manually so per-page latency is recorded.

Usage:
    scripts/measure-list-cost.py <calendarId> [<calendarId> ...]

Discovery (to pick calendar IDs):
    gws calendar calendarList list --format json | jq -r '.items[] | "\\(.accessRole)\\t\\(.id)\\t\\(.summary)"'
"""

from __future__ import annotations

import json
import subprocess
import sys
import time
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone

HORIZON_DAYS = 365
PAGE_SIZE = 250
EVENT_TYPES = ["default", "outOfOffice", "focusTime"]


def fmt_iso(t: datetime) -> str:
    return t.replace(microsecond=0).isoformat().replace("+00:00", "Z")


def call_gws(params: dict) -> dict:
    proc = subprocess.run(
        ["gws", "calendar", "events", "list", "--params", json.dumps(params)],
        capture_output=True,
        text=True,
        check=False,
    )
    if proc.returncode != 0:
        raise RuntimeError(
            f"gws exited {proc.returncode}: {proc.stderr.strip() or proc.stdout.strip()}"
        )
    return json.loads(proc.stdout)


@dataclass
class Result:
    calendar: str
    events: int = 0
    pages: int = 0
    page_times_ms: list[int] = field(default_factory=list)
    error: str | None = None

    @property
    def total_ms(self) -> int:
        return sum(self.page_times_ms)


def measure(calendar_id: str, time_min: str, time_max: str) -> Result:
    r = Result(calendar=calendar_id)
    page_token: str | None = None

    while True:
        params: dict = {
            "calendarId": calendar_id,
            "timeMin": time_min,
            "timeMax": time_max,
            "singleEvents": False,
            "showDeleted": True,
            "eventTypes": EVENT_TYPES,
            "maxResults": PAGE_SIZE,
        }
        if page_token:
            params["pageToken"] = page_token

        try:
            start = time.monotonic_ns()
            resp = call_gws(params)
            end = time.monotonic_ns()
        except RuntimeError as e:
            r.error = str(e)
            return r

        page_ms = (end - start) // 1_000_000
        items = resp.get("items") or []
        r.events += len(items)
        r.pages += 1
        r.page_times_ms.append(page_ms)
        print(
            f"  page {r.pages}: {len(items)} events, {page_ms} ms",
            file=sys.stderr,
            flush=True,
        )

        page_token = resp.get("nextPageToken")
        if not page_token:
            return r


def main(argv: list[str]) -> int:
    if len(argv) < 2:
        print(__doc__, file=sys.stderr)
        return 64

    now = datetime.now(timezone.utc)
    time_min = fmt_iso(now)
    time_max = fmt_iso(now + timedelta(days=HORIZON_DAYS))

    print(
        f"window: {time_min} -> {time_max} ({HORIZON_DAYS} days)",
        file=sys.stderr,
    )

    results: list[Result] = []
    for cal in argv[1:]:
        print(f"\n=== {cal} ===", file=sys.stderr, flush=True)
        r = measure(cal, time_min, time_max)
        results.append(r)
        if r.error:
            print(f"ERROR: {r.error}", file=sys.stderr)
            continue
        avg = r.total_ms // max(r.pages, 1)
        print(
            f"calendar={cal} events={r.events} pages={r.pages} "
            f"total_ms={r.total_ms} avg_page_ms={avg}",
            file=sys.stderr,
        )

    summary = [
        {
            "calendar": r.calendar,
            "events": r.events,
            "pages": r.pages,
            "total_ms": r.total_ms,
            "page_times_ms": r.page_times_ms,
            "error": r.error,
        }
        for r in results
    ]
    print(json.dumps(summary, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
