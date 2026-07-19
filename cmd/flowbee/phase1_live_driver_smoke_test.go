package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestPhase1ServeLiveDriverObservationSmoke is the smallest process-level Phase 1
// v2.4 activation proof. It is opt-in because it talks to the installed tmux-driver
// daemon, but everything Flowbee owns (database, configuration, keys, listeners,
// and alert receiver) is isolated under the test directory.
//
// The counting UDS proxy is deliberately transparent: Flowbee still executes its
// production DriverPort wire adapter against the real daemon. The proxy lets this
// test prove the empty-store posture as well as infer it: startup requires both
// the global metadata feature and exact authenticated control-origin capability,
// performs Driver readiness/observation reads, and makes zero route-grant or
// message mutations when there is no durable Flowbee action to execute.
//
//	FLOWBEE_DRIVER_LIVE_TEST=1 \
//	FLOWBEE_DRIVER_SOCKET=/path/to/api.sock \
//	FLOWBEE_DRIVER_TOKEN_FILE=/path/to/control.token \
//	go test ./cmd/flowbee -run TestPhase1ServeLiveDriverObservationSmoke -v
func TestPhase1ServeLiveDriverObservationSmoke(t *testing.T) {
	if os.Getenv("FLOWBEE_DRIVER_LIVE_TEST") != "1" {
		t.Skip("set FLOWBEE_DRIVER_LIVE_TEST=1 to run the Phase 1 serve smoke against an installed Driver daemon")
	}
	realSocket := strings.TrimSpace(os.Getenv("FLOWBEE_DRIVER_SOCKET"))
	tokenFile := strings.TrimSpace(os.Getenv("FLOWBEE_DRIVER_TOKEN_FILE"))
	if realSocket == "" || tokenFile == "" {
		t.Fatal("FLOWBEE_DRIVER_SOCKET and FLOWBEE_DRIVER_TOKEN_FILE are required")
	}
	if _, err := os.Stat(realSocket); err != nil {
		t.Fatalf("stat Driver socket: %v", err)
	}
	assertOwnerOnlyRegularFile(t, tokenFile)

	tmp := t.TempDir()
	// Darwin caps sockaddr_un paths at 104 bytes; testing.T.TempDir can exceed
	// that before the socket basename is appended. Keep only the proxy socket in
	// a short, private directory while all Flowbee-owned artifacts stay in TempDir.
	shortSocketDir, err := os.MkdirTemp("/private/tmp", "fb-p1-")
	if err != nil {
		t.Fatalf("create short Driver proxy directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortSocketDir) })
	proxySocket := filepath.Join(shortSocketDir, "driver.sock")
	proxy, calls := startCountingDriverProxy(t, proxySocket, realSocket)
	defer proxy.Close()

	var alertCalls atomic.Int64
	alertReceiver := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		alertCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})}
	alertListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen alert receiver: %v", err)
	}
	defer alertListener.Close()
	go func() { _ = alertReceiver.Serve(alertListener) }()
	defer alertReceiver.Close()

	privateAddr := reserveTCPAddr(t)
	healthAddr := reserveTCPAddr(t)
	dbPath := filepath.Join(tmp, "flowbee.db")
	configPath := filepath.Join(tmp, "flowbee.yaml")
	writeOwnerOnly(t, configPath, fmt.Sprintf(`database_url: %q
private_addr: %q
health_addr: %q
lease_ttl_s: 60
heartbeat_interval_s: 10
long_poll_wait_s: 1
backup_interval_s: -1
`, dbPath, privateAddr, healthAddr))
	alertKey := filepath.Join(tmp, "alert.key")
	humanKey := filepath.Join(tmp, "human.key")
	humanGrants := filepath.Join(tmp, "human.grants")
	writeOwnerOnly(t, alertKey, "phase1-live-smoke-alert-secret\n")
	writeOwnerOnly(t, humanKey, "phase1-live-smoke-human-session-key-with-32-bytes\n")
	writeOwnerOnly(t, humanGrants, "smoke-user@default=admin\n")

	logPath := filepath.Join(tmp, "serve.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("open child log: %v", err)
	}
	defer logFile.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestPhase1ServeLiveDriverProcess$", "-test.v")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Env = replaceProcessEnv(os.Environ(), map[string]string{
		"FLOWBEE_PHASE1_SERVE_TEST_PROCESS": "1",
		"FLOWBEE_CONFIG":                    configPath,
		"FLOWBEE_EPIC_REVIEW_HANDOFF_V2":    "1",
		"FLOWBEE_PHASE1_DASHBOARD":          "1",
		"FLOWBEE_CAPACITY_ROUTING_V2":       "",
		"FLOWBEE_CAPACITY_V2":               "",
		"FLOWBEE_ALERT_WEBHOOK_URL":         "http://" + alertListener.Addr().String() + "/alerts",
		"FLOWBEE_ALERT_WEBHOOK_SECRET_FILE": alertKey,
		"FLOWBEE_EXTERNAL_WATCHDOG_ID":      "phase1-live-smoke-watchdog",
		"FLOWBEE_DRIVER_SOCKET":             proxySocket,
		"FLOWBEE_DRIVER_TOKEN_FILE":         tokenFile,
		"FLOWBEE_DRIVER_INSTANCE_REF":       "phase1-live-smoke-driver",
		"FLOWBEE_HUMAN_SESSION_KEY_FILE":    humanKey,
		"FLOWBEE_HUMAN_GRANTS_FILE":         humanGrants,
		"FLOWBEE_HUMAN_LOOPBACK_DEV":        "",
		"FLOWBEE_WORKER_AUTH_SECRET":        "",
		"FLOWBEE_ENROLLED_IDENTITIES":       "",
		"FLOWBEE_INSECURE":                  "",
		"FLOWBEE_GITHUB_TOKEN":              "",
		"FLOWBEE_GITHUB_OWNER":              "",
		"FLOWBEE_GITHUB_REPO":               "",
		"FLOWBEE_MIRROR_PATH":               "",
		"FLOWBEE_WEBHOOK_SECRET":            "",
	})
	if err := cmd.Start(); err != nil {
		t.Fatalf("start Phase 1 serve subprocess: %v", err)
	}
	stopped := false
	defer func() {
		if !stopped && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()

	healthBody := waitHTTPStatus(t, cmd, "http://"+healthAddr+"/healthz", http.StatusOK, logPath)
	for _, want := range []string{`"status":"ok"`, `"driver_control":{"required":true,"available":true,"status":"ready"`} {
		if !strings.Contains(healthBody, want) {
			t.Fatalf("v2.4 ready health response missing %s: %s", want, healthBody)
		}
	}
	if strings.Contains(healthBody, `"gap":"GAP-FD-003"`) {
		t.Fatalf("authorized v2.4 control origin still reported GAP-FD-003: %s", healthBody)
	}
	waitHTTPStatus(t, cmd, "http://"+privateAddr+"/dashboard", http.StatusOK, logPath)
	waitHTTPStatus(t, cmd, "http://"+privateAddr+"/workspace?project=default", http.StatusOK, logPath)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal serve subprocess: %v", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err := <-wait:
		stopped = true
		if err != nil {
			t.Fatalf("serve did not stop cleanly: %v\n%s", err, readTestLog(logPath))
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("serve did not stop within graceful-shutdown budget\n%s", readTestLog(logPath))
	}

	readCounts, mutationCounts := calls.snapshot()
	if readCounts["GET /v2/meta"] == 0 || readCounts["GET /v2/instance"] == 0 {
		t.Fatalf("serve did not execute Driver readiness checks; calls=%v\n%s", readCounts, readTestLog(logPath))
	}
	if readCounts["GET /v2/control/capabilities"] == 0 {
		t.Fatalf("serve did not authenticate the exact Driver control-origin capability; calls=%v\n%s", readCounts, readTestLog(logPath))
	}
	if readCounts["GET /v2/sessions"]+readCounts["GET /v2/events"] == 0 {
		t.Fatalf("serve did not execute Driver observation reads; calls=%v\n%s", readCounts, readTestLog(logPath))
	}
	if len(mutationCounts) != 0 {
		t.Fatalf("empty-store startup made Driver route/message mutation calls: %v", mutationCounts)
	}
	if got := alertCalls.Load(); got != 0 {
		t.Fatalf("empty isolated store emitted %d unexpected alerts", got)
	}
	t.Logf("Phase 1 v2.4 serve smoke green: control-origin ready, Driver reads=%v, route/message mutations=0", readCounts)
}

// TestPhase1ServeLiveDriverProcess is the isolated child entrypoint used above.
// Keeping runServe in a subprocess gives the smoke the same signal/graceful-stop
// behavior as the shipped binary without sending a process-wide signal to `go test`.
func TestPhase1ServeLiveDriverProcess(t *testing.T) {
	if os.Getenv("FLOWBEE_PHASE1_SERVE_TEST_PROCESS") != "1" {
		t.Skip("Phase 1 serve smoke helper")
	}
	if err := runServe(nil); err != nil {
		t.Fatal(err)
	}
}

type countedDriverCalls struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *countedDriverCalls) record(method, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[method+" "+path]++
}

func (c *countedDriverCalls) snapshot() (map[string]int, map[string]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	all, mutations := map[string]int{}, map[string]int{}
	for key, count := range c.calls {
		all[key] = count
		if strings.HasPrefix(key, "POST /v2/routes/grants") ||
			strings.HasPrefix(key, "DELETE /v2/routes/grants/") ||
			strings.HasPrefix(key, "POST /v2/messages") {
			mutations[key] = count
		}
	}
	return all, mutations
}

func startCountingDriverProxy(t *testing.T, proxySocket, realSocket string) (*http.Server, *countedDriverCalls) {
	t.Helper()
	listener, err := net.Listen("unix", proxySocket)
	if err != nil {
		t.Fatalf("listen Driver counting proxy: %v", err)
	}
	upstream, _ := url.Parse("http://driver.local")
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", realSocket)
	}}
	reverse := httputil.NewSingleHostReverseProxy(upstream)
	reverse.Transport = transport
	calls := &countedDriverCalls{calls: map[string]int{}}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.record(r.Method, r.URL.Path)
		reverse.ServeHTTP(w, r)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Close()
		transport.CloseIdleConnections()
		_ = os.Remove(proxySocket)
	})
	return server, calls
}

func reserveTCPAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve TCP address: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	return addr
}

func writeOwnerOnly(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatalf("write %s: %v", filepath.Base(path), err)
	}
}

func assertOwnerOnlyRegularFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("%s must be an owner-only regular file, mode=%s", path, info.Mode())
	}
}

func replaceProcessEnv(base []string, values map[string]string) []string {
	out := make([]string, 0, len(base)+len(values))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if _, replace := values[key]; !replace {
			out = append(out, entry)
		}
	}
	for key, value := range values {
		out = append(out, key+"="+value)
	}
	return out
}

func waitHTTPStatus(t *testing.T, cmd *exec.Cmd, endpoint string, want int, logPath string) string {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(endpoint)
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			_ = response.Body.Close()
			if response.StatusCode == want {
				return string(body)
			}
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			t.Fatalf("serve exited before %s became ready\n%s", endpoint, readTestLog(logPath))
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to return %d\n%s", endpoint, want, readTestLog(logPath))
	return ""
}

func readTestLog(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}
