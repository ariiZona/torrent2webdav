# torrent2webdav

Serves the files of one torrent as a read-only WebDAV share. A file is only
downloaded when a client reads it: reads go through a torrent `ReadSeeker`, so
range requests (what rclone and media players use) seek into the swarm instead
of fetching from the start.

The whole thing is a single Go binary built on two libraries:

- [`anacrolix/torrent`](https://github.com/anacrolix/torrent) — pure Go BitTorrent client with per-file readers and readahead.
- [`x/net/webdav`](https://pkg.go.dev/golang.org/x/net/webdav) — WebDAV handler.

No database, no download queue, no API server.

## Run

```sh
mkdir -p torrents cache
cp .env.example .env   # optional: choose the BitTorrent port
cp docker-compose.example.yaml docker-compose.yml
# edit docker-compose.yml and set TORRENT_FILE
docker compose pull
docker compose up -d
```

The example Compose file pulls `ghcr.io/ariizona/torrent2webdav:latest`, built by the
GitHub Actions workflow. If that package is private, run `docker login ghcr.io`
first, or uncomment `build: .` for a local build.

or without Docker:

```sh
go run . -torrent /path/to/file.torrent -cache-dir ./cache
```

WebDAV is served on `127.0.0.1:8080` (localhost only) by default; see
[LAN and TLS](#lan-and-tls) to expose it on other interfaces.

Put the `.torrent` in `./torrents/` and set `TORRENT_FILE` in your copied
`docker-compose.yml` (or run Compose with `-f docker-compose.example.yaml`).

## Status page

`http://DOCKER_HOST:8080/_status` shows the torrent name and infohash, progress,
download/upload speed, global download/upload and WebDAV-sent totals (kept
across restarts), swarm and peer counts, the files open for streaming with how
much of each is cached, and
all files with cached data (25 per page, sortable, with last-opened time and
bytes uploaded to peers), the connected peers and the configured trackers. It
refreshes once a second from `/_status.json`. The paths deliberately start with
`_` so a torrent file named `status` still resolves through WebDAV.

At startup the app also logs a self-test:

```
self-test: public IP 203.0.113.9; torrent port 42069/tcp is reachable
```

It asks `api.ipify.org` for the public IP and `api.networktools.dev` whether the
BitTorrent port accepts TCP connections from the internet (one check per start;
the free tier allows 50/day). UDP and the WebDAV port are not tested. The check
runs in the background, so a slow or unavailable service never delays serving:
the log says `unknown` and the app continues. A `TIMEOUT` or `CLOSED` result
means the port is not forwarded or the host is behind CGNAT; downloads then use
fewer peers.

## Configure

Flags and environment variables are equivalent.

| Flag | Env | Default | Meaning |
|------|-----|---------|---------|
| `-torrent` | `TORRENT_FILE` | *(required)* | Path to the `.torrent` file |
| `-webdav-addr` | `WEBDAV_ADDR` | `127.0.0.1:8080` | WebDAV listen address |
| `-torrent-port` | `TORRENT_PORT` | `42069` | BitTorrent TCP + UDP listen port |
| `-cache-dir` | `CACHE_DIR` | `./cache` | Parent directory for the app's private cache |
| `-cache-max-size` | `CACHE_MAX_SIZE` | `0` | Clear the cache above this size while idle (`50G`, `1.5TB`, bytes; `0` = no limit) |
| `-cache-ttl` | `CACHE_TTL` | `0` | Remove files not read for this long (`30m`, `24h`, `168h`; `0` = never) |
| `-upload-rate` | `UPLOAD_RATE` | `0` | Max upload speed to peers (`1M`, `500K`; bytes/s; `0` = unlimited) |
| `-full-cache-after` | `FULL_CACHE_AFTER` | `250M` | After this many bytes of a file have been read through WebDAV, download the rest of it in the background, continuing after the client disconnects (`250M`, `1G`; `0` = never) |
| `-user` | `WEBDAV_USER` | *(empty)* | Enable HTTP basic auth with this user |
| `-pass` | `WEBDAV_PASS` | *(empty)* | Password for basic auth |

With Compose, set `TORRENT_PORT` in `.env` (copy `.env.example`): both the
published TCP/UDP ports and the app's listen port follow it. Under
`network_mode: host`, only the listen port matters. To change the WebDAV port,
change `WEBDAV_ADDR` and the container port mapping together; inside a container
it must stay `:PORT` or `0.0.0.0:PORT` (not `127.0.0.1`), or the published port
cannot reach it. `WEBDAV_USER` and `WEBDAV_PASS` must be set together (or both
left empty): a lone username or password stops the app at startup instead of
serving an unprotected share.

## Mount on the client (rclone)

```sh
rclone config create t2w webdav url http://DOCKER_HOST:8080 vendor other
rclone mount t2w: /mnt/torrent \
  --vfs-cache-mode full \
  --vfs-cache-max-size 50G \
  --dir-cache-time 1h
```

`--vfs-cache-mode full` is what gives you local caching, background prefetch,
and seeking without re-downloading.

### Auth

The status page uses the same credentials. With `WEBDAV_USER` / `WEBDAV_PASS`
set, add the credentials:

```sh
rclone config create t2w webdav url https://DOCKER_HOST/ vendor other \
  user YOUR_USER pass $(rclone obscure 'YOUR_PASS')
```

Basic auth sends the password in cleartext, so use it over TLS only.

### LAN and TLS

The Compose file publishes WebDAV on `127.0.0.1:8080`, so only the Docker host
can reach it by default. To mount it from another machine:

- **Reverse proxy with TLS (recommended).** Keep the localhost binding and put
  Caddy, nginx or Traefik in front with a certificate; point rclone at the
  proxy's `https://` URL. Enable basic auth as well if the proxy does not
  authenticate for you.
- **Trusted LAN without TLS.** Change the mapping to `"8080:8080/tcp"` (or bind
  a specific host IP) and enable basic auth. The password crosses the network in
  the clear, so only do this on a network you trust.

Running without Docker, the binary listens on `127.0.0.1:8080` (localhost
only) by default. To expose it on the LAN, pass
`-webdav-addr 0.0.0.0:8080` or set `WEBDAV_ADDR=0.0.0.0:8080` (`:8080` is
equivalent), and enable basic auth or put a TLS proxy in front.

## What the share looks like

The torrent's contents are exposed at the share root — no extra
torrent-name folder:

```
/
├── movie.mkv
└── Subs/
    └── movie.srt
```

Single-file torrents appear as one file at `/`. Directories and file sizes come
from the torrent metadata, so listing (PROPFIND) never touches the swarm. Writes
(`PUT`, `MKCOL`, `DELETE`, ...) are refused: the share is read-only.

File mtimes come from the torrent's creation date (or the epoch if absent) and
never change, so clients don't re-download on every container restart.

## Cache limits

`CACHE_MAX_SIZE` and `CACHE_TTL` are enforced about once a minute, and only
while the share is idle: no file open, and no read in the last minute. The
share stays available and the next read downloads from the swarm again.

- Both limits remove whole files, least-recently-read first.
- `CACHE_MAX_SIZE` removes files until the cached total fits.
- `CACHE_TTL` removes files whose last read is older than that.
- Read times are kept in memory; data cached by an earlier process counts as
  old as the persisted last-read time.
- Eviction rebuilds the torrent session. A cached piece that spans a removed
  file is fetched again on the next read.
- The cache can grow past `CACHE_MAX_SIZE` during a long stream; eviction waits
  until you stop. Set the limit at least as large as the biggest file you plan
  to watch, or leave it unlimited and rely on rclone's cache.
- `CACHE_TTL` also counts idle time while the container is stopped. The last-use
  timestamp lives outside the torrent payload so torrent filenames can't collide with it.
- **Upgrading an existing cache:** the app starts a new managed cache beneath
  `CACHE_DIR`. Legacy files and unrelated siblings are left in place. After checking
  the new mount, remove old downloads manually if you need the space.
- Size is the bytes downloaded for the current torrent. If you reuse one cache
  directory across different torrents, old torrents' data still occupies disk
  until the TTL clears the directory; a dedicated cache dir per torrent is
  simplest.
- **Run at most one instance per `CACHE_DIR`.** Two processes or containers
  pointed at the same directory (a shared bind mount or host path) both adopt it
  and write the same payload, and either instance's TTL/eviction can delete data
  the other is still writing, losing it on restart. Reusing the directory across
  restarts is fine; sharing it between running instances is not.
- rclone's `--vfs-cache-max-size` is a separate cache on the client. Size both.

Example: keep at most 50 GiB and drop it after a day without playback.

```yaml
environment:
  CACHE_MAX_SIZE: 50G
  CACHE_TTL: 24h
```

## Big torrents

Memory scales with **piece count**, not torrent size. Measured with a 3 TB
synthetic torrent (Go 1.27, anacrolix/torrent v1.63), live heap after adding it:

| Shape | Pieces | Torrent state |
|---|---|---|
| 3 TB, 16 MiB pieces | 196k | ~19 MB |
| 3 TB, 8 MiB pieces | 393k | ~37 MB |
| 3 TB, 8 MiB pieces, 40k files | 393k | ~78 MB |
| 3 TB, 1 MiB pieces | 3.1M | ~300 MB |

Allow roughly another 100–200 MB for the runtime, peer buffers and
downloaded-but-unverified data while streaming. A 3 TB release with 8–16 MiB
pieces therefore runs fine in 512 MB; one authored with 1 MiB pieces wants
about 1 GB. The startup log prints the actual piece count and piece length.

The bigger practical limit is disk: a 3 TB torrent does not fit in the server
cache. Size `CACHE_MAX_SIZE` to the disk you can spare and expect re-downloads
after an eviction; the working set normally lives in rclone's own cache.

## Performance notes

- Piece reads are prioritized from the current read position with 32 MiB of
  readahead, which suits rclone's sequential access.
- When the HTTP client disconnects, the pending torrent read is cancelled, so
  abandoned range requests don't keep pulling data.
- With `FULL_CACHE_AFTER` set (default 250M), once that many bytes of a file
  have been read, the rest of the file is downloaded in the background at normal
  priority, even if playback stops: three minutes into a 60 GB file queues all
  60 GB. Set `0` to disable, and size `CACHE_MAX_SIZE` (or a dedicated cache
  dir) for the largest file you watch. Reads and readahead keep a higher
  priority, so background filling doesn't compete with what the player seeks to.
- Inbound peers matter most. On Linux use `network_mode: host`, or forward the
  `TORRENT_PORT` TCP *and* UDP ports on your router. Without inbound
  connectivity you still download, just from fewer peers.
- DHT, PEX and trackers are enabled. While the share is idle it refreshes its
  DHT entry every 15 minutes and accepts inbound peers, so the next read starts
  with a warm peer pool instead of bootstrapping from scratch.
- The server cache (`/cache`) grows with everything that has been read, up to
  the torrent size. It is kept across restarts (piece completion is persisted).
  Bound it with `CACHE_MAX_SIZE` / `CACHE_TTL` (see above) or delete the
  directory to reclaim the space.

## Limits

- One torrent per container. To switch, change `TORRENT_FILE` and restart.
  Each torrent has its own payload directory, keyed by its metadata hash.
- No magnet links or multiple torrents — both are small additions around
  `Client.AddTorrent` if you need them.
- Auth is off unless both `WEBDAV_USER` and `WEBDAV_PASS` are set. Don't
  expose the port beyond localhost without auth, and use TLS when you do.
- Reads block until the swarm delivers the requested pieces; there is no
  timeout on the WebDAV side by design.
- Seeding is enabled: the container uploads cached pieces to peers, including
  while nothing is being read. It only downloads pieces requested through
  WebDAV (plus the `FULL_CACHE_AFTER` background fill), so it never fetches the
  whole torrent just to seed it. Cap the upload bandwidth with `UPLOAD_RATE` if
  needed.
- Files without an extension can still make PROPFIND read 512 bytes for MIME
  sniffing (and thus wait for the swarm). Everything with an extension lists
  instantly.

## Test

```sh
go test ./...
```

The image build also runs the tests. A local seeder supplies real pieces so
the streaming test can check a cold range read, a full stream, and a nested
directory without depending on a public swarm.
