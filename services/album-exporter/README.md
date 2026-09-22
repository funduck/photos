# album-exporter

Copies the photos of Immich albums into plain folders: in this setup, the "For Max" album goes to a folder that syncthing then pushes to a phone. It exports **each asset once**. Deleting a file from the folder, or removing a photo from the album, never brings the file back and never deletes anything.

Once is decided by a SQLite state DB keyed by **job name + Immich asset ID**, not by what is in the folder. That DB lives on the `album_exporter_state` named volume, deliberately outside the folder being synced and pruned. The idea is the same as ingest's state, with transcoding, file watching and launchd left out. Here the source is the Immich API, so there's nothing to watch: the service polls.

## Each pass, per job
0. Refuse a destination that ingest would copy back into the library. If the destination or any directory above it has an `.ingest/` directory, the pass fails. Ingest's config is not consulted, so an ignore rule there doesn't make such a destination acceptable. Otherwise every exported photo could come back into Immich as a duplicate. The check runs on every pass, dry run included, so ingest set up later is still caught.
1. Resolve the album by name through `GET /api/albums`. The name must match exactly one album the key can see (owned or shared). Otherwise use the album ID.
2. List its assets through `POST /api/search/metadata {albumIds}`, which is paginated.
3. Skip assets that are done, parked, or waiting out a retry backoff. For the rest, stream `GET /api/assets/{id}/original` into `.album-export.<stem>.part<ext>` in the destination, fsync, rename to the original file name, and set mtime to the capture time.
   - The exporter never overwrites an existing file. On a name clash the file becomes `<stem>-<first 8 of asset id><ext>`.
   - A failed download is retried with exponential backoff and parked after `max_attempts`.
   - Leftover `.part` files are swept at startup.
4. With `live_photo_video: true`, the motion half of a live photo is exported too, as its own asset.

## Setup
1. In Immich, create an API key under **Account Settings → API Keys** with `album.read`, `asset.read` and `asset.download`.
2. In the root `.env`, set `ALBUM_EXPORT_API_KEY` and set `ALBUM_EXPORT_DIR`, the host folder mounted at `/export`.
3. `cp config.example.yaml config.yaml` (gitignored). Every job's `dest` goes under `/export`. Do this before the first `up`: if the file is missing, compose creates a directory in its place.
4. `docker compose up -d --build album-exporter`, then read the log: dry run lists what it would export and writes nothing, not even state.
5. Set `dry_run: false` in `config.yaml` and run `docker compose restart album-exporter`.

## Operating it
```sh
docker compose logs -f album-exporter
docker compose run --rm album-exporter -once                    # one pass now
docker compose run --rm album-exporter -once -dry-run=false     # one real pass, config unchanged
docker compose run --rm album-exporter -report                  # per-job totals and failures
```
To export a photo again, delete its row: `DELETE FROM exports WHERE job='for-max' AND asset_id='…'`. Renaming a job gives it an empty history, so the whole album is exported again.

## Development
`make check` (fmt, vet, test). Tests run against an in-memory fake of the Immich endpoints (`src/export_test.go`).
