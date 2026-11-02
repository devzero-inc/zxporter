package main

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// The pprof server is wired by hand rather than by a blank import, so the thing worth
// testing is that the wiring is actually there: a blank import would compile, look
// enabled, and serve nothing, because net/http/pprof registers on DefaultServeMux and
// this binary never serves that mux.
//
// These run against the real server on a real listener. netmon itself cannot be started
// on a developer machine -- conntrack is Linux-only and main exits before reaching the
// HTTP servers -- so the handler wiring has no other seam to be checked at.

// startTestPprofServer brings the server up on a free port and returns its base URL,
// restoring the flag globals afterwards so the tests do not leak into each other.
func startTestPprofServer(t *testing.T, mutexFraction, blockRate int) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}

	prevAddr, prevMutex, prevBlock := *pprofAddr, *pprofMutexFraction, *pprofBlockRate
	*pprofAddr, *pprofMutexFraction, *pprofBlockRate = addr, mutexFraction, blockRate
	t.Cleanup(func() {
		*pprofAddr, *pprofMutexFraction, *pprofBlockRate = prevAddr, prevMutex, prevBlock
	})

	srv := startPprofServer(logr.Discard())
	if srv == nil {
		t.Fatal("startPprofServer returned nil for a non-empty address")
	}
	t.Cleanup(func() {
		_ = srv.Close()
	})

	// ListenAndServe runs in its own goroutine, so the port is not necessarily bound
	// when this returns. Dialled rather than slept on.
	base := "http://" + addr
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return base
		}
		if time.Now().After(deadline) {
			t.Fatalf("pprof server never accepted a connection on %s: %v", addr, err)
		}
	}
}

func TestPprofServerIsOffByDefault(t *testing.T) {
	prev := *pprofAddr
	*pprofAddr = ""
	t.Cleanup(func() { *pprofAddr = prev })

	if srv := startPprofServer(logr.Discard()); srv != nil {
		t.Fatal("pprof server started with an empty bind address; it must be opt-in")
	}
}

func TestPprofServerServesProfiles(t *testing.T) {
	base := startTestPprofServer(t, 0, 0)
	client := &http.Client{Timeout: 30 * time.Second}

	// One per registered route. The index is what makes heap, goroutine and allocs
	// reachable -- they have no route of their own -- so a missing index would remove
	// most of what an operator came for while /debug/pprof/profile still answered.
	for _, path := range []string{
		"/debug/pprof/",
		"/debug/pprof/cmdline",
		"/debug/pprof/heap",
		"/debug/pprof/goroutine",
		"/debug/pprof/profile?seconds=1",
	} {
		t.Run(path, func(t *testing.T) {
			resp, err := client.Get(base + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer func() { _ = resp.Body.Close() }()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200: %s", path, resp.StatusCode, body)
			}
			if len(body) == 0 {
				t.Fatalf("GET %s returned an empty body", path)
			}
		})
	}
}

// A CPU profile is served by holding the request open for its full duration, which is
// why the server has no write timeout. This pins that: a profile longer than any timeout
// somebody might later think is a good idea must still complete.
func TestPprofProfileOutlivesAnyWriteTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("takes seconds by construction")
	}

	base := startTestPprofServer(t, 0, 0)
	client := &http.Client{Timeout: 60 * time.Second}

	start := time.Now()
	resp, err := client.Get(base + "/debug/pprof/profile?seconds=6")
	if err != nil {
		t.Fatalf("fetching a 6s profile: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the profile: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("profile = %d, want 200: %s", resp.StatusCode, body)
	}
	if elapsed := time.Since(start); elapsed < 6*time.Second {
		t.Fatalf("profile returned after %s, want at least the 6s requested", elapsed)
	}
	if len(body) == 0 {
		t.Fatal("profile body is empty")
	}
}
