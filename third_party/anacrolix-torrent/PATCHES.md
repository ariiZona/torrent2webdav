# Local patch

Fork of `github.com/anacrolix/torrent` v1.63.0, pointed at by the `replace`
directive in the root `go.mod`. Everything else is upstream source.

The only change adds an exact per-chunk peer-upload signal, because the public
API only exposes torrent-wide upload totals:

- `config.go`: `ClientConfig.PeerUploaded func(*Torrent, off, n int64)`.
- `peerconn.go`: call it from `(*PeerConn).sendChunk` with the torrent offset
  and length of each piece chunk queued for a peer.

To update, copy a newer upstream module over this directory, re-apply both
hunks, and run `go mod tidy` in the root module.
