# person-album

Adds the photos of chosen Immich people to albums: in this setup, every photo with Max goes to the "Max" album, which album-exporter can then copy to a folder for a phone. It adds **each asset once**. Removing a photo from the album, for example a face Immich got wrong, never brings it back. The service never removes anything from an album.

Once is decided by a SQLite state DB keyed by **job name + Immich asset ID**, not by what is in the album. That DB lives on the `person_album` named volume.

Immich workflows can't do this themselves (as of 3.x). There's no person filter, and face recognition runs as a background job after upload, so an upload trigger fires before any faces are known. Polling sidesteps both: a photo is added one poll after recognition has tagged it.

## Each pass, per job
Every request uses the job's own `api_key`. Immich keeps separate people and albums for each user, so a job runs as one account: its key decides whose faces are searched and whose albums are seen.

1. Resolve the album by name through `GET /api/albums`. The name must match exactly one album the key can see (owned or shared). Otherwise use the album ID. A missing album is an error: the service never creates one.
2. Resolve each person by name through `GET /api/people` (hidden people included). The name must match exactly one person, otherwise use the person ID.
3. List the matching assets through `POST /api/search/metadata {personIds}`, which is paginated. Immich ANDs `personIds`. So `match: all` is one search with every person, and `match: any` is one search per person, merged. Only timeline assets are listed: archived, hidden (such as the video half of a live photo), locked and trashed ones are left out.
4. Skip assets that are done, parked, or waiting out a retry backoff. Add the rest with `PUT /api/albums/{id}/assets`, in batches of 500.
   - `duplicate` (already in the album) counts as done, so removing the photo later sticks.
   - Any other per-asset error (`no_permission`, `not_found`, …), or a failed request for the whole batch, is retried with exponential backoff and parked after `max_attempts`.

## Setup
1. In Immich, create the album. Logged in as each account that jobs run as, create an API key under **Account Settings → API Keys** with `person.read`, `asset.read`, `album.read` and `albumAsset.create`.
2. In the root `.env`, set `PERSON_ALBUM_API_KEY_OLEG` / `PERSON_ALBUM_API_KEY_KATE`. `docker-compose.yml` passes exactly those into the container; for another account, add a variable there too.
3. `cp config.example.yaml config.yaml` (gitignored) and give every job its `api_key: ${PERSON_ALBUM_API_KEY_…}`. Do this before the first `up`: if the file is missing, compose creates a directory in its place.
4. The service is behind the `person-album` compose profile. Add it to `COMPOSE_PROFILES` in `.env` (comma-separated), or name the service explicitly as below. Run `docker compose up -d --build person-album`, then read the log: dry run lists what it would add and changes nothing, not even state.
5. Set `dry_run: false` in `config.yaml` and run `docker compose restart person-album`.

## Operating it
```sh
docker compose logs -f person-album
docker compose run --rm person-album -once                    # one pass now
docker compose run --rm person-album -once -dry-run=false     # one real pass, config unchanged
docker compose run --rm person-album -report                  # per-job totals and failures
```
To have a removed photo added again, delete its row: `DELETE FROM additions WHERE job='max' AND asset_id='…'`. Renaming a job gives it an empty history, so every match is added again, including photos removed from the album earlier.

## Development
`make check` (fmt, vet, test). Tests run against an in-memory fake of the Immich endpoints (`src/sync_test.go`).
