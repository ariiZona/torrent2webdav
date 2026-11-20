package main

import (
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

// A handler blocked on the swarm is released by canceling its request context
// at the grace deadline, and shutdown still waits for it before returning.
func TestShutdownCancelsBlockedHandler(t *testing.T) {
	entered := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	exited := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()

	l := newHTTPLifecycle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(canceled)
		<-release // hold the handler open to prove shutdown waits
		close(exited)
	}), 20*time.Millisecond)
	defer l.srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	go func() { _ = l.srv.Serve(ln) }()

	go func() {
		if res, err := http.Get("http://" + ln.Addr().String() + "/"); err == nil {
			res.Body.Close()
		}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never ran")
	}

	done := make(chan struct{})
	go func() {
		l.shutdown()
		close(done)
	}()

	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("request context not canceled at the shutdown deadline")
	}
	select {
	case <-done:
		t.Fatal("shutdown returned while a handler was still running")
	default:
	}

	unblock()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not exit")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not return after the last handler exited")
	}
}
