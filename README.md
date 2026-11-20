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
docker compose up -d --build
```

or without Docker:

```sh
go run . -torrent /path/to/file.torrent -cache-dir ./cache
```

Put the `.torrent` in `./torrents/` and set `TORRENT_FILE` in
`docker-compose.yml`.

## Configure

Flags and environment variables are equivalent.

| Flag | Env | Default | Meaning |
|------|-----|---------|---------|
| `-torrent` | `TORRENT_FILE` | *(required)* | Path to the `.torrent` file |
| `-webdav-addr` | `WEBDAV_ADDR` | `:8080` | WebDAV listen address |
| `-torrent-port` | `TORRENT_PORT` | `42069` | BitTorrent TCP + UDP listen port |
| `-cache-dir` | `CACHE_DIR` | `./cache` | Parent directory for the app's private cache |
| `-cache-max-size` | `CACHE_MAX_SIZE` | `0` | Clear the cache above this size while idle (`50G`, `1.5TB`, bytes; `0` = no limit) |
| `-cache-ttl` | `CACHE_TTL` | `0` | Clear the cache after this much idle time (`30m`, `24h`, `168h`; `0` = never) |
| `-user` | `WEBDAV_USER` | *(empty)* | Enable HTTP basic auth with this user |
| `-pass` | `WEBDAV_PASS` | *(empty)* | Password for basic auth |

To change the WebDAV port, change `WEBDAV_ADDR` and the container port
mapping together. `WEBDAV_USER` and `WEBDAV_PASS` must be set together (or both
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

With `WEBDAV_USER` / `WEBDAV_PASS` set, add the credentials:

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

Running without Docker, the binary listens on `:8080` (all interfaces) by
default; pass `-webdav-addr 127.0.0.1:8080` to restrict it the same way.

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
while the share is idle: no file open, and no read in the last minute. Both
policies evict managed torrent data and restart the torrent session; the share
stays available and the next read downloads from the swarm again.

That coarse granularity is deliberate: torrent data is stored in whole files
with piece hashes spread across them, and ripping out individual pieces while a
reader may still touch them is not safe. So:

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
- Inbound peers matter most. On Linux use `network_mode: host`, or forward the
  `TORRENT_PORT` TCP *and* UDP ports on your router. Without inbound
  connectivity you still download, just from fewer peers.
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
- Seeding is disabled: the container uploads only incidentally while
  downloading.
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
