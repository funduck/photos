#!/usr/bin/env python3
"""
Immich duplicate resolver.

Within each duplicate group, keeps the asset with the earliest capture
date and permanently deletes the rest with a single API call (force=true).
For Immich, force=true means an immediate physical delete: both the file
on disk and the DB record are removed together -- this applies to external
libraries too, as long as the volume isn't mounted read-only (':ro').

By default, a duplicate is only deleted if its filename matches the kept
asset's filename; pass --allow-name-mismatch to delete regardless of
filename. Every decision (kept/deleted/skipped) is appended to a log file
(--log-file, default duplicate_resolver.log).

Requirements:
  - The "Duplicate Detection" job must have run in Immich (it runs
    automatically when ML is enabled; check Administration -> Jobs,
    or Utilities -> Review duplicates).
  - An API key: Account Settings -> API Keys in Immich
    (needs at least asset.read, asset.delete permissions).

Usage:
  export IMMICH_URL="http://localhost:2283"
  export IMMITCH_DEDUP_API_KEY="..."
  python3 dedup_immich.py --dry-run     # just print the plan
  python3 dedup_immich.py --execute     # actually delete (not recoverable)
"""

import argparse
import logging
import os
import sys
from datetime import datetime, timezone

import requests

IMMICH_URL = os.environ.get("IMMICH_URL", "http://localhost:2283").rstrip("/")
API_KEY = os.environ.get("IMMITCH_DEDUP_API_KEY")

log = logging.getLogger("duplicate_resolver")


def setup_logging(log_file: str):
    log.setLevel(logging.INFO)
    handler = logging.FileHandler(log_file)
    handler.setFormatter(logging.Formatter("%(asctime)s %(levelname)s %(message)s"))
    log.addHandler(handler)


def api_headers():
    if not API_KEY:
        sys.exit("Set the IMMITCH_DEDUP_API_KEY environment variable")
    return {"x-api-key": API_KEY, "Accept": "application/json"}


def get_duplicates():
    r = requests.get(f"{IMMICH_URL}/api/duplicates", headers=api_headers())
    r.raise_for_status()
    return r.json()


def asset_date(asset: dict) -> datetime:
    for key in ("fileCreatedAt", "localDateTime"):
        v = asset.get(key)
        if v:
            try:
                return datetime.fromisoformat(v.replace("Z", "+00:00"))
            except ValueError:
                continue
    return datetime.max.replace(tzinfo=timezone.utc)


def delete_assets(ids: list[str]):
    # force=True -> permanent delete, bypassing the trash: the file on
    # disk and the Immich record are removed in one action.
    r = requests.delete(
        f"{IMMICH_URL}/api/assets",
        headers=api_headers(),
        json={"ids": ids, "force": False}, # TODO change to True for permanent delete
    )
    r.raise_for_status()


def main():
    parser = argparse.ArgumentParser()
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--dry-run", action="store_true", help="only print the plan")
    mode.add_argument("--execute", action="store_true", help="actually delete")
    parser.add_argument(
        "--log-file",
        default="duplicate_resolver.log",
        help="file to append a log of all operations to (default: duplicate_resolver.log)",
    )
    parser.add_argument(
        "--allow-name-mismatch",
        action="store_true",
        help=(
            "by default, only duplicates whose filename matches the kept "
            "asset's filename are deleted; pass this to delete duplicates "
            "regardless of filename"
        ),
    )
    args = parser.parse_args()

    setup_logging(args.log_file)
    log.info("run started mode=%s log_file=%s allow_name_mismatch=%s",
              "execute" if args.execute else "dry-run", args.log_file, args.allow_name_mismatch)

    groups = get_duplicates()
    print(f"Found duplicate groups: {len(groups)}")
    log.info("found %d duplicate groups", len(groups))

    total_delete = 0
    total_skipped = 0

    for g in groups:
        assets = g.get("assets", [])
        if len(assets) < 2:
            continue

        assets_sorted = sorted(assets, key=asset_date)
        keep = assets_sorted[0]
        drop = assets_sorted[1:]
        keep_name = keep.get("originalFileName")

        print(f"\nGroup {g.get('duplicateId')}:")
        print(f"  keeping: {keep_name} ({asset_date(keep)})")
        log.info("group %s keeping %s (%s)", g.get("duplicateId"), keep_name, asset_date(keep))

        to_delete = []
        for a in drop:
            name = a.get("originalFileName")
            if not args.allow_name_mismatch and name != keep_name:
                print(f"  skipping (name mismatch): {name} ({asset_date(a)})")
                log.info("group %s skipping id=%s name=%s (mismatch vs %s)",
                         g.get("duplicateId"), a.get("id"), name, keep_name)
                total_skipped += 1
                continue
            print(f"  deleting: {name} ({asset_date(a)})")
            log.info("group %s deleting id=%s name=%s", g.get("duplicateId"), a.get("id"), name)
            to_delete.append(a)

        total_delete += len(to_delete)

        if args.execute and to_delete:
            delete_assets([a["id"] for a in to_delete])
            log.info("group %s deleted %d asset(s)", g.get("duplicateId"), len(to_delete))

    verb = "Would delete" if args.dry_run else "Deleted"
    print(f"\n{verb} duplicates: {total_delete} (skipped due to name mismatch: {total_skipped})")
    log.info("run finished: %s duplicates=%d skipped=%d", verb.lower(), total_delete, total_skipped)


if __name__ == "__main__":
    main()
