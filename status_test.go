package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStatusPageAndJSON(t *testing.T) {
	_, torrentPath, _, _ := makeTorrent(t)
	s := newStream(torrentPath, t.TempDir(), 0, 0, 0, 0, 0)
	t.Cleanup(s.Close)

	st := &selfTest{ip: "203.0.113.9", port: 42069, portStatus: "OPEN"}
	srv := httptest.NewServer(newStatusHandler(s, st))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/_status.json")
	must(t, err)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("json status = %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("json Content-Type = %q", got)
	}
	var got statusSnapshot
	must(t, json.NewDecoder(res.Body).Decode(&got))

	if got.Torrent.Name != "smoke" {
		t.Errorf("name = %q, want smoke", got.Torrent.Name)
	}
	if got.Torrent.Size <= 0 || got.Torrent.Pieces <= 0 || got.Torrent.PieceLength <= 0 {
		t.Errorf("bad torrent shape: %+v", got.Torrent)
	}
	if got.Torrent.Cached != 0 || got.Torrent.Percent != 0 {
		t.Errorf("cold torrent shows %d cached / %.1f%%", got.Torrent.Cached, got.Torrent.Percent)
	}
	if len(got.Open) != 0 || len(got.CachedFiles) != 0 {
		t.Errorf("cold torrent files = %+v / %+v", got.Open, got.CachedFiles)
	}
	if len(got.Listen) == 0 {
		t.Error("no listening addresses reported")
	}
	if got.Check.PublicIP != "203.0.113.9" || got.Check.Port != 42069 || got.Check.PortStatus != "OPEN" {
		t.Errorf("self-test = %+v", got.Check)
	}

	res, err = http.Get(srv.URL + "/_status")
	must(t, err)
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	must(t, err)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("page status = %d", res.StatusCode)
	}
	if !strings.Contains(string(body), "/_status.json") {
		t.Error("page does not poll the JSON endpoint")
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/_status", nil)
	must(t, err)
	res, err = http.DefaultClient.Do(req)
	must(t, err)
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", res.StatusCode)
	}
}

// Open files report their own cached bytes, and closing the reader moves the
// file into the cached-files list.
func TestStatusReportsPerFileCache(t *testing.T) {
	mi, torrentPath, dataRoot, _ := makeTorrent(t)
	s := newStream(torrentPath, t.TempDir(), 0, 0, 0, 0, 0)
	t.Cleanup(s.Close)

	seeder := newTestClient(t, dataRoot, true)
	st, err := seeder.AddTorrent(mi)
	must(t, err)
	<-st.GotInfo()
	must(t, st.VerifyDataContext(t.Context()))

	srv := httptest.NewServer(newStatusHandler(s, &selfTest{}))
	defer srv.Close()

	f, err := s.OpenFile(t.Context(), "big.t2wsmoke", 0, 0)
	must(t, err)
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()

	read := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(f)
		read <- err
	}()
	waitForPeer(t, s.t, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: seeder.LocalPort()})
	must(t, <-read)

	got := fetchStatus(t, srv.URL)
	if len(got.Open) != 1 {
		t.Fatalf("open files = %+v", got.Open)
	}
	if f := got.Open[0]; f.Name != "big.t2wsmoke" || f.Size != 3<<20 || f.Cached == 0 {
		t.Fatalf("open file = %+v", f)
	}

	must(t, f.Close())
	closed = true
	waitIdle(t, s)

	got = fetchStatus(t, srv.URL)
	if len(got.Open) != 0 {
		t.Errorf("closed file still open: %+v", got.Open)
	}
	if len(got.CachedFiles) != 1 {
		t.Fatalf("cached files = %+v", got.CachedFiles)
	}
	if f := got.CachedFiles[0]; f.Name != "big.t2wsmoke" || f.Size != 3<<20 || f.Cached == 0 {
		t.Errorf("cached file = %+v", f)
	}
}

func fetchStatus(t *testing.T, baseURL string) statusSnapshot {
	t.Helper()
	res, err := http.Get(baseURL + "/_status.json")
	must(t, err)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	must(t, err)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", res.StatusCode, body)
	}
	var got statusSnapshot
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode status %q: %v", body, err)
	}
	return got
}

func TestStatusPathsDoNotClaimShareFiles(t *testing.T) {
	for path, want := range map[string]bool{
		"/_status":       true,
		"/_status/":      true,
		"/_status.json":  true,
		"/status":        false,
		"/_status/other": false,
		"/movie.mkv":     false,
	} {
		if got := isStatusPath(path); got != want {
			t.Errorf("isStatusPath(%q) = %v, want %v", path, got, want)
		}
	}
}
