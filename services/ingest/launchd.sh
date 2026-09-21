#!/usr/bin/env bash
#
# The launchd side of the ingest service: writes the LaunchAgent plist, installs
# the binary, and fronts launchctl. The Makefile does nothing but call this.
#
# Ingest is the one thing in this repo that is not a container — hevc_videotoolbox
# is unreachable from a Linux container on Docker Desktop — so it runs as a host
# binary under launchd.
#
#   ./launchd.sh install-bin bin/ingest       install the built binary
#   ./launchd.sh install /Users/me/SyncPhones install and load the agent
#   ./launchd.sh install                      reload, reusing the same sources
#   ./launchd.sh restart|status|logs|unload
#   ./launchd.sh uninstall
#
# Override any path from the environment: INGEST_BIN, INGEST_LOG, INGEST_TZ.

set -euo pipefail

LABEL="com.user.photos-ingest"

# Installed outside the repo on purpose, so a rebuild never swaps the file
# launchd is currently running.
BIN="${INGEST_BIN:-$HOME/Library/Scripts/photos-ingest}"
LOG="${INGEST_LOG:-$HOME/Library/Logs/photos-ingest.log}"
AGENT_DIR="${INGEST_AGENT_DIR:-$HOME/Library/LaunchAgents}"
AGENT="$AGENT_DIR/$LABEL.plist"
TZ_NAME="${INGEST_TZ:-Asia/Tbilisi}"

# launchd does not inherit your shell's PATH, and Homebrew's ffmpeg is invisible
# without this. It is the single most likely deployment failure, which is why
# ingest logs `ffmpeg -version` at startup.
AGENT_PATH="${INGEST_PATH:-/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin}"

DOMAIN="gui/$(id -u)"

# Where `ingest setup` puts a source tree's config.
config_for() { printf '%s/.ingest/config.yaml' "$1"; }

die() { echo "launchd.sh: $*" >&2; exit 1; }

# --- the plist -------------------------------------------------------------

# Everything is fixed except ProgramArguments, which carries one -config per
# source tree. Several trees must share one process: transcode_workers is an
# in-process semaphore over the single VideoToolbox engine, so two agents each
# set to 1 would run two encodes.
write_plist() {
    local out="$1"; shift

    local args=""
    local src cfg
    for src in "$@"; do
        cfg="$(config_for "$src")"
        [ -f "$cfg" ] || die "no config at $cfg — run: make config ARGS='-source $src'"
        args+="        <string>-config</string>
        <string>$cfg</string>
"
    done

    cat > "$out" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!--
  Written by services/ingest/launchd.sh. Do not edit: the next install
  overwrites it. Change the script instead.

  A LaunchAgent, not a LaunchDaemon: the external drive is mounted in the user
  session, and Full Disk Access prompts are resolvable for an agent.
-->
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$LABEL</string>

    <key>ProgramArguments</key>
    <array>
        <string>$BIN</string>
$args        <!-- Harmless here: launchd gives the service /dev/null for stdin, which
             is not a terminal, so the confirmation is skipped anyway. Explicit
             is better than relying on that. -->
        <string>-dont-ask</string>
    </array>

    <key>EnvironmentVariables</key>
    <dict>
        <!-- Mandatory: launchd does not inherit your shell's PATH, so Homebrew's
             ffmpeg is invisible without this. -->
        <key>PATH</key>
        <string>$AGENT_PATH</string>
        <key>TZ</key>
        <string>$TZ_NAME</string>
    </dict>

    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ThrottleInterval</key>
    <integer>30</integer>

    <!-- Keeps a two-hour transcode from making the desktop sluggish. Do NOT set
         ProcessType to Background: that gets aggressively CPU-throttled, which
         is exactly wrong for transcoding. -->
    <key>Nice</key>
    <integer>5</integer>

    <!-- kqueue costs a file descriptor per watched directory. -->
    <key>SoftResourceLimits</key>
    <dict>
        <key>NumberOfFiles</key>
        <integer>8192</integer>
    </dict>

    <key>StandardOutPath</key>
    <string>$LOG</string>
    <key>StandardErrorPath</key>
    <string>$LOG</string>
</dict>
</plist>
PLIST
}

# installed_sources recovers the source trees from the plist already in place, so
# a plain `./launchd.sh install` after a rebuild reloads the same set. The paths
# are <src>/.ingest/config.yaml, so stripping that suffix gives the source back.
installed_sources() {
    [ -f "$AGENT" ] || return 0
    sed -n 's|^ *<string>\(/.*\)/\.ingest/config\.yaml</string>$|\1|p' "$AGENT"
}

# --- subcommands -----------------------------------------------------------

cmd_install_bin() {
    local built="${1:-}"
    [ -n "$built" ] || die "usage: launchd.sh install-bin <path to the built binary>"
    [ -f "$built" ] || die "no such file: $built"

    mkdir -p "$(dirname "$BIN")"
    # Replacing a running binary in place fails with ETXTBSY; the rename is atomic.
    cp "$built" "$BIN.new"
    chmod +x "$BIN.new"
    mv -f "$BIN.new" "$BIN"
    echo "installed $BIN"
    echo "if the agent is loaded, pick it up with:  make restart"
}

cmd_install() {
    local sources=("$@")
    if [ ${#sources[@]} -eq 0 ]; then
        local recovered
        recovered="$(installed_sources)"
        [ -n "$recovered" ] || die "no source given and none in $AGENT
  usage: launchd.sh install <source dir> [source dir ...]"
        # shellcheck disable=SC2206
        sources=($recovered)
        echo "reusing the sources already in the agent: ${sources[*]}"
    fi

    [ -x "$BIN" ] || die "no binary at $BIN — run 'make install' first"

    mkdir -p "$AGENT_DIR" "$(dirname "$LOG")"
    local tmp="$AGENT.new"
    write_plist "$tmp" "${sources[@]}"
    # A malformed plist otherwise fails as a mysterious launchctl error later.
    plutil -lint "$tmp" >/dev/null || { rm -f "$tmp"; die "generated an invalid plist"; }
    mv -f "$tmp" "$AGENT"
    echo "installed $AGENT"

    # launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
    # launchctl bootstrap "$DOMAIN" "$AGENT"
    # echo "loaded $LABEL for: ${sources[*]}"
    # echo "follow it with:  make logs"
}

cmd_uninstall() {
    launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
    rm -f "$AGENT" "$BIN"
    echo "removed $AGENT and $BIN"
    echo "configs, state databases and logs are left in place"
}

case "${1:-help}" in
    install-bin) shift; cmd_install_bin "$@" ;;
    install)     shift; cmd_install "$@" ;;
    uninstall)   cmd_uninstall ;;
    load)        launchctl bootstrap "$DOMAIN" "$AGENT" ;;
    unload)      launchctl bootout "$DOMAIN/$LABEL" ;;
    restart)     launchctl kickstart -k "$DOMAIN/$LABEL" ;;
    status)      launchctl print "$DOMAIN/$LABEL" ;;
    logs)        tail -f "$LOG" ;;
    bin-path)    echo "$BIN" ;;
    agent-path)  echo "$AGENT" ;;
    log-path)    echo "$LOG" ;;
    help|-h|--help)
        awk 'NR>1 && /^#/ { sub(/^# ?/, ""); print; next } NR>1 { exit }' "$0"
        ;;
    *) die "unknown command: $1 (try: help)" ;;
esac
