// Command torrent2webdav serves the files of one torrent over read-only WebDAV,
// downloading pieces on demand from the swarm.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/net/webdav"
)

const (
	// How much data to prioritize ahead of the current read position. Big enough
	// for rclone's sequential reads, small enough not to fetch the whole torrent.
	readaheadBytes = 32 << 20
	// Cache eviction only runs after this much time without a read, so a paused
	// player or a gap between episodes doesn't throw the cache away.
	evictIdle = time.Minute
	// Records when cached data was last read, so CACHE_TTL also works across restarts.
	cacheMarker = ".last-used"
)

func main() {
	torrentFile := flag.String("torrent", env("TORRENT_FILE", ""), "path to the .torrent file (required)")
	webdavAddr := flag.String("webdav-addr", env("WEBDAV_ADDR", ":8080"), "WebDAV listen address")
	torrentPort := flag.Int("torrent-port", envInt("TORRENT_PORT", 42069), "BitTorrent TCP/UDP listen port")
	cacheDir := flag.String("cache-dir", env("CACHE_DIR", "./cache"), "directory for downloaded pieces")
	cacheMax := flag.String("cache-max-size", env("CACHE_MAX_SIZE", "0"), "clear the cache above this size while idle (e.g. 50G; 0 = no limit)")
	cacheTTL := flag.Duration("cache-ttl", envDuration("CACHE_TTL", 0), "clear the cache after this much idle time (e.g. 24h; 0 = never)")
	user := flag.String("user", env("WEBDAV_USER", ""), "optional WebDAV basic-auth username")
	pass := flag.String("pass", env("WEBDAV_PASS", ""), "optional WebDAV basic-auth password")
	flag.Parse()

	if *torrentFile == "" {
		log.Fatal("no torrent: set TORRENT_FILE or -torrent")
	}
	maxBytes, err := parseSize(*cacheMax)
	if err != nil {
		log.Fatalf("cache-max-size: %v", err)
	}

	s := newStream(*torrentFile, *cacheDir, *torrentPort, maxBytes, *cacheTTL)
	defer s.Close()

	h := http.Handler(&webdav.Handler{
		Prefix:     "/",
		FileSystem: s,
		LockSystem: webdav.NewMemLS(),
	})
	if *user != "" {
		h = withBasicAuth(*user, *pass, h)
	}

	srv := &http.Server{
		Addr:              *webdavAddr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: a read can legitimately block until the swarm delivers data.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("webdav on %s", *webdavAddr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Fatalf("%s: %v", key, err)
	}
	return d
}

// parseSize parses "50G", "1.5GB", "20GiB", "500M" or a plain byte count.
// Sizes are binary (G = GiB).
func parseSize(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	if t == "" || t == "0" {
		return 0, nil
	}
	mult := int64(1)
	for _, suffix := range []struct {
		text string
		mult int64
	}{
		{"TIB", 1 << 40}, {"TB", 1 << 40}, {"T", 1 << 40},
		{"GIB", 1 << 30}, {"GB", 1 << 30}, {"G", 1 << 30},
		{"MIB", 1 << 20}, {"MB", 1 << 20}, {"M", 1 << 20},
		{"KIB", 1 << 10}, {"KB", 1 << 10}, {"K", 1 << 10},
		{"B", 1},
	} {
		if strings.HasSuffix(t, suffix.text) {
			mult = suffix.mult
			t = strings.TrimSpace(strings.TrimSuffix(t, suffix.text))
			break
		}
	}
	n, err := strconv.ParseFloat(t, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(n * float64(mult)), nil
}

func withBasicAuth(user, pass string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="torrent2webdav"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// stream owns the torrent client, its cache, and the current WebDAV share, and
// can reset all of them to enforce cache limits.
type stream struct {
	torrentFile string
	cacheDir    string
	port        int
	maxBytes    int64
	ttl         time.Duration

	mu       sync.Mutex
	cond     *sync.Cond
	active   int // open WebDAV files; eviction waits for zero
	wiping   bool
	lastUsed time.Time

	cl    *torrent.Client
	st    storage.ClientImplCloser
	t     *torrent.Torrent
	share *share

	done      chan struct{}
	loopDone  chan struct{}
	closeOnce sync.Once
}

func newStream(torrentFile, cacheDir string, port int, maxBytes int64, ttl time.Duration) *stream {
	s := &stream{
		torrentFile: torrentFile,
		cacheDir:    cacheDir,
		port:        port,
		maxBytes:    maxBytes,
		ttl:         ttl,
		lastUsed:    time.Now(),
		done:        make(chan struct{}),
		loopDone:    make(chan struct{}),
	}
	s.cond = sync.NewCond(&s.mu)

	if ttl > 0 && cacheIdleExpired(cacheDir, ttl) {
		log.Printf("cache idle for over %s, clearing %s", ttl, cacheDir)
		if err := os.RemoveAll(cacheDir); err != nil {
			log.Printf("clear cache: %v", err)
		}
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		log.Fatal(err)
	}
	s.open()
	go s.cleanLoop()
	log.Printf("serving %q: %d file(s), %.1f GiB, %d pieces of %.1f MiB, torrent port %d",
		s.t.Name(), len(s.t.Files()), float64(s.t.Length())/(1<<30),
		s.t.NumPieces(), float64(s.t.Info().PieceLength)/(1<<20), port)
	return s
}

// open starts a fresh client/torrent/share against cacheDir. It is only called
// before serving or from cleanLoop while no files are open.
func (s *stream) open() {
	cfg := torrent.NewDefaultClientConfig()
	cfg.ListenPort = s.port
	cfg.Seed = false
	st := storage.NewFile(s.cacheDir)
	cfg.DefaultStorage = st // the client never closes storage it was given
	cfg.Slogger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	cl, err := torrent.NewClient(cfg)
	if err != nil {
		st.Close()
		log.Fatalf("torrent client: %v", err)
	}
	t, err := cl.AddTorrentFromFile(s.torrentFile)
	if err != nil {
		cl.Close()
		st.Close()
		log.Fatalf("add torrent: %v", err)
	}
	<-t.GotInfo()
	s.cl, s.st, s.t, s.share = cl, st, t, newShare(t)
}

func (s *stream) close() {
	if s.cl != nil {
		s.cl.Close() // flushes the piece-completion database
	}
	if s.st != nil {
		if err := s.st.Close(); err != nil {
			log.Printf("close storage: %v", err)
		}
	}
	s.cl, s.st, s.t, s.share = nil, nil, nil, nil
}

func (s *stream) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		<-s.loopDone
		s.persistLastUsed()
		s.close()
	})
}

// cleanLoop enforces the cache limits about once a minute, only while idle.
func (s *stream) cleanLoop() {
	defer close(s.loopDone)
	if s.maxBytes <= 0 && s.ttl <= 0 {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.persistLastUsed()
			s.evictIfNeeded()
		}
	}
}

func (s *stream) evictIfNeeded() {
	s.mu.Lock()
	if s.wiping || s.active != 0 || time.Since(s.lastUsed) < evictIdle {
		s.mu.Unlock()
		return
	}
	var reason string
	size := s.t.BytesCompleted()
	switch {
	case s.maxBytes > 0 && size > s.maxBytes:
		reason = fmt.Sprintf("%d bytes cached, limit %d", size, s.maxBytes)
	case s.ttl > 0 && time.Since(s.lastUsed) > s.ttl:
		reason = "idle for " + s.ttl.String()
	default:
		s.mu.Unlock()
		return
	}
	s.wiping = true
	s.mu.Unlock()

	log.Printf("clearing cache: %s", reason)
	s.close()
	if err := os.RemoveAll(s.cacheDir); err != nil {
		log.Printf("clear cache: %v", err)
	}
	if err := os.MkdirAll(s.cacheDir, 0o755); err != nil {
		log.Fatalf("cache dir: %v", err)
	}
	s.open()

	s.mu.Lock()
	s.lastUsed = time.Now()
	s.wiping = false
	s.cond.Broadcast()
	s.mu.Unlock()
}

// persistLastUsed records the last read time so CACHE_TTL survives restarts.
func (s *stream) persistLastUsed() {
	if s.ttl <= 0 {
		return
	}
	s.mu.Lock()
	used := s.lastUsed
	s.mu.Unlock()
	path := filepath.Join(s.cacheDir, cacheMarker)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		return
	}
	_ = os.Chtimes(path, used, used)
}

func cacheIdleExpired(cacheDir string, ttl time.Duration) bool {
	fi, err := os.Stat(filepath.Join(cacheDir, cacheMarker))
	if err != nil {
		// No record of use. Empty/no cache is fine; old data without a marker
		// is treated as expired.
		entries, _ := os.ReadDir(cacheDir)
		return len(entries) > 0
	}
	return time.Since(fi.ModTime()) > ttl
}

func (s *stream) touch() {
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

func (s *stream) current() *share {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.wiping {
		s.cond.Wait()
	}
	return s.share
}

func (s *stream) Mkdir(ctx context.Context, name string, perm fs.FileMode) error {
	return s.current().Mkdir(ctx, name, perm)
}

func (s *stream) RemoveAll(ctx context.Context, name string) error {
	return s.current().RemoveAll(ctx, name)
}

func (s *stream) Rename(ctx context.Context, oldName, newName string) error {
	return s.current().Rename(ctx, oldName, newName)
}

func (s *stream) Stat(ctx context.Context, name string) (fs.FileInfo, error) {
	return s.current().Stat(ctx, name)
}

func (s *stream) OpenFile(ctx context.Context, name string, flag int, perm fs.FileMode) (webdav.File, error) {
	s.mu.Lock()
	for s.wiping {
		s.cond.Wait()
	}
	sh := s.share
	s.active++
	s.mu.Unlock()

	f, err := sh.OpenFile(ctx, name, flag, perm)
	if err != nil {
		s.release()
		return nil, err
	}
	return &trackedFile{File: f, s: s}, nil
}

func (s *stream) release() {
	s.mu.Lock()
	s.active--
	s.cond.Broadcast()
	s.mu.Unlock()
}

type trackedFile struct {
	webdav.File
	s    *stream
	once sync.Once
}

func (f *trackedFile) Read(p []byte) (int, error) {
	f.s.touch()
	return f.File.Read(p)
}

func (f *trackedFile) Seek(offset int64, whence int) (int64, error) {
	f.s.touch()
	return f.File.Seek(offset, whence)
}

func (f *trackedFile) Close() error {
	err := f.File.Close()
	f.once.Do(f.s.release)
	return err
}

// share is a read-only in-memory directory tree backed by torrent file readers.
type share struct {
	root *entry
}

func newShare(t *torrent.Torrent) *share {
	// Stable modtime (torrent creation date, or the epoch) so clients don't
	// treat files as changed on every container restart.
	mod := time.Unix(t.Metainfo().CreationDate, 0)
	root := &entry{name: t.Name(), dir: true, modTime: mod, kids: map[string]*entry{}}
	for _, f := range t.Files() {
		// x/net/webdav sniffs unknown extensions for the getcontenttype property
		// by reading 512 bytes. Registering a type keeps PROPFIND local instead
		// of blocking on the swarm.
		// shortcut: files without an extension still get sniffed; rare in torrents.
		ext := filepath.Ext(f.DisplayPath())
		if ext != "" && mime.TypeByExtension(ext) == "" {
			mime.AddExtensionType(ext, "application/octet-stream")
		}
		parts := strings.Split(f.DisplayPath(), "/")
		cur := root
		for _, p := range parts[:len(parts)-1] {
			next := cur.kids[p]
			if next == nil {
				next = &entry{name: p, dir: true, modTime: mod, kids: map[string]*entry{}}
				cur.kids[p] = next
			}
			cur = next
		}
		last := parts[len(parts)-1]
		cur.kids[last] = &entry{name: last, size: f.Length(), modTime: mod, file: f}
	}
	return &share{root: root}
}

type entry struct {
	name    string
	size    int64
	modTime time.Time
	dir     bool
	file    *torrent.File
	kids    map[string]*entry
}

func (e *entry) Name() string       { return e.name }
func (e *entry) Size() int64        { return e.size }
func (e *entry) ModTime() time.Time { return e.modTime }
func (e *entry) IsDir() bool        { return e.dir }
func (e *entry) Sys() any           { return nil }
func (e *entry) Mode() fs.FileMode {
	if e.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}

func (s *share) lookup(name string) *entry {
	cur := s.root
	for _, p := range strings.Split(name, "/") {
		if p == "" {
			continue
		}
		cur = cur.kids[p]
		if cur == nil {
			return nil
		}
	}
	return cur
}

func (s *share) Mkdir(context.Context, string, fs.FileMode) error { return fs.ErrPermission }
func (s *share) RemoveAll(context.Context, string) error          { return fs.ErrPermission }
func (s *share) Rename(context.Context, string, string) error     { return fs.ErrPermission }

func (s *share) Stat(_ context.Context, name string) (fs.FileInfo, error) {
	e := s.lookup(name)
	if e == nil {
		return nil, fs.ErrNotExist
	}
	return e, nil
}

func (s *share) OpenFile(ctx context.Context, name string, flag int, _ fs.FileMode) (webdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) != 0 {
		return nil, fs.ErrPermission
	}
	e := s.lookup(name)
	if e == nil {
		return nil, fs.ErrNotExist
	}
	return &openEntry{e: e, ctx: ctx}, nil
}

// openEntry lazily creates its torrent reader on first read or seek, so PROPFIND
// and stat-only requests never touch the swarm.
type openEntry struct {
	e   *entry
	ctx context.Context
	r   torrent.Reader
	off int // Readdir position
}

func (o *openEntry) reader() torrent.Reader {
	if o.r == nil {
		o.r = o.e.file.NewReader()
		o.r.SetReadahead(readaheadBytes)
		o.r.SetContext(o.ctx) // abort blocked reads when the client disconnects
	}
	return o.r
}

func (o *openEntry) Read(p []byte) (int, error) {
	if o.e.dir {
		return 0, fs.ErrInvalid
	}
	return o.reader().Read(p)
}

func (o *openEntry) Seek(offset int64, whence int) (int64, error) {
	if o.e.dir {
		return 0, fs.ErrInvalid
	}
	return o.reader().Seek(offset, whence)
}

func (o *openEntry) Write([]byte) (int, error)  { return 0, fs.ErrPermission }
func (o *openEntry) Stat() (fs.FileInfo, error) { return o.e, nil }

func (o *openEntry) Close() error {
	if o.r != nil {
		return o.r.Close()
	}
	return nil
}

func (o *openEntry) Readdir(n int) ([]fs.FileInfo, error) {
	if !o.e.dir {
		return nil, fs.ErrInvalid
	}
	names := make([]string, 0, len(o.e.kids))
	for name := range o.e.kids {
		names = append(names, name)
	}
	sort.Strings(names)
	if n <= 0 {
		n = len(names)
	}
	out := make([]fs.FileInfo, 0, n)
	for o.off < len(names) && len(out) < n {
		out = append(out, o.e.kids[names[o.off]])
		o.off++
	}
	if len(out) == 0 {
		return nil, io.EOF
	}
	return out, nil
}
