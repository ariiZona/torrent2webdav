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

	srv := httptest.NewServer(&webdav.Handler{
		Prefix:     "/",
		FileSystem: newShare(dlt),
		LockSystem: webdav.NewMemLS(),
	})
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

	// A reader is what creates download demand, so start streaming first and
	// only then connect the peer.
	streamed := make(chan []byte, 1)
	go func() {
		b, err := doGet(client, srv.URL+"/big.t2wsmoke", "")
		if err != nil {
			t.Error(err)
		}
		streamed <- b
	}()
	waitForPeer(t, dlt, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: seeder.LocalPort()})
	if got := <-streamed; !bytes.Equal(got, content) {
		t.Fatalf("full read differs (%d bytes)", len(got))
	}
	got := get(t, client, srv.URL+"/big.t2wsmoke", "bytes=1048576-1048599")
	if !bytes.Equal(got, content[1048576:1048600]) {
		t.Fatal("ranged read differs")
	}
	if got := get(t, client, srv.URL+"/sub/small.txt", ""); string(got) != testSmallText {
		t.Fatalf("small.txt = %q", got)
	}

	req, err := http.NewRequest("PUT", srv.URL+"/new.txt", strings.NewReader("x"))
	must(t, err)
	res, err := client.Do(req)
	must(t, err)
	res.Body.Close()
	if res.StatusCode < 400 {
		t.Errorf("PUT status = %d, want a 4xx", res.StatusCode)
	}
}

// Over-limit cache is wiped while idle, and the rebuilt session keeps serving.
func TestCacheEvictionAndRestart(t *testing.T) {
	mi, torrentPath, dataRoot, content := makeTorrent(t)
	s := newStream(torrentPath, t.TempDir(), 0, 1<<20, 0) // 1 MiB cap, 3 MiB file
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
}

func TestCacheIdleExpired(t *testing.T) {
	dir := t.TempDir()
	if cacheIdleExpired(dir, time.Hour) {
		t.Fatal("empty cache treated as expired")
	}
	must(t, os.WriteFile(filepath.Join(dir, "data"), nil, 0o644))
	if !cacheIdleExpired(dir, time.Hour) {
		t.Fatal("unmarked cache data not treated as expired")
	}
	marker := filepath.Join(dir, cacheMarker)
	must(t, os.WriteFile(marker, nil, 0o644))
	if cacheIdleExpired(dir, time.Hour) {
		t.Fatal("fresh cache treated as expired")
	}
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(marker, old, old)
	if !cacheIdleExpired(dir, time.Hour) {
		t.Fatal("old cache not treated as expired")
	}
}

func TestParseSize(t *testing.T) {
	ok := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"", 0},
		{"4096", 4096},
		{"1K", 1 << 10},
		{"1.5M", 1572864},
		{"50G", 50 << 30},
		{"2GiB", 2 << 30},
		{"1TB", 1 << 40},
		{"10 B", 10},
	}
	for _, tc := range ok {
		got, err := parseSize(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"x", "-1G", "G", "1EB"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) unexpectedly succeeded", bad)
		}
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
	mi = &metainfo.MetaInfo{InfoBytes: infoBytes}

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
	if rng != "" && res.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("range status = %d", res.StatusCode)
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
