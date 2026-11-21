package main

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/net/webdav"
)

const testSmallText = "hello from the swarm\n"

// End-to-end: one local client seeds a real torrent, the WebDAV share streams it
// from that peer, including a ranged read into the middle of the file.
func TestShareStreamsFromSwarm(t *testing.T) {
	mi, _, dataRoot, content := makeTorrent(t)

	dl := newTestClient(t, t.TempDir(), false)
	dlt, err := dl.AddTorrent(mi)
	must(t, err)
	<-dlt.GotInfo()

	seeder := newTestClient(t, dataRoot, true)
	st, err := seeder.AddTorrent(mi)
	must(t, err)
	<-st.GotInfo()
	must(t, st.VerifyDataContext(t.Context()))

	srv := httptest.NewServer(methodWrapper(&webdav.Handler{
		Prefix:     "/",
		FileSystem: newShare(dlt, mi.CreationDate),
		LockSystem: webdav.NewMemLS(),
	}))
	defer srv.Close()
	client := &http.Client{Timeout: 60 * time.Second}

	// Listing happens before any peer is connected: it must not read file data.
	for dir, want := range map[string]string{
		srv.URL + "/":     "big.t2wsmoke",
		srv.URL + "/sub/": "small.txt",
	} {
		if body := propfind(t, client, dir); !bytes.Contains(body, []byte(want)) {
			t.Errorf("PROPFIND %s missing %q", dir, want)
		}
	}

	// The cache is still cold here. A ranged read must fetch the requested
	// bytes without starting from the beginning of the file.
	if got := dlt.BytesCompleted(); got != 0 {
		t.Fatalf("%d bytes cached before any read", got)
	}
	const rangeStart, rangeEnd = 1048576, 1048599
	streamed := make(chan []byte, 1)
	go func() {
		b, err := doGet(client, srv.URL+"/big.t2wsmoke", fmt.Sprintf("bytes=%d-%d", rangeStart, rangeEnd))
		if err != nil {
			t.Error(err)
		}
		streamed <- b
	}()
	waitForPeer(t, dlt, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: seeder.LocalPort()})
	want := content[rangeStart : rangeEnd+1]
	if got := <-streamed; !bytes.Equal(got, want) {
		t.Fatalf("cold ranged read differs: got %d bytes, want %d", len(got), len(want))
	}
	if got := get(t, client, srv.URL+"/big.t2wsmoke", ""); !bytes.Equal(got, content) {
		t.Fatalf("full read differs (%d bytes)", len(got))
	}
	if got := get(t, client, srv.URL+"/sub/small.txt", ""); string(got) != testSmallText {
		t.Fatalf("small.txt = %q", got)
	}

	req, err := http.NewRequest("PUT", srv.URL+"/new.txt", strings.NewReader("x"))
	must(t, err)
	res, err := client.Do(req)
	must(t, err)
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT status = %d, want %d", res.StatusCode, http.StatusMethodNotAllowed)
	}
	if got := res.Header.Get("Allow"); got != allowHeader {
		t.Errorf("PUT Allow = %q, want %q", got, allowHeader)
	}
}

// Over-limit cache is wiped while idle, and the rebuilt session keeps serving.
func TestCacheEvictionAndRestart(t *testing.T) {
	mi, torrentPath, dataRoot, content := makeTorrent(t)
	s := newStream(torrentPath, t.TempDir(), 0, 1<<20, 0, 0, 0) // 1 MiB cap, 3 MiB file
	t.Cleanup(s.Close)

	seeder := newTestClient(t, dataRoot, true)
	st, err := seeder.AddTorrent(mi)
	must(t, err)
	<-st.GotInfo()
	must(t, st.VerifyDataContext(t.Context()))

	srv := httptest.NewServer(&webdav.Handler{
		Prefix:     "/",
		FileSystem: s,
		LockSystem: webdav.NewMemLS(),
	})
	defer srv.Close()
	client := &http.Client{Timeout: 60 * time.Second}

	getFile := func() []byte {
		out := make(chan []byte, 1)
		go func() {
			b, err := doGet(client, srv.URL+"/big.t2wsmoke", "")
			if err != nil {
				t.Error(err)
			}
			out <- b
		}()
		waitForPeer(t, s.t, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: seeder.LocalPort()})
		return <-out
	}
	if got := getFile(); !bytes.Equal(got, content) {
		t.Fatalf("full read differs (%d bytes)", len(got))
	}
	before, err := s.Stat(t.Context(), "")
	must(t, err)
	if want := time.Unix(mi.CreationDate, 0); !before.ModTime().Equal(want) {
		t.Fatalf("modtime before eviction = %v, want %v", before.ModTime(), want)
	}

	waitIdle(t, s)
	s.mu.Lock()
	s.lastUsed = time.Now().Add(-2 * evictIdle)
	cached := s.t.BytesCompleted()
	s.mu.Unlock()
	if cached <= s.maxBytes {
		t.Fatalf("only %d bytes cached, limit is %d", cached, s.maxBytes)
	}
	s.evictIfNeeded()

	if got := s.t.BytesCompleted(); got > s.maxBytes {
		t.Fatalf("cache not cleared: %d bytes", got)
	}
	if got := getFile(); !bytes.Equal(got, content) {
		t.Fatalf("read after eviction differs (%d bytes)", len(got))
	}
	after, err := s.Stat(t.Context(), "")
	must(t, err)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("modtime changed across eviction: %v -> %v", before.ModTime(), after.ModTime())
	}
}

func TestCacheIdleExpired(t *testing.T) {
	dir := t.TempDir()
	if cacheIdleExpired(dir, time.Hour) {
		t.Fatal("empty cache treated as expired")
	}
	must(t, os.WriteFile(filepath.Join(dir, "data"), nil, 0o644))
	if cacheIdleExpired(dir, time.Hour) {
		t.Fatal("cache without a marker treated as expired")
	}
	marker := filepath.Join(dir, cacheMarker)
	must(t, writeCacheMarker(marker, time.Now()))
	if cacheIdleExpired(dir, time.Hour) {
		t.Fatal("fresh cache treated as expired")
	}
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(marker, old, old)
	if !cacheIdleExpired(dir, time.Hour) {
		t.Fatal("old cache not treated as expired")
	}
	// Only a regular file is a valid record.
	must(t, os.Remove(marker))
	must(t, os.Mkdir(marker, 0o755))
	if cacheIdleExpired(dir, time.Hour) {
		t.Fatal("directory marker treated as expired")
	}
}

// A restart must continue the idle timer from the persisted marker.
func TestCacheTTLAcrossRestart(t *testing.T) {
	_, torrentPath, _, _ := makeTorrent(t)
	root := t.TempDir()
	managed, err := prepareManagedCache(root)
	must(t, err)
	payloadFile := filepath.Join(managed, payloadDirName, "cached")
	must(t, os.WriteFile(payloadFile, []byte("data"), 0o644))

	const ttl = time.Hour
	const idle = 45 * time.Minute
	must(t, writeCacheMarker(filepath.Join(managed, cacheMarker), time.Now().Add(-idle)))

	s := newStream(torrentPath, root, 0, 0, ttl, 0, 0)
	t.Cleanup(s.Close)

	if _, err := os.Stat(payloadFile); err != nil {
		t.Fatalf("payload cleared with TTL remaining: %v", err)
	}
	remaining := time.Until(s.lastUsed.Add(ttl))
	want := ttl - idle
	if remaining < want-2*time.Minute || remaining > want+2*time.Minute {
		t.Fatalf("remaining TTL = %v, want about %v", remaining, want)
	}
}

// A payload without a marker predates this version: never delete it on a guess.
func TestCacheTTLMissingMarkerKeepsPayload(t *testing.T) {
	_, torrentPath, _, _ := makeTorrent(t)
	root := t.TempDir()
	managed, err := prepareManagedCache(root)
	must(t, err)
	payloadFile := filepath.Join(managed, payloadDirName, "cached")
	must(t, os.WriteFile(payloadFile, []byte("data"), 0o644))

	s := newStream(torrentPath, root, 0, 0, time.Hour, 0, 0)
	t.Cleanup(s.Close)
	if _, err := os.Stat(payloadFile); err != nil {
		t.Fatalf("unmarked payload cleared: %v", err)
	}
}

// An old marker still clears the payload at startup.
func TestCacheTTLExpiredMarkerClearsPayload(t *testing.T) {
	_, torrentPath, _, _ := makeTorrent(t)
	root := t.TempDir()
	managed, err := prepareManagedCache(root)
	must(t, err)
	payloadFile := filepath.Join(managed, payloadDirName, "cached")
	must(t, os.WriteFile(payloadFile, []byte("data"), 0o644))
	must(t, writeCacheMarker(filepath.Join(managed, cacheMarker), time.Now().Add(-2*time.Hour)))

	s := newStream(torrentPath, root, 0, 0, time.Hour, 0, 0)
	t.Cleanup(s.Close)
	if _, err := os.Stat(payloadFile); !os.IsNotExist(err) {
		t.Fatalf("expired payload survived: %v", err)
	}
}

// The timer must not restart the torrent client, or refresh the TTL, when the
// payload is already empty.
func TestCacheTTLSkipsEmptyPayload(t *testing.T) {
	_, torrentPath, _, _ := makeTorrent(t)
	s := newStream(torrentPath, t.TempDir(), 0, 0, time.Hour, 0, 0)
	t.Cleanup(s.Close)
	if got := s.t.BytesCompleted(); got != 0 {
		t.Fatalf("%d bytes cached before any read", got)
	}

	s.mu.Lock()
	s.lastUsed = time.Now().Add(-2 * time.Hour)
	oldUsed, before := s.lastUsed, s.t
	s.mu.Unlock()

	s.evictIfNeeded()

	s.mu.Lock()
	gotUsed := s.lastUsed
	s.mu.Unlock()
	if s.t != before {
		t.Fatal("empty session was rebuilt")
	}
	if !gotUsed.Equal(oldUsed) {
		t.Fatalf("TTL reset on empty payload: %v -> %v", oldUsed, gotUsed)
	}
}

func TestClientNameHasLibraryVersion(t *testing.T) {
	if name := clientName(); !strings.HasPrefix(name, "anacrolix/torrent v") {
		t.Fatalf("client name = %q, want library name and version", name)
	}
}

func TestReadThresholdStartsFullCache(t *testing.T) {
	_, torrentPath, _, _ := makeTorrent(t)
	s := newStream(torrentPath, t.TempDir(), 0, 0, 0, 0, 1<<20)
	t.Cleanup(s.Close)

	e := s.share.lookup("big.t2wsmoke")
	if e == nil {
		t.Fatal("file not found in share")
	}
	s.noteRead(e, 1<<20-1)
	if p := e.file.Priority(); p != torrent.PiecePriorityNone {
		t.Fatalf("priority = %v before threshold", p)
	}
	s.noteRead(e, 1)
	if p := e.file.Priority(); p != torrent.PiecePriorityNormal {
		t.Fatalf("priority = %v after threshold", p)
	}
}

func TestParseSize(t *testing.T) {
	ok := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"4096", 4096},
		{"1K", 1 << 10},
		{"1.5M", 1572864},
		{"50G", 50 << 30},
		{"2GiB", 2 << 30},
		{"1TB", 1 << 40},
		{"10 B", 10},
		{"0.5K", 512},
	}
	for _, tc := range ok {
		got, err := parseSize(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{
		"x", "-1G", "G", "1EB",
		"NaN", "Inf", "-Inf", "1e999",
		"0.5", "0.5B", "1e-400", "1e-400G",
		"9223372036854775808", "9000000000000000000G",
	} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) unexpectedly succeeded", bad)
		}
	}
}

func TestParseDuration(t *testing.T) {
	ok := []struct {
		in   string
		want time.Duration
	}{
		{"0", 0},
		{"0s", 0},
		{"30m", 30 * time.Minute},
		{"1.5h", 90 * time.Minute},
		{"24h", 24 * time.Hour},
	}
	for _, tc := range ok {
		got, err := parseDuration(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parseDuration(%q) = %s, %v; want %s", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{
		"x", "-1h", "0.5ns", "+0.4ns", "0.0000000001s",
		"NaN", "Inf", "9999999999999999999h",
	} {
		if _, err := parseDuration(bad); err == nil {
			t.Errorf("parseDuration(%q) unexpectedly succeeded", bad)
		}
	}
}

func TestCheckPort(t *testing.T) {
	for _, port := range []int{1, 80, 42069, 65535} {
		if err := checkPort(port); err != nil {
			t.Errorf("checkPort(%d) = %v; want nil", port, err)
		}
	}
	for _, port := range []int{-1, 0, 65536} {
		if err := checkPort(port); err == nil {
			t.Errorf("checkPort(%d) unexpectedly succeeded", port)
		}
	}
}

func TestValidateAuth(t *testing.T) {
	for _, tc := range []struct {
		user, pass string
		wantErr    bool
	}{
		{"", "", false},
		{"u", "p", false},
		{"u", "", true},
		{"", "p", true},
	} {
		if err := validateAuth(tc.user, tc.pass); (err != nil) != tc.wantErr {
			t.Errorf("validateAuth(%q, %q) = %v, wantErr %v", tc.user, tc.pass, err, tc.wantErr)
		}
	}
}

// With auth enabled, an anonymous request must be challenged, and the right
// credentials must still reach the handler.
func TestBasicAuthRejectsAnonymous(t *testing.T) {
	h := withBasicAuth("u", "p", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	res, err := http.Get(srv.URL)
	must(t, err)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", res.StatusCode)
	}
	if res.Header.Get("WWW-Authenticate") == "" {
		t.Error("missing WWW-Authenticate challenge")
	}

	req, err := http.NewRequest("GET", srv.URL, nil)
	must(t, err)
	req.SetBasicAuth("u", "p")
	res, err = http.DefaultClient.Do(req)
	must(t, err)
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("authenticated status = %d, want 204", res.StatusCode)
	}
}

func makeTorrent(t *testing.T) (mi *metainfo.MetaInfo, torrentPath, dataRoot string, content []byte) {
	t.Helper()
	dataRoot = t.TempDir()
	dataDir := filepath.Join(dataRoot, "smoke")
	must(t, os.MkdirAll(filepath.Join(dataDir, "sub"), 0o755))

	content = make([]byte, 3<<20)
	_, err := rand.Read(content)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(dataDir, "big.t2wsmoke"), content, 0o644))
	must(t, os.WriteFile(filepath.Join(dataDir, "sub", "small.txt"), []byte(testSmallText), 0o644))

	info := metainfo.Info{PieceLength: 256 << 10}
	must(t, info.BuildFromFilePath(dataDir))
	infoBytes, err := bencode.Marshal(info)
	must(t, err)
	mi = &metainfo.MetaInfo{InfoBytes: infoBytes, CreationDate: 1700000000}

	torrentPath = filepath.Join(t.TempDir(), "smoke.torrent")
	f, err := os.Create(torrentPath)
	must(t, err)
	must(t, mi.Write(f))
	must(t, f.Close())
	return
}

func propfind(t *testing.T, c *http.Client, url string) []byte {
	t.Helper()
	req, err := http.NewRequest("PROPFIND", url, nil)
	must(t, err)
	req.Header.Set("Depth", "1")
	res, err := c.Do(req)
	must(t, err)
	defer res.Body.Close()
	if res.StatusCode != 207 {
		t.Fatalf("PROPFIND %s status = %d", url, res.StatusCode)
	}
	b, err := io.ReadAll(res.Body)
	must(t, err)
	return b
}

// Re-adds the peer until a connection exists: anacrolix only dials while the
// torrent actually needs data, which is the moment the reader appears.
func waitForPeer(t *testing.T, tor *torrent.Torrent, addr net.Addr) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for len(tor.PeerConns()) == 0 {
		tor.AddPeers([]torrent.PeerInfo{{Addr: addr}})
		if time.Now().After(deadline) {
			t.Fatal("no peer connection established")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func waitIdle(t *testing.T, s *stream) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		active := s.active
		s.mu.Unlock()
		if active == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("WebDAV file still open")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func doGet(c *http.Client, url, rng string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if rng != "" {
		if res.StatusCode != http.StatusPartialContent {
			return nil, fmt.Errorf("range status = %d", res.StatusCode)
		}
		// A partial response must start exactly at the requested offset, not at
		// the start of the file.
		rangeSpec := strings.TrimPrefix(rng, "bytes=")
		if cr := res.Header.Get("Content-Range"); !strings.HasPrefix(cr, "bytes "+rangeSpec+"/") {
			return nil, fmt.Errorf("Content-Range = %q for %q", cr, rng)
		}
	}
	return io.ReadAll(res.Body)
}

func get(t *testing.T, c *http.Client, url, rng string) []byte {
	t.Helper()
	b, err := doGet(c, url, rng)
	must(t, err)
	return b
}

func newTestClient(t *testing.T, dir string, seed bool) *torrent.Client {
	t.Helper()
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = dir
	cfg.DefaultStorage = storage.NewFileWithCompletion(dir, storage.NewMapPieceCompletion())
	cfg.ListenPort = 0
	cfg.Seed = seed
	cfg.AlwaysWantConns = true
	cfg.NoDHT = true
	cfg.DisablePEX = true
	cfg.DisableTrackers = true
	cfg.NoDefaultPortForwarding = true
	cfg.Slogger = slog.New(slog.DiscardHandler)
	cl, err := torrent.NewClient(cfg)
	must(t, err)
	t.Cleanup(func() { cl.Close() })
	return cl
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
