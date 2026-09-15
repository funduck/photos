#!/usr/bin/env python3
"""Periodically runs `rclone copy` to back up photos to S3, with catch-up
after downtime: it persists the last-sync time and, on every check, syncs
immediately if the configured interval has already elapsed (whether that's
because a run is simply due, or because the container was stopped past it)
rather than only firing on wall-clock schedule matches like plain cron.
"""

import os
import re
import signal
import subprocess
import sys
import threading
import time
from datetime import timedelta

RCLONE_CONF_PATH = "/tmp/rclone.conf"
POLL_INTERVAL_SECONDS = 60

DURATION_RE = re.compile(r"^(\d+)([smhd])$")
DURATION_UNITS = {"s": 1, "m": 60, "h": 3600, "d": 86400}

stop_event = threading.Event()
current_proc: subprocess.Popen | None = None


def handle_stop_signal(signum, frame) -> None:
    print(f"[sync] received signal {signum}, shutting down", flush=True)
    stop_event.set()
    if current_proc is not None:
        current_proc.terminate()


def parse_duration(value: str) -> int:
    match = DURATION_RE.match(value.strip())
    if not match:
        raise ValueError(f"invalid duration {value!r}, expected e.g. '24h', '30m', '1d'")
    amount, unit = match.groups()
    return int(amount) * DURATION_UNITS[unit]


def require_env(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise SystemExit(f"missing required env var: {name}")
    return value


def write_rclone_conf(remote: str, access_key: str, secret_key: str, region: str) -> None:
    with open(RCLONE_CONF_PATH, "w") as f:
        f.write(
            f"[{remote}]\n"
            "type = s3\n"
            "provider = AWS\n"
            f"access_key_id = {access_key}\n"
            f"secret_access_key = {secret_key}\n"
            f"region = {region}\n"
        )
    os.chmod(RCLONE_CONF_PATH, 0o600)


def read_last_sync(state_file: str) -> float:
    try:
        with open(state_file) as f:
            return float(f.read().strip())
    except (FileNotFoundError, ValueError):
        return 0.0


def write_last_sync(state_file: str, when: float) -> None:
    os.makedirs(os.path.dirname(state_file), exist_ok=True)
    with open(state_file, "w") as f:
        f.write(str(when))


def run_sync(remote: str, bucket: str, sync_name: str, dry_run: bool) -> bool:
    global current_proc

    cmd = [
        "rclone", "copy",
        "/data", f"{remote}:{bucket}/{sync_name}",
        "--config", RCLONE_CONF_PATH,
        "--s3-storage-class", "DEEP_ARCHIVE",
        "--size-only",
        "--fast-list",
        "--transfers", "8",
        "--checkers", "16",
        "--progress",
        "--exclude", "._*",
        "--exclude", ".DS_Store",
        "--exclude", ".Spotlight-V100/**",
        "--exclude", ".Trashes/**",
        "--exclude", ".fseventsd/**",
        "--exclude", ".TemporaryItems/**",
    ]
    if dry_run:
        cmd.append("--dry-run")

    print(f"[sync] running: {' '.join(cmd)}", flush=True)
    current_proc = subprocess.Popen(cmd, stdout=sys.stdout, stderr=sys.stderr)
    returncode = current_proc.wait()
    current_proc = None

    if returncode != 0:
        print(f"[sync] rclone copy failed with exit code {returncode}", flush=True)
        return False
    return True


def main() -> None:
    signal.signal(signal.SIGTERM, handle_stop_signal)
    signal.signal(signal.SIGINT, handle_stop_signal)

    remote = require_env("RCLONE_REMOTE")
    access_key = require_env("RCLONE_ACCESS_KEY")
    secret_key = require_env("RCLONE_SECRET_ACCESS_KEY")
    bucket = require_env("AWS_BUCKET")
    region = os.environ.get("AWS_REGION", "us-east-1")

    sync_name = os.environ.get("SYNC_NAME", "Photos")
    interval = parse_duration(os.environ.get("SYNC_INTERVAL", "24h"))
    dry_run = os.environ.get("DRY_RUN", "true").lower() != "false"
    state_file = os.environ.get("STATE_FILE", f"/state/{sync_name}/last_sync.txt")

    write_rclone_conf(remote, access_key, secret_key, region)

    last_sync = read_last_sync(state_file)
    next_sync = last_sync + interval
    next_sync_in = timedelta(seconds=max(0, next_sync - time.time()))

    print(
        f"[sync] starting: interval={interval}s dry_run={dry_run} "
        f"state_file={state_file}",
        f"next sync at {time.ctime(next_sync)} in {next_sync_in}",
        flush=True,
    )

    while not stop_event.is_set():
        last_sync = read_last_sync(state_file)
        elapsed = time.time() - last_sync

        if elapsed >= interval:
            now = time.time()
            succeeded = run_sync(remote, bucket, sync_name, dry_run)
            if stop_event.is_set():
                break
            if succeeded:
                last_sync = now
            else:
                # Retry sooner rather than waiting a full interval after a failure.
                last_sync = now - interval + POLL_INTERVAL_SECONDS
            write_last_sync(state_file, last_sync)
            next_sync = last_sync + interval
            next_sync_in = timedelta(seconds=max(0, next_sync - time.time()))
            print(
                f"[sync] next sync at {time.ctime(next_sync)} in {next_sync_in}",
                flush=True,
            )
            sleep_for = POLL_INTERVAL_SECONDS
        else:
            sleep_for = min(interval - elapsed, POLL_INTERVAL_SECONDS)

        # stop_event.wait returns as soon as a signal handler sets it, instead
        # of blocking for the full poll interval like time.sleep would.
        stop_event.wait(max(sleep_for, 1))

    print("[sync] stopped", flush=True)


if __name__ == "__main__":
    main()
