package main

import (
	"encoding/json"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
)

// statusHandler serves a read-only dashboard next to the WebDAV share. The page
// polls the JSON endpoint every second; the WebDAV tree is untouched.
type statusHandler struct {
	s  *stream
	st *selfTest

	mu      sync.Mutex
	at      time.Time
	read    int64
	written int64
}

type statusTorrent struct {
	Name        string  `json:"name"`
	InfoHash    string  `json:"infohash"`
	Size        int64   `json:"size"`
	Cached      int64   `json:"cached"`
	PieceLength int64   `json:"pieceLength"`
	Pieces      int     `json:"pieces"`
	Percent     float64 `json:"percent"`
}

type statusSwarm struct {
	Total     int `json:"total"`
	Connected int `json:"connected"`
	Seeders   int `json:"seeders"`
	Pending   int `json:"pending"`
	HalfOpen  int `json:"halfOpen"`
}

type statusTransfer struct {
	Down       float64 `json:"down"`
	Up         float64 `json:"up"`
	Downloaded int64   `json:"downloaded"`
	Uploaded   int64   `json:"uploaded"`
}

type statusPeer struct {
	Addr   string  `json:"addr"`
	Source string  `json:"source"`
	Down   float64 `json:"down"`
	Up     float64 `json:"up"`
	Pieces int     `json:"pieces"`
}

type statusFile struct {
	Name    string `json:"name"`
	Readers int    `json:"readers"`
}

type statusIP struct {
	PublicIP   string `json:"publicIp"`
	Port       int    `json:"port"`
	PortStatus string `json:"portStatus"`
	Checked    string `json:"checked,omitempty"`
	Error      string `json:"error,omitempty"`
}

type statusSnapshot struct {
	Torrent  statusTorrent  `json:"torrent"`
	Swarm    statusSwarm    `json:"swarm"`
	Transfer statusTransfer `json:"transfer"`
	Open     []statusFile   `json:"openFiles"`
	Peers    []statusPeer   `json:"peers"`
	Trackers []string       `json:"trackers"`
	Listen   []string       `json:"listeners"`
	Check    statusIP       `json:"selfTest"`
}

func newStatusHandler(s *stream, st *selfTest) http.Handler {
	return &statusHandler{s: s, st: st}
}

// isStatusPath claims only the dashboard paths, so torrent files named
// "status" still belong to the share.
func isStatusPath(path string) bool {
	return path == "/_status" || path == "/_status/" || path == "/_status.json"
}

func (h *statusHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if strings.HasSuffix(r.URL.Path, ".json") {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(h.snapshot())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(statusPage))
}

func (h *statusHandler) snapshot() statusSnapshot {
	cl, t := h.s.hold()
	defer h.s.release()

	stats := t.Stats()
	read, written := stats.BytesReadData.Int64(), stats.BytesWrittenData.Int64()
	down, up := h.rates(read, written)

	out := statusSnapshot{
		Torrent: statusTorrent{
			Name:        t.Name(),
			InfoHash:    t.InfoHash().HexString(),
			Size:        t.Length(),
			Cached:      t.BytesCompleted(),
			PieceLength: t.Info().PieceLength,
			Pieces:      int(t.NumPieces()),
		},
		Swarm: statusSwarm{
			Total:     stats.TotalPeers,
			Connected: len(t.PeerConns()),
			Seeders:   stats.ConnectedSeeders,
			Pending:   stats.PendingPeers,
			HalfOpen:  stats.HalfOpenPeers,
		},
		Transfer: statusTransfer{Down: down, Up: up, Downloaded: read, Uploaded: written},
		Open:     h.s.fileList(),
		Peers:    peerList(t),
		Listen:   listenAddrs(cl.ListenAddrs()),
		Check:    h.st.report(),
	}
	if out.Torrent.Size > 0 {
		out.Torrent.Percent = 100 * float64(out.Torrent.Cached) / float64(out.Torrent.Size)
	}
	mi := t.Metainfo()
	out.Trackers = mi.UpvertedAnnounceList().DistinctValues()
	sort.Strings(out.Trackers)
	return out
}

// rates turns the cumulative counters into bytes per second between polls.
func (h *statusHandler) rates(read, written int64) (down, up float64) {
	now := time.Now()
	h.mu.Lock()
	prevAt, prevRead, prevWritten := h.at, h.read, h.written
	h.at, h.read, h.written = now, read, written
	h.mu.Unlock()
	if prevAt.IsZero() {
		return 0, 0
	}
	secs := now.Sub(prevAt).Seconds()
	if secs <= 0 {
		return 0, 0
	}
	return float64(max(0, read-prevRead)) / secs, float64(max(0, written-prevWritten)) / secs
}

func peerList(t *torrent.Torrent) []statusPeer {
	conns := t.PeerConns()
	out := make([]statusPeer, 0, len(conns))
	for _, pc := range conns {
		st := pc.Stats()
		addr := ""
		if pc.RemoteAddr != nil {
			addr = pc.RemoteAddr.String()
		}
		out = append(out, statusPeer{
			Addr:   addr,
			Source: string(pc.Discovery),
			Down:   st.DownloadRate,
			Up:     st.LastWriteUploadRate,
			Pieces: st.RemotePieceCount,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}

func listenAddrs(addrs []net.Addr) []string {
	seen := make(map[string]struct{}, len(addrs))
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		addr := a.String()
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		out = append(out, addr)
	}
	sort.Strings(out)
	return out
}
