# Ingest service

Watches a source directory, transcodes large videos, and mirrors the result into a
destination directory — remembering what it has already done, so nothing is ever
copied twice.

It has replaced the **container syncthing** hop in this repo's pipeline (host temp
folder → `$STORAGE_DIR` on the external drive). That service is commented out in
`docker-compose.yml` rather than deleted, as a fallback; the host-side syncthing that
receives from the phones is untouched and still feeds the temp folder.

Unlike everything else here, ingest is **not** a container. It runs as a host binary
under launchd, because `hevc_videotoolbox` — Apple's hardware HEVC encoder, which is
what makes transcoding fast enough to be worth doing — is unreachable from a Linux
container on Docker Desktop.

## What it does

```
source/                              dest/
  Oleg/oleg-pixel/                     Oleg/oleg-pixel/
    VID_0001.mp4      ───transcode──►    VID_0001.mp4     (HEVC, ~30-70% of the size)
    .ingest.VID_0001.mp4.transcoded
    IMG_0002.jpg      ─────copy─────►    IMG_0002.jpg
    VID_0003.mp4      ─────copy─────►    VID_0003.mp4     (low bitrate, not worth it)
    ~syncthing~x.tmp  ─────skip
```

1. **Discover** — an fsnotify watcher for near-instant pickup, plus a periodic full
   rescan as a safety net. A file must hold a stable size for `scan.settle` before it
   is touched, so a half-downloaded file is never ingested.
2. **Filter** — anything matching an ignore rule is skipped. By default that is
   dotfiles/dirs, `*.tmp` (syncthing's in-progress downloads), and `@eaDir`.
3. **Decide** — videos above `min_video_bitrate` are transcoded; everything else is
   copied through untouched.
4. **Transcode** — ffmpeg to HEVC with the flags validated in
   `../../scripts/transcode-videos.sh`, then exiftool copies every tag across (minus
   `Rotation`, which ffmpeg has already baked into the pixels), then the original's
   mtime is restored. If the output is not smaller than the input, it is discarded and
   the original is copied instead.
5. **Copy** — written to a temp file and `rename`d into place, so the destination never
   shows a partial file to Immich or rclone.
6. **Record** — a row in SQLite, plus a sidecar file next to the source (below).

**The source file is never modified or deleted.** Pruning the source is still a manual
job (`../../scripts/delete_copied_files.sh`).

## Sidecar files

Ingest writes exactly two files into the source tree, both small, both only after
the work is finished. State itself lives in the database — these exist so you can
see what happened while standing in the directory:

| Sidecar | Meaning |
| --- | --- |
| `.ingest.VID.mp4.transcoded` | this file was re-encoded — empty, presence is the signal |
| `.ingest.VID.mp4.failed` | what went wrong, one line per attempt, with the stage and the error |

`.transcoded` appears **only** when ffmpeg actually ran, so it means exactly what it
says — files copied through untouched get no sidecar. A later success deletes a stale
`.failed`, so what is on disk is the current state, not a history.

Partial data never lands in the source tree. Both ffmpeg and the plain copy write
straight to a hidden temp file **in the destination** —
`.ingest.VID.part.mp4`, extension last because ffmpeg picks the container from it —
which is renamed into place once it is known to be good. Encoding directly into the
destination also saves a full read-and-write pass of the encoded video, and since the
temp file is already on the destination filesystem the rename is atomic, so Immich and
rclone never see a half-written file.

Both markers start with a dot, so they stay out of Finder and are already covered by
every ignore rule in this repo. The single `.ingest.` prefix is what makes them
manageable:

```bash
find "$SRC" -name '.ingest.*.failed'    # what went wrong, and where
find "$SRC" -name '.ingest.*' -delete   # forget everything, start clean
```

On startup ingest sweeps both trees for anything carrying the prefix that is not one of
those two records, clearing debris left by a run that died.

## State

`state.db` (SQLite, via `modernc.org/sqlite` — pure Go, no cgo) is the authority on what
has been ingested. A file is keyed by its **source-relative path plus its size** — no
mtime, because these files are never edited and a syncthing touch must not trigger a
re-copy, and no content hash, because reading every byte on every scan is not worth it.

This is what lets you **delete files from the destination** (or from the source) without
them coming back on the next scan — which is the whole point of the service.

`ingest setup` puts the DB at `<source>/.ingest/state.db`, beside the config and the
tree it describes. A config can instead set `state.dir`, in which case the DB lands at
`<state.dir>/<slug of the source path>/state.db` — a second source tree still gets its
own state, and several configs can share one `state.dir`. Two configs that resolve to
the same file are refused at startup: SQLite's WAL would let them interleave rather
than fail, which is the worst of both.

**Adoption**: a source file whose destination counterpart already exists is recorded as
`adopted` without being copied. Without this, a first run — or any run after the state
DB is lost — would re-ingest the entire existing library. Two ways it matches:

- `dest-exists-same-size` — the destination file is byte-for-byte the same size, so it
  was copied through.
- `dest-exists-transcoded` — the sizes differ, but the `.transcoded` marker beside the
  source says we produced that file. A transcode is *deliberately* a different size
  from its source, so size alone could never adopt one, and without the marker a lost
  state DB would re-encode and overwrite every video in the library.

Note that adoption is driven by walking the **source**: if a file is no longer in the
source tree, there is nothing to adopt and the destination copy is simply left alone.

**Destination marker**: ingest refuses to write until `dest` contains a `.ingest-dest`
file that you create by hand. On macOS an unmounted `/Volumes/...` leaves behind an
empty, writable directory — without the marker, the library would be silently written to
the boot disk. If the marker is missing the service does not exit; it tells you what to
run and waits for it.

## Install

```bash
brew install ffmpeg exiftool          # ffprobe comes with ffmpeg

cd services/ingest
make check                            # fmt + vet + test
make config                           # interview: writes <source>/.ingest/config.yaml
make install                          # build, install ~/Library/Scripts/photos-ingest
```

`make config` runs `ingest setup`, which asks for the source and destination —
defaulting the destination to `STORAGE_DIR` from the repo-root `.env` — and then
writes the config, creates the `.ingest-dest` marker, and adds `/.ingest` to the
source's `.stignore` if it is a syncthing folder. Every answer also has a flag,
and a flag that is passed is not asked about:

```bash
make config ARGS='-source ~/SyncPhones'              # asks about the rest
make config ARGS='-source ~/SyncPhones -dest /Volumes/EXTDATA/Photos -yes'
make config ARGS='-source ~/SyncPhones -force'       # overwrite an existing config
```

The binary is installed out of the repo on purpose, so a rebuild never swaps the
file launchd is running. `make` on its own lists every target; `./launchd.sh
bin-path`, `agent-path` and `log-path` print where things go, and `INGEST_BIN`,
`INGEST_LOG`, `INGEST_AGENT_DIR` and `INGEST_TZ` override them.

Without make:

```bash
go build -trimpath -o bin/ingest ./src
./bin/ingest setup -source ~/SyncPhones
cp bin/ingest ~/Library/Scripts/photos-ingest
```

### Where the config and the database live

Both go in `<source>/.ingest/`, so a source tree is self-contained: point the
binary at it and everything it needs is already there. Writing into the source
looks wrong at first glance, but the scanner never sees it — the default ignore
list matches both `.*` and `.ingest*`, and an ignored directory is never
descended into. The sweep matches the sidecar prefix `.ingest.`, *with* the
trailing dot, so the `.ingest` directory is never a candidate for deletion.

The two ignore rules overlap on purpose. `.*` is the one somebody might relax —
wanting hidden files ingested is a reasonable thing to want — so `.ingest*` is
the backstop that still keeps ingest's own config and database out of the
library. Note that these are globs, not regexes: the dot is literal, which is
why the pattern is `.ingest*` and not `.ingest.*` — the latter matches the
sidecars but not the directory.

The one real hazard is syncthing, which would treat a live SQLite database as
content and push it out to every phone sharing the folder. That is what the
`.stignore` entry is for, and why setup asks whether the source is a syncthing
folder.

## Run

```bash
CFG=~/SyncPhones/.ingest/config.yaml

photos-ingest -config $CFG -once              # one pass, then exit (dry-run by default)
photos-ingest -config $CFG -once -dry-run=false   # one real pass
photos-ingest -config $CFG                    # daemon: watch + periodic rescan
photos-ingest -config $CFG -report            # list failures and exit
photos-ingest -config $CFG -once -stop-before-action   # inventory only
photos-ingest -config $CFG -once -dont-ask    # skip the confirmation
```

### Several source trees

Different trees often want different rules, destinations and state. Write one config
file each and repeat `-config`:

```bash
photos-ingest -config ~/SyncPhones/.ingest/config.yaml -config ~/Cameras/.ingest/config.yaml
```

Each config becomes a **job** with its own filter, queue, scanner, state DB and
pipeline, and they run concurrently — a plain copy in one tree proceeds while the
other transcodes. Every log line carries `job=<name>`, taken from the config's `name:`
or its file name.

Run them as two processes instead and the encoder budget breaks: `transcode_workers`
is an in-process limit, so two agents each honouring `transcode_workers: 1` still put
two encodes on VideoToolbox — the one hardware engine the setting exists to protect.
In one process the limit is shared, and when the configs disagree the smallest wins
(logged at startup). That is the whole reason `-config` repeats rather than the agent
being installed twice — and why the LaunchAgent takes a list of source trees rather
than being installed once per tree.

Startup refuses the collisions that only arise here: two jobs with the same name,
overlapping source/dest trees, or a shared state database.

Before writing anything, ingest prints what it is about to do — source, destination,
state DB, plan, encoding rules — and waits for a `y`. One prompt covers every job:

```
about to run, for real — this writes to the destination

  phones
    from:     /Users/oleg/SyncPhones
    to:       /Volumes/EXTDATA/Photos
    state:    /Users/oleg/SyncPhones/.ingest/state.db
    plan:     watch the source and process continuously
    encoding: video-hevc (.mp4 .mov above 8 Mbps)

continue? [y/N]
```

It only asks when something will actually be written (a dry run is never gated) and
only when stdin is a terminal — under launchd there is nobody to answer, so it logs
`not a terminal, continuing without confirmation` and proceeds. `-dont-ask` skips it
explicitly, for scripts or an interactive run you are sure about.

`-stop-before-action` scans the source, decides what should happen to each file it
does not already know about — probing bitrates and matching rules, exactly as a real
run would — records that plan as `pending`, and stops. Nothing is copied, transcoded,
or written to the destination. The next regular run drains those rows.

```
planned rel=HIGH.mp4 action=transcode size=3000000 bitrate=19750194
planned rel=LOW.mp4  action=copy reason=low-bitrate size=2000000 bitrate=4000000
planned rel=IMG.jpg  action=copy reason=no-rule size=900000
run complete ... planned=3 plan_transcode=1 plan_copy=2
```

Use it to take stock of a source tree before committing CPU to it, or to split a large
first ingest into "decide" and "do" phases.

**The plan is advisory.** The run that drains a pending row decides again from the
config in force at that moment, so editing `min_video_bitrate` or the rules between
the two runs takes effect as you would expect — a recorded plan can never go stale in
a way that silently does the wrong thing. It also means the plan is a forecast, not a
promise: if you change the config, what happens may differ from what was recorded.

The `action`/`reason` columns therefore mean "what we expect to do" on a `pending` row
and "what was actually done" on a `done` one; the status tells you which you are
reading.

It records rather than simulates, so it needs `dry_run: false` (or `-dry-run=false`)
and refuses to run otherwise. Repeating it is harmless: rows are recorded without
touching the attempt counter, a re-run updates the plan, and a file already `done`
stays `done`.

Read the plan back at any time with `-report`:

```
$ photos-ingest -config $CFG -report
pending work
  copy            2 files      2.8 MB
  transcode       1 files      2.9 MB
  total           3 files      5.6 MB
```

Add `-v` to list the files themselves, grouped by planned action and largest first:

```
$ photos-ingest -config $CFG -report -v
pending work
  transcode       2 files      7.6 MB
  copy            2 files      2.8 MB
  total           4 files     10.4 MB

  copy           1.9 MB    4.0 Mbps  Kate/LOW.mp4  (low-bitrate)
  copy         878.9 KB              Kate/IMG.jpg  (no-rule)
  transcode      4.8 MB   19.8 Mbps  Oleg/oleg-pixel/HIGH2.mp4
  transcode      2.9 MB   19.8 Mbps  Oleg/oleg-pixel/HIGH.mp4
```

A blank bitrate means ffprobe could not determine one. `-report` lists outstanding
work first, then anything that failed.

`dry_run: true` is the default, per this repo's convention for anything one-way. A dry
run takes the identical decision path, prints what it would do, and writes nothing — no
files, no sidecars, and no state rows, so it can never poison the DB.

Verify a real run against the originals with the existing checker, which compares
creation dates, GPS, camera model, duration and dimensions:

```bash
../../scripts/verify-metadata.sh "$SRC" "$DST"
```

### launchd

```bash
make install-plist SOURCES=~/SyncPhones   # write the LaunchAgent and load it
make install-plist                        # again: reuses the same sources
make restart         # after a rebuild or a config change
make status          # launchctl print
make logs            # tail -f the log
make unload          # stop and unload
```

Everything launchd lives in `launchd.sh`; the Makefile only calls it. There is no
plist template and no substitution pass — the script writes the plist from a
heredoc, `plutil -lint`s it, and refuses to install one that does not pass. It
needs no `STORAGE_DIR`, because the generated config carries the resolved
destination path rather than `${STORAGE_DIR}`.

`SOURCES` is a list of **source directories**; each becomes one
`-config <src>/.ingest/config.yaml`, so several trees run as jobs in a single
process:

```bash
make install-plist SOURCES='~/SyncPhones ~/Cameras'
```

With no `SOURCES` the script reads the sources back out of the plist it installed
last time, which makes a reload after a rebuild a bare `make install-plist`. It
unloads any previous version first, so it is always safe to re-run. The
equivalent by hand is `launchctl bootstrap gui/$(id -u) <plist>`.

It is a LaunchAgent, not a LaunchDaemon: the external drive is mounted in the user
session. The plist must set `PATH` explicitly — launchd does not inherit your shell's,
and Homebrew's ffmpeg will not be found without it. Startup logs `ffmpeg -version` so
this failure is obvious rather than mysterious. Logs go to
`~/Library/Logs/photos-ingest.log`.

## Stopping it

Ctrl-C (or `SIGTERM`, which is what `launchctl bootout` sends) stops the service
promptly, wherever it is:

- a copy in progress is abandoned mid-file rather than run to completion — on a USB
  drive a multi-gigabyte file takes long enough that finishing it would be
  indistinguishable from a hang;
- ffmpeg is interrupted, and killed outright if it does not exit within 3 seconds;
- the confirmation prompt gives up waiting for an answer.

Nothing partial survives: the temp file in the destination is removed, the source is
untouched, and the row stays `pending` so the next run picks it up again. A second
Ctrl-C kills the process outright, in case shutdown ever wedges.

## Configuration

See `config.example.yaml` — every key is commented there. The settings worth knowing:

| Key | Default | Notes |
| --- | --- | --- |
| `dry_run` | `true` | flip once you have read a dry run's output |
| `scan.interval` | `15m` | full rescan; the watcher is the fast path, this is the net |
| `scan.settle` | `10s` | size must hold steady this long before a file is touched; a file untouched for longer than this skips the wait entirely |
| `name` | the file's base name | tags this tree's log lines; must be distinct across configs |
| `workers` | `2` | generic workers (probe + copy), per source tree |
| `transcode_workers` | `1` | concurrent ffmpeg runs — VideoToolbox is one hardware engine, raising this splits the same throughput. Process-wide: shared by every `-config`, smallest wins |
| `transcode.on_transcode_error` | `fallback` | `fallback` copies the original after `max_attempts`; `fail` parks the file |
| `transcode.on_metadata_error` | `warn` | `warn` keeps the transcode without full metadata; `fail` treats it as a transcode failure |
| `transcode.keep_if_larger` | `false` | discard an encode that came out bigger and copy the original |

`${VAR}` in a config is expanded from the environment, so a hand-written `dest:
${STORAGE_DIR}` stays in lockstep with the root `.env`. `ingest setup` resolves the
paths up front and writes them literally instead — that is what lets the LaunchAgent
carry no environment of its own, at the cost of re-running setup (or editing one line)
if `STORAGE_DIR` ever moves.

## Tests

```bash
go test ./src/...
```

Covers the filtering rules, config parsing and validation, the state key and retry
logic, the transcode decision branches (through a fake command runner, so no ffmpeg is
needed), and `ingest setup` — the generated config is loaded and validated by the same
code the daemon uses, so what users actually get is what is tested.

## Not implemented yet

- **Images.** The config's `transcode.rules` list is shaped to take an image rule, but
  only the video rule exists.
- **Source pruning.** `delete_copied_files.sh` compares source and destination by size
  and `cmp`, which stops being meaningful once a file has been re-encoded. The natural
  successor is an `ingest -prune` subcommand driven off the `done` rows.
