package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Public services used by the startup self-test. Variables so a test can point
// them at a local server.
var (
	publicIPURL  = "https://api.ipify.org"
	portCheckURL = "https://api.networktools.dev/v1/port-test"
)

// selfTest finds our public IP and asks an external checker whether the
// BitTorrent port is reachable. It runs once at startup, in the background, so
// a slow or unavailable service never delays serving.
type selfTest struct {
	mu         sync.Mutex
	ip         string
	port       int
	portStatus string
	err        string
	checked    time.Time
}

func startSelfTest(ctx context.Context, port int) *selfTest {
	st := &selfTest{port: port, portStatus: "checking"}
	go st.run(ctx)
	return st
}

func (st *selfTest) run(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	ip, err := fetchPublicIP(ctx)
	if err != nil {
		st.finish("", "unknown", err)
		log.Printf("self-test: public IP check failed: %v", err)
		return
	}
	status, err := checkReachable(ctx, ip, st.port)
	if err != nil {
		st.finish(ip, "unknown", err)
		log.Printf("self-test: public IP %s; torrent port %d/tcp check failed: %v", ip, st.port, err)
		return
	}
	st.finish(ip, status, nil)
	if status == "OPEN" {
		log.Printf("self-test: public IP %s; torrent port %d/tcp is reachable", ip, st.port)
	} else {
		log.Printf("self-test: public IP %s; torrent port %d/tcp is %s (forward the port or use network_mode: host)", ip, st.port, status)
	}
}

func (st *selfTest) finish(ip, status string, err error) {
	st.mu.Lock()
	st.ip, st.portStatus, st.checked = ip, status, time.Now()
	st.err = ""
	if err != nil {
		st.err = err.Error()
	}
	st.mu.Unlock()
}

func (st *selfTest) report() statusIP {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := statusIP{PublicIP: st.ip, Port: st.port, PortStatus: st.portStatus, Error: st.err}
	if !st.checked.IsZero() {
		out.Checked = st.checked.UTC().Format(time.RFC3339)
	}
	return out
}

func fetchPublicIP(ctx context.Context) (string, error) {
	body, err := fetchURL(ctx, publicIPURL, 128)
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("unexpected response %q", ip)
	}
	return ip, nil
}

// checkReachable asks the external port checker for the status of one TCP port.
// UDP cannot be checked this way.
func checkReachable(ctx context.Context, ip string, port int) (string, error) {
	q := url.Values{"ip": {ip}, "port": {fmt.Sprint(port)}}
	body, err := fetchURL(ctx, portCheckURL+"?"+q.Encode(), 1024)
	if err != nil {
		return "", err
	}
	var res struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if status := strings.ToUpper(strings.TrimSpace(res.Status)); status != "" {
		return status, nil
	}
	if res.Error != "" {
		return "", fmt.Errorf("checker: %s", res.Error)
	}
	return "", fmt.Errorf("checker returned no status")
}

func fetchURL(ctx context.Context, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, limit))
}
