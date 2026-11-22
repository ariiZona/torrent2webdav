package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

// shutdownGrace is how long running requests may take to finish on their own
// before blocked torrent reads are canceled.
const shutdownGrace = 5 * time.Second

// httpLifecycle bounds server shutdown: Shutdown stops accepting and drains,
// then the base context is canceled so reads blocked on the swarm return. The
// handler count makes it safe to close the stream's storage afterwards.
type httpLifecycle struct {
	srv   *http.Server
	grace time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	cond     *sync.Cond
	draining bool
	active   int
}

func newHTTPLifecycle(h http.Handler, grace time.Duration) *httpLifecycle {
	ctx, cancel := context.WithCancel(context.Background())
	l := &httpLifecycle{grace: grace, ctx: ctx, cancel: cancel}
	l.cond = sync.NewCond(&l.mu)
	l.srv = &http.Server{
		Handler:           l.track(h),
		ReadTimeout:       time.Minute, // Bounds the request only; WriteTimeout stays unset so responses can stream.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext: func(net.Listener) context.Context { return l.ctx },
	}
	return l
}

// track counts running handlers. A request that races with shutdown is refused
// instead of touching storage that is about to close.
func (l *httpLifecycle) track(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		if l.draining {
			l.mu.Unlock()
			http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
			return
		}
		l.active++
		l.mu.Unlock()
		defer func() {
			l.mu.Lock()
			l.active--
			l.cond.Broadcast()
			l.mu.Unlock()
		}()
		h.ServeHTTP(w, r)
	})
}

// serve accepts connections until ctx is canceled, then shuts down and waits
// for every handler before returning.
func (l *httpLifecycle) serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- l.srv.Serve(ln) }()

	select {
	case err := <-errCh:
		l.shutdown()
		return err
	case <-ctx.Done():
	}

	l.shutdown()
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// shutdown stops accepting, waits up to the grace period for handlers to
// finish, then cancels the request contexts of anything still blocked.
func (l *httpLifecycle) shutdown() {
	l.mu.Lock()
	l.draining = true
	l.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), l.grace)
	err := l.srv.Shutdown(ctx)
	cancel()

	// Shutdown has returned, so no handler can start now. Cancel the base
	// context to abort reads still blocked on the swarm.
	l.cancel()
	if err != nil {
		// Context cancellation alone does not unblock a socket read or write.
		_ = l.srv.Close()
	}

	l.mu.Lock()
	for l.active > 0 {
		l.cond.Wait()
	}
	l.mu.Unlock()
}
