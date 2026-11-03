# Streamforge

A small asynchronous video-on-demand backend for a local interview demo. Go's
standard HTTP router accepts uploads; a separate Go worker polls PostgreSQL and
creates 360p and 720p H.264/AAC HLS with FFmpeg. No queue broker or cloud account.

## Start

Requires Docker Engine with Compose. Host Go 1.26+, FFmpeg (with libx264), ffprobe,
Python 3 and curl are needed for the verification commands, not to run the app.

```sh
cp .env.example .env
docker compose up --build -d
# Open http://localhost:8080
./scripts/sample.sh
# Upload samples/demo.mp4 or samples/silent.mp4 through the page.
docker compose logs -f api worker
```

Only the API port is published, bound to localhost. PostgreSQL and media are named
volumes. The SQL migration runs on first creation of the database volume. This is
an initial-schema demo, not a general migration runner. Rebuild after code changes.

```sh
docker compose down             # stop, preserve data
docker compose down -v          # RESET: permanently delete demo database + media
```

The UI uses native HLS when available, otherwise hls.js **1.6.13** from jsDelivr.
The latter requires internet access. Go module and container builds also need
network access initially. There is no JavaScript build step.

## Workflow and layout

```text
multipart upload -> source temporary file -> rename -> INSERT queued -> HTTP 202
                                                        |
worker: short claim transaction / SKIP LOCKED -> COMMIT -> ffprobe -> FFmpeg
                                                        |
             ready <- UPDATE row <- rename complete output <- verify playlists
```

- `cmd/api`: dependency wiring, HTTP server, SIGTERM shutdown.
- `cmd/worker`: one job at a time; also the manual requeue command.
- `internal/httpapi`: streaming upload, JSON API, media routes, embedded page.
- `internal/store`: pgx pool, persistence, short claim transaction.
- `internal/media`: input inspection, bounded child-process output, HLS publishing.
- `internal/worker`: poll loop and final-state recording.
- `migrations/001_videos.sql`: the single metadata/job table.

There are only two small interfaces at the HTTP/worker boundaries, for focused
checks without a database or child process. No transaction or database connection
is retained while FFmpeg runs. Claims use `FOR UPDATE SKIP LOCKED`; worker
concurrency is one. ffprobe validates the container, duration, dimensions, aspect
ratio and rotation metadata. Extensions and MIME types are not trusted.

Supported demo input: a decodable MP4 with landscape square-pixel video, height
720–2160, width at most 4096, duration greater than zero and at most 60 seconds.
Rotated/anamorphic/portrait inputs are rejected. First video and first audio stream
are selected; subtitles and extra streams are ignored. Audio is optional. Invalid
media is accepted as a queued upload, then fails asynchronously with a safe message.

Each rendition preserves aspect ratio using `scale=-2:height`, uses even dimensions,
yuv420p, 30 fps, closed GOPs, scene-cut keyframes disabled and forced keyframes every
4 seconds. Renditions are encoded sequentially under one overall deadline, keeping
CPU use bounded (two encoder threads). Segments are MPEG-TS, playlists are complete
VOD (`ENDLIST`). The fixed master advertises both bandwidths. All playlists and
nonempty referenced local segments are checked before directory rename. The API
requires `ready` and allows only the expected playlists/segment names, so temporary
and orphaned output is never served. Sources are never exposed.

See the [FFmpeg HLS muxer documentation](https://ffmpeg.org/ffmpeg-formats.html#hls-2)
and [pgxpool documentation](https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool)
for the underlying muxer and transaction APIs.

## API

```sh
curl -i -F 'file=@samples/demo.mp4' -F 'title=Interview demo' localhost:8080/api/videos
# 202, Location: /api/videos/UUID, {"id":"UUID","status":"queued"}
curl localhost:8080/api/videos
curl localhost:8080/api/videos/UUID
curl localhost:8080/healthz
curl localhost:8080/readyz
```

| Endpoint | Result |
| --- | --- |
| `POST /api/videos` | One `file` and optional `title`; HTTP 202 with ID/status |
| `GET /api/videos` | Up to 50 recent records, newest first |
| `GET /api/videos/{id}` | ID/title/status, created/started/finished UTC timestamps, failure message or ready playback URL |
| `GET /media/{id}/...` | Ready HLS playlists and segments; supports range requests |
| `GET /healthz` | Process liveness |
| `GET /readyz` | DB ping, HTTP 503 if unavailable |
| `GET /` | Upload/status/player page |

Metadata never includes filesystem paths or raw child-process diagnostics. Errors
have `{"error":{"code":"...","message":"..."}}`. Upload limit is **100 MiB for
the entire request**, including multipart framing, not just file bytes. Fields are
streamed with bounded memory; title is limited to 512 UTF-8 bytes. Duplicate or
unexpected fields and incomplete uploads are rejected. Server-generated UUID
paths prevent filename traversal. A successful request never runs FFmpeg.

## Configuration

| Environment | Default | Purpose |
| --- | --- | --- |
| `DATABASE_URL` | required; set by Compose | pgx PostgreSQL URL |
| `DATA_DIR` | `./data`; `/data` in Compose | Shared sources/output root |
| `HTTP_ADDR` | `:8080` | API listen address |
| `PORT` | `8080` | Compose host port |
| `PROBE_TIMEOUT` | `15s` | Per inspection deadline |
| `TRANSCODE_TIMEOUT` | `5m` | Deadline covering both renditions and publication |
| `POLL_INTERVAL` | `1s` | Context-aware wait when queue empty/DB unavailable |

Durations must be positive Go duration strings. HTTP header timeout is 5 seconds;
request read and response write timeouts are 2 minutes, idle timeout 60 seconds.
SIGTERM cancels active uploads/FFmpeg. Worker records failure using a fresh
five-second context if PostgreSQL is still available. Raw diagnostics (bounded to
16 KiB per invocation) are retained only in structured worker logs with video ID.

## Verification

```sh
go test -race ./...
go vet ./...
# The store test skips without TEST_DATABASE_URL; this executes it against Compose:
docker compose --profile test run --build --rm tests
./scripts/sample.sh
python3 scripts/verify.py
```

`go test` generates its own fixtures with lavfi. Media tests skip if FFmpeg is not
installed. They transcode audio and silent clips, check VOD artifacts, decode both
renditions, reject corrupt input, cancel real FFmpeg after a segment is created,
and assert temporary output cleanup. HTTP tests check accepted uploads, safe
filenames, insertion-failure cleanup, malformed uploads and a >100 MiB stream
without Content-Length. Worker tests verify shutdown can persist sanitized failure
with an independently live context. The PostgreSQL integration test uses an
isolated temporary schema and two simultaneous claims against one queued row;
exactly one must succeed. It does not mutate application rows.

`scripts/verify.py` uses actual HTTP uploads through Compose, waits for ready or
failed, fetches all playlist references and segments, inspects H.264/AAC and output
sizes, decodes both HLS qualities over HTTP, and checks corrupt and oversized input.
Browser playback remains a separate check: upload a sample, observe status updates,
click Play and the native player's play control, confirm frames and advancing time.

Executed on 2026-11-03 (macOS arm64 host, Docker/Colima Linux arm64):

| Check | Executed result |
| --- | --- |
| `go test -race ./...` | Passed; actual FFmpeg audio/silent, cancellation, upload and worker tests ran. Host store test skipped without URL. |
| `go vet ./...` | Passed |
| `docker compose up --build -d` | Passed; API and DB healthy, separate worker running |
| `docker compose --profile test run --build --rm tests` | Passed `TestConcurrentClaim` against real PostgreSQL, exactly one claimant |
| `python3 scripts/verify.py` | Passed audio/silent upload → ready → 360p/720p HTTP fetch, codec/dimension inspection and full decode; corrupt → failed; oversized → 413 |
| Asynchronous response | Audio upload 46 ms, silent upload 35 ms; worker processing approximately 3.3 s / 2.7 s |
| Visual browser playback | **Not executed**: no browser automation provider was available and native Safari access timed out. HTTP decoding passed; no visual/audible playback claim. |

The initial concurrent image build exposed a shared-tag export collision; separate
API/worker image tags fixed it, and the actual Compose build/start then passed.

## Interrupted-job recovery and limitations

No automatic retries or distributed leases. SIGKILL, host failure or a DB outage
during shutdown can leave a row `processing`. **Stop all workers before manual
requeue**, including workers started outside Compose:

```sh
docker compose stop worker
docker compose run --rm --no-deps worker worker -requeue UUID
docker compose up -d worker
```

Only `processing` rows can be requeued. The command removes that ID's stale output
and attempt directories before requeueing. A crash during recovery is recoverable
by rerunning it with workers still stopped. Ordinary processing failures become
`failed` and remove the attempt directory. Failed jobs are not retried; upload again.

Filesystem and PostgreSQL cannot commit atomically. A process crash after source
rename but before INSERT can orphan a source; an uncertain INSERT commit can also
leave a row with a missing source after cleanup. A crash after output rename but
before the ready UPDATE leaves inaccessible orphan output and a processing row.
If publication succeeds but the DB update fails, the worker logs an explicit
inconsistency with `published=true`. Inspect logs and use stopped-worker requeue.
There is no reconciliation daemon. Data is local, without backups or disk quotas.

This is **only a local development demo**, without authentication or hostile-media
isolation. Anyone with API access can upload, list and play all videos. Network
protocols for inspection/transcoding are restricted to file/pipe, but FFmpeg still
parses untrusted files. No public deployment, rate limiting, automatic retention,
worker leases, retry policy or general video-format support is included.

Optional future work (not implemented): job leases with bounded retries, orphan
reconciliation, retention/quotas, vendored hls.js, and stronger media sandboxing.
