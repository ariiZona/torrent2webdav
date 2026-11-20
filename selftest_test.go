package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestSelfTestChecksIPAndPort(t *testing.T) {
	ipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "203.0.113.9\n")
	}))
	defer ipSrv.Close()

	queried := make(chan url.Values, 1)
	portSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queried <- r.URL.Query()
		_, _ = io.WriteString(w, `{"status":"OPEN","ms":3}`)
	}))
	defer portSrv.Close()

	restore := swapSelfTestURLs(ipSrv.URL, portSrv.URL)
	defer restore()

	st := startSelfTest(context.Background(), 42069)
	got := waitSelfTest(t, st)
	if got.PublicIP != "203.0.113.9" || got.Port != 42069 || got.PortStatus != "OPEN" || got.Error != "" {
		t.Fatalf("self-test = %+v", got)
	}
	select {
	case q := <-queried:
		if q.Get("ip") != "203.0.113.9" || q.Get("port") != "42069" {
			t.Errorf("checker query = %v", q)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("port checker was not called")
	}
}

func TestSelfTestReportsFailure(t *testing.T) {
	ipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer ipSrv.Close()

	restore := swapSelfTestURLs(ipSrv.URL, ipSrv.URL)
	defer restore()

	st := startSelfTest(context.Background(), 42069)
	got := waitSelfTest(t, st)
	if got.PortStatus != "unknown" || got.Error == "" {
		t.Fatalf("self-test = %+v", got)
	}
	if got.Checked == "" {
		t.Error("failure not timestamped")
	}
}

func swapSelfTestURLs(ip, port string) func() {
	oldIP, oldPort := publicIPURL, portCheckURL
	publicIPURL, portCheckURL = ip, port
	return func() { publicIPURL, portCheckURL = oldIP, oldPort }
}

func waitSelfTest(t *testing.T, st *selfTest) statusIP {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := st.report(); got.Checked != "" {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatal("self-test did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
