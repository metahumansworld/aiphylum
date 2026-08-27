package runner

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// These tests exercise the actual network policy against a real Docker
// daemon: an agent container attempts a direct provider call, a DNS lookup,
// and a raw socket to an external host, and all must fail, while the relay —
// the metered path — must work. They skip when Docker is absent (CI without a
// daemon) and under -short.

func testRunner(t *testing.T) *Runner {
	t.Helper()
	if testing.Short() {
		t.Skip("egress tests need Docker; skipped in -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !Available(ctx) {
		t.Skip("no usable docker daemon")
	}
	r := New(slog.Default())
	// A dedicated network name so the test cannot fight a running daemon.
	r.Network = "phylum-test-net"
	r.Subnet = "172.29.0.0/16"
	r.RelayIP = "172.29.0.2"
	if err := r.EnsureNetwork(context.Background()); err != nil {
		t.Fatalf("ensure network: %v", err)
	}
	return r
}

// runInAgentNet runs a python one-liner under the exact flags an agent gets.
func runInAgentNet(t *testing.T, r *Runner, name, code string) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	res, err := r.Run(ctx, AgentSpec{
		Name:    name,
		Cmd:     []string{"python3", "-c", code},
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("run %s: %v", name, err)
	}
	return res
}

func TestEgressIsDenied(t *testing.T) {
	r := testRunner(t)

	cases := []struct {
		name string
		code string
	}{
		{
			// A direct call to a provider by IP: the internal network has no
			// route out, so the connect must fail.
			"direct-ip",
			`import socket
s = socket.socket(); s.settimeout(5)
try:
    s.connect(("1.1.1.1", 443))
    print("ESCAPED")
except OSError as e:
    print("DENIED", e)`,
		},
		{
			// DNS: the container's resolver is 127.0.0.1, where nothing
			// answers, so provider hostnames must not resolve.
			"dns",
			`import socket
socket.setdefaulttimeout(5)
try:
    socket.getaddrinfo("api.anthropic.com", 443)
    print("ESCAPED")
except OSError as e:
    print("DENIED", e)`,
		},
		{
			// A raw UDP datagram to an external host: sending may not error
			// on an unrouted network, so prove no reply can ever arrive.
			"raw-socket",
			`import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(5)
try:
    s.sendto(b"x", ("8.8.8.8", 53))
    s.recvfrom(64)
    print("ESCAPED")
except OSError as e:
    print("DENIED", e)`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runInAgentNet(t, r, "egress-"+tc.name, tc.code)
			if strings.Contains(res.Stdout, "ESCAPED") {
				t.Fatalf("%s escaped the sandbox:\n%s%s", tc.name, res.Stdout, res.Stderr)
			}
			if !strings.Contains(res.Stdout, "DENIED") {
				t.Fatalf("%s produced neither DENIED nor ESCAPED:\nstdout: %s\nstderr: %s",
					tc.name, res.Stdout, res.Stderr)
			}
		})
	}
}

func TestRelayIsTheOnlyPathAndItWorks(t *testing.T) {
	r := testRunner(t)

	// A stand-in for the daemon's proxy: any host HTTP listener will do to
	// prove the path exists and is the only one.
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, "proxy-says-hello")
	})}
	go srv.Serve(ln)
	defer srv.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	if err := r.StartRelay(context.Background(), port); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	t.Cleanup(func() { r.StopRelay(context.Background()) })

	code := fmt.Sprintf(`import urllib.request
resp = urllib.request.urlopen(%q, timeout=15)
print(resp.read().decode())`, r.ProxyURL())
	res := runInAgentNet(t, r, "egress-relay-ok", code)
	if !strings.Contains(res.Stdout, "proxy-says-hello") {
		t.Fatalf("agent could not reach the proxy through the relay:\nstdout: %s\nstderr: %s",
			res.Stdout, res.Stderr)
	}
}
