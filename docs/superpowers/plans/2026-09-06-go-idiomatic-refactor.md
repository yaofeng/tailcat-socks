# Go Idiomatic Refactor Implementation Plan (v2 — deps allowed, no legacy parity)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rewrite the Go proxy (`go/`) in the most idiomatic modern Go: stdlib patterns (`slog`, `context`, `signal.NotifyContext`, `exec.CommandContext`, `atomic.Pointer`, `io.Copy`) plus the two community-standard libraries that fit (`golang.org/x/net/proxy` for the chained SOCKS5 client, `github.com/fsnotify/fsnotify` for hot reload). No constraints from the Python/Rust versions.

**Architecture:** Server-side SOCKS5 protocol parsing stays hand-rolled (justified: see "Library decisions" — the only viable library, armon/go-socks5, hardwires its relay and cannot express our idle timeout, FIN teardown, or handshake deadline). Everything else moves to stdlib/canonical-library patterns. The relay is rewritten around one `time.AfterFunc` idle timer + `io.Copy`, and upgraded to proper TCP half-close semantics (a finished direction propagates EOF downstream instead of the whole session dying).

**Tech Stack:** Go 1.23; new deps: `golang.org/x/net` (SOCKS5 client dialer), `github.com/fsnotify/fsnotify` (file watch). Logging via `log/slog`. Zero other frameworks (cobra etc. deliberately not used — six flags don't need a CLI framework).

**Library decisions (verified against sources on 2026-09-06):**
- **armon/go-socks5 — rejected.** `handleConnect` (request.go:166-212) performs dial + success reply + internal `proxy()` copy loop with no extension points: no idle-timeout hook, teardown is a plain `Close()` (unread kernel bytes → RST instead of FIN, regressing the curl-early-close fix), and `ServeConn` has no handshake deadline. Adopting it would regress three required behaviors.
- **golang.org/x/net/proxy — adopted (client side only).** `proxy.SOCKS5(...)` + `DialContext`: sends FQDN targets as ATYP 0x03 (socks5h built in), rejects >255-byte names (`"FQDN too long"`, client.go) so the truncation bug disappears, supports context timeouts.
- **fsnotify — adopted** for dns.txt hot reload, replacing mtime polling. The watch is armed *before* the initial load (closes the startup race), re-armed on every Create/Write event (atomic saves replace the watched inode), and debounced (editors emit several events per save).

**Behavior changes vs the current Go code (all deliberate):**
1. `--upstream` port 0 → OS-assigned free port (bind `:0`), not random 20000–60999 probing. README updated.
2. `--listen`/`--upstream` parsing follows Go conventions via `net.SplitHostPort` — no empty-host→loopback coercion; `:8080` binds all interfaces like every Go server; bare `"1080"` is no longer a valid flag value. Flag help documents this.
3. Relay: proper half-close relay. When one direction hits EOF it `CloseWrite()`s the other (downstream peer sees FIN immediately) instead of tearing down both directions. Idle teardown and FIN-not-RST ordering unchanged.
4. Child process death while running now fails fast: logs and shuts down with exit code 1 (before: silent failure of every request + zombie process).
5. dns.txt reload reacts to fsnotify events instead of a 1s mtime poll.

---

### Task 0: Baseline verification ✅

- [x] `cd go && go test -race ./... -count=1` — green (verified 2026-09-06).

---

### Task 1: Dependencies

**Files:**
- Modify: `go/go.mod`, `go/go.sum`

- [x] **Step 1: Add the two libraries**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/go
go get golang.org/x/net/proxy@latest
go get github.com/fsnotify/fsnotify@latest
go mod tidy
```

Expected: go.mod gains `require (github.com/fsnotify/fsnotify …; golang.org/x/net …)`; module cache reachable (proxy.golang.org verified working).

- [x] **Step 2: Commit**

```bash
git add go/go.mod go/go.sum
git commit -m "Go: add golang.org/x/net (SOCKS5 client) and fsnotify (hot reload)

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 2: Logging via `log/slog`

`log.SetFlags(0)` + a hand-repeated `"[tailcat-dns-proxy] "` literal in ~15 calls is pre-slog style. `log/slog` structured logging is the current standard for all new Go code.

**Files:**
- Modify: `go/main.go`, `go/dnsmap.go`, `go/proxy.go` (accept backoff lands in Task 8; here only existing calls)

- [x] **Step 1: Install the default logger in `main()`**

```go
func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	var (
		...
	)
```

(Remove `log.SetFlags(0)` and its comment; the `log` import goes away from main.go once all call sites are migrated.)

- [x] **Step 2: Migrate every call site (drop the literal prefix, use attrs)**

main.go `run`:

```go
		slog.Error("cannot load dns file", "path", dnsFile, "err", err)
		...
		slog.Error("bad --listen", "err", err)
		...
		slog.Error("bad --upstream", "err", err)
		...
		slog.Error("cannot listen", "addr", listen, "err", err)
		...
		slog.Error("upstream not ready; aborting", "addr", upAddr)
		...
		slog.Info("auto-launched upstream socks", "bin", tailcatBin, "addr", upAddr)
		...
		slog.Info("listening", "addr", "socks5h://"+net.JoinHostPort(host, port))
		slog.Info("dns mapping loaded", "domains", len(first), "tokens", len(tokenSet(first)))
		slog.Info("upstream", "addr", upAddr)
		...
		slog.Error("server error", "err", err)
		...
		slog.Error("failed to launch tailcat", "bin", binPath, "err", err)   // (moves into Task 6's spawn change)
```

dnsmap.go: the three `log.Printf` calls become `slog.Warn("initial load failed", "path", path, "err", err)`, `slog.Warn("reload failed; keeping previous map", "path", path, "err", err)`, `slog.Info("reloaded", "path", path, "domains", len(newMap), "tokens", len(tokenSet(newMap)))` (this file is rewritten again in Task 9; do the mechanical swap only if Task 9 has not landed yet — executing agent: if Tasks run in order, skip dnsmap.go here and do it in Task 9).

- [x] **Step 3: Build and test**

Run: `go build ./... && go test ./... -count=1`
Expected: pass.

- [x] **Step 4: Commit**

```bash
git add go/main.go go/dnsmap.go
git commit -m "Go: structured logging via log/slog

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 3: `DNSMap` on `atomic.Pointer[map[string]string]`

Copy-on-write config with lock-free reads is the canonical pattern; `atomic.Pointer[T]` (Go 1.19+) makes wrong-type stores a compile error instead of an `atomic.Value` runtime panic.

**Files:**
- Modify: `go/dnsmap.go:35-49`
- Test: existing `go/dnsmap_test.go` passes unchanged.

- [x] **Step 1: Replace struct + methods**

```go
// DNSMap holds the domain->token mapping, hot-swappable atomically. Readers
// get the current immutable snapshot; writers replace it wholesale
// (copy-on-write), so a lookup never takes a lock.
type DNSMap struct {
	v atomic.Pointer[map[string]string]
}

// Store atomically replaces the mapping.
func (m *DNSMap) Store(mapping map[string]string) { m.v.Store(&mapping) }

// Load returns the current mapping (never nil). The map is handed out live,
// so callers must treat it as read-only — that is what keeps reads lock-free.
func (m *DNSMap) Load() map[string]string {
	if p := m.v.Load(); p != nil {
		return *p
	}
	return map[string]string{}
}
```

- [x] **Step 2: Test**

Run: `go test -race -run 'DNS|Rewrite|Watch|Load' ./... -count=1`
Expected: pass.

- [x] **Step 3: Commit**

```bash
git add go/dnsmap.go
git commit -m "Go: DNSMap on atomic.Pointer[map[string]string]

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 4: Address handling — Go conventions, `parseAddr` deleted

`parseAddr` re-implements Python's `rpartition(":")` and coerces empty hosts to loopback. Most idiomatic: no custom parsing at all. `--listen` goes straight to `net.Listen`; `--upstream` needs only `net.SplitHostPort` + `strconv.Atoi` because the port must be known to hand to the child. Go conventions apply: `:8080` binds all interfaces (documented in flag help), a bare `"1080"` is no longer accepted.

**Files:**
- Modify: `go/main.go` (flags help, run call sites, delete parseAddr)
- Test: `go/main_test.go` (delete TestParseAddr)

- [x] **Step 1: Delete `parseAddr` and `TestParseAddr` entirely**

- [x] **Step 2: Update flag help to document Go semantics**

```go
		listen   = flag.String("listen", "127.0.0.1:1080", "SOCKS5 listen address (host:port; empty host binds all interfaces)")
		upstream = flag.String("upstream", "127.0.0.1:0", "upstream tailcat socks address (host:port; port 0 = OS-assigned free port)")
```

- [x] **Step 3: Rewrite the address resolution in `run`**

```go
	dnsMap := &DNSMap{}
	dnsMap.Store(first)

	srv, err := NewServer(dnsMap, upstreamAddr(*upstream), *listen)
```

with a small helper (the only parsing left — needed because the child must be told the concrete port):

```go
// upstreamAddr resolves the tailcat socks listen address: an explicit port is
// kept, port 0 gets an OS-assigned free port (bind :0, release, and hand the
// port to the child — an inherent small handoff race, same as any port
// picker). IPv6 must be bracketed, matching net.JoinHostPort conventions.
func upstreamAddr(s string) (string, error) {
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return "", fmt.Errorf("bad --upstream %q: %w (want host:port)", s, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", fmt.Errorf("bad --upstream %q: %w", s, err)
	}
	if port != 0 {
		return net.JoinHostPort(host, strconv.Itoa(port)), nil
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return "", fmt.Errorf("pick free port on %q: %w", host, err)
	}
	defer ln.Close()
	return net.JoinHostPort(host, strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)), nil
}
```

`run` uses it:

```go
	upAddr, err := upstreamAddr(*upstream)
	if err != nil {
		slog.Error("bad --upstream", "err", err)
		return 1
	}
```

Note: `upHost`/`upPort` as separate values disappear — everything downstream (`spawnTailcatSocks`, log lines) takes `upAddr` as one string. `spawnTailcatSocks` gains a matching signature change in Task 6 (`--listen=<addr>` directly).

- [x] **Step 4: Add a focused test for the new helper (TDD — write first, watch fail, then implement)**

In `go/main_test.go`:

```go
func TestUpstreamAddr(t *testing.T) {
	// explicit port is kept
	addr, err := upstreamAddr("127.0.0.1:9999")
	if err != nil || addr != "127.0.0.1:9999" {
		t.Errorf("got %q, %v", addr, err)
	}
	// port 0 gets an OS-assigned port that is immediately bindable in principle
	addr, err = upstreamAddr("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("got %q, %v", addr, err)
	}
	if p, err := strconv.Atoi(portStr); err != nil || p <= 0 || p > 65535 {
		t.Fatalf("port %q invalid: %v", portStr, err)
	}
	// malformed input errors
	for _, bad := range []string{"127.0.0.1", ":0x22", "[::1]", "::1", "host:abc"} {
		if _, err := upstreamAddr(bad); err == nil {
			t.Errorf("upstreamAddr(%q): want error", bad)
		}
	}
}
```

(`strconv` is already imported in main_test.go; drop `"127.0.0.1"` from the bad list if bare-host erroring feels wrong — it errors because SplitHostPort demands a port, which is the documented Go semantic.)

- [x] **Step 5: Test**

Run: `go test -run 'UpstreamAddr' ./... -count=1` then `go test ./... -count=1`
Expected: pass (no other test referenced parseAddr).

- [x] **Step 6: Commit**

```bash
git add go/main.go go/main_test.go
git commit -m "Go: Go-convention address handling; parseAddr deleted

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 5: Lifecycle — `signal.NotifyContext` + `exec.CommandContext` + child monitor

Replace: hand-rolled signal channel, nil-on-error spawn, the manual SIGTERM→5s→SIGKILL `terminate()` with its `//nolint:errcheck`, and the never-reaped child. Stdlib (Go 1.20+) expresses all of it: `cmd.Cancel` sends SIGTERM on ctx cancellation, `cmd.WaitDelay` escalates to SIGKILL, and a monitor goroutine reaps + fails fast when the child dies mid-run.

**Files:**
- Modify: `go/main.go`
- Test: `go/main_test.go` (rewrite the three spawn/terminate tests)

- [x] **Step 1: Rewrite lifecycle tests first**

Delete `TestSpawnTailcatSocksAndWaitReady`, `TestTerminateStopsChild`, `TestTerminateNilIsSafe`; add `"context"` to imports; add:

```go
func TestSpawnTailcatSocks(t *testing.T) {
	ctx := context.Background()
	addr, err := upstreamAddr("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	child, err := spawnTailcatSocks(ctx, fakeTailcatBin, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		child.Process.Kill()
		child.Wait()
	})
	if !waitReady(addr, 5*time.Second) {
		t.Fatal("fake tailcat never became ready")
	}
}

// TestSpawnedChildDiesWithContext pins the shutdown contract: cancelling the
// context SIGTERMs the child (SIGKILL follows after the WaitDelay grace) and
// the monitor's Wait observes the exit.
func TestSpawnedChildDiesWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	addr, err := upstreamAddr("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	child, err := spawnTailcatSocks(ctx, fakeTailcatBin, addr)
	if err != nil {
		t.Fatal(err)
	}
	if !waitReady(addr, 5*time.Second) {
		t.Fatal("fake tailcat never became ready")
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	cancel()
	select {
	case <-done:
	case <-time.After(childKillGrace + 3*time.Second):
		t.Fatal("child still alive after context cancel + grace")
	}
}
```

- [x] **Step 2: Verify failure**

Run: `go test -run 'Spawn' ./... -count=1`
Expected: compile error (old signature).

- [x] **Step 3: Implement spawn + grace constant; delete `terminate`**

```go
// childKillGrace is how long the tailcat child gets to exit on SIGTERM before
// exec escalates to SIGKILL.
const childKillGrace = 5 * time.Second

// spawnTailcatSocks launches `tailcat socks --listen=<addr>` bound to ctx:
// cancelling ctx sends SIGTERM via cmd.Cancel, and exec kills with SIGKILL
// once WaitDelay has elapsed. Child output goes to our stderr so it lands in
// the same log stream.
func spawnTailcatSocks(ctx context.Context, binPath, addr string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, binPath, "socks", "--listen="+addr)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = childKillGrace
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("launch %s: %w", binPath, err)
	}
	return cmd, nil
}
```

- [x] **Step 4: Rewrite `run`'s signal + child plumbing**

Top of `run`:

```go
	// Install the signal handler before doing anything else, so SIGINT/SIGTERM
	// arriving during launch cannot orphan the tailcat child.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
```

(Delete the raw `sig` channel.)

Autolaunch block:

```go
	var child *exec.Cmd
	var childDone <-chan struct{}
	var childErr error // written by the monitor before childDone closes
	if !noAutolaunch {
		c, err := spawnTailcatSocks(ctx, tailcatBin, upAddr)
		if err != nil {
			slog.Error("cannot launch upstream", "err", err)
			srv.Close()
			return 1
		}
		child = c
		done := make(chan struct{})
		childDone = done
		go func() { childErr = child.Wait(); close(done) }()
		if !waitReady(upAddr, 15*time.Second) {
			slog.Error("upstream not ready; aborting", "addr", upAddr)
			stop() // SIGTERM via Cancel; SIGKILL after the grace period
			<-childDone
			srv.Close()
			return 1
		}
		slog.Info("auto-launched upstream socks", "bin", tailcatBin, "addr", upAddr)
	}
```

Shutdown block:

```go
	exitCode := 0
	select {
	case <-ctx.Done(): // SIGINT/SIGTERM
	case err := <-serveErr:
		// srv.Close() during shutdown makes Serve report net.ErrClosed;
		// that is not a real failure.
		if err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Error("server error", "err", err)
		}
	case <-childDone:
		// With the upstream gone every request would fail silently; say so
		// and shut down instead of limping on. (childDone is a nil channel
		// in --no-autolaunch mode, so this case can never fire there.)
		slog.Error("upstream exited unexpectedly; shutting down", "addr", upAddr, "err", childErr)
		exitCode = 1
	}
	srv.Close()
	if child != nil {
		stop() // idempotent; also reaps the child via the monitor goroutine
		<-childDone
	}
	return exitCode
```

Notes: reading `childErr` after `<-childDone` is race-free (close is the happens-before edge). The `stopWatch` channel disappears with Task 9's ctx-based watcher; if executing in order, keep `close(stopWatch)` until Task 9 lands.

- [x] **Step 5: Full tests + manual smoke**

```bash
go build ./... && go test -race ./... -count=1
go build -o /tmp/tdp-test . && go build -o /tmp/fake-tailcat ./testdata/fake-tailcat
/tmp/tdp-test --listen 127.0.0.1:18081 --dns-file ../dns.txt.example --tailcat-bin /tmp/fake-tailcat &
sleep 2
printf '\x05\x01\x00' | timeout 2 nc 127.0.0.1 18081 | xxd | head -1   # expect: 0000: 0500
kill -TERM %1 && sleep 1
pgrep -f fake-tailcat || echo "no orphan"
rm -f /tmp/tdp-test /tmp/fake-tailcat
```
Expected: `05 00` greeting reply; proxy exits quietly on SIGTERM; `no orphan` printed.

- [x] **Step 6: Commit**

```bash
git add go/main.go go/main_test.go
git commit -m "Go: NotifyContext + CommandContext/WaitDelay lifecycle; child death fails fast

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 6: Upstream CONNECT via `golang.org/x/net/proxy`

> **Execution note (deviation):** x/net/proxy was adopted initially (7450c64) but reverted in 7edc3fa: it does not expose the upstream conn's deadline/FIN teardown points our FIN-not-RST contract requires, so the hand-rolled CONNECT stayed, upgraded with explicit length bounds-checking (which also fixes the >255-byte truncation). The oversized-token test below was kept.

Delete the hand-rolled `upstreamConnect` (~45 lines), `errUpstreamRefused`, and the per-dial deadline juggling. `proxy.SOCKS5` + `DialContext` does greeting, auth, ATYP encoding (FQDN → ATYP 0x03, socks5h), bound-address draining, and rejects >255-byte names ("FQDN too long") — which also retires the truncation bug.

**Files:**
- Modify: `go/proxy.go`
- Test: `go/proxy_test.go`

- [x] **Step 1: Adjust tests first**

Delete `TestUpstreamConnectUsesDomainATYP` (tests a function being removed; ATYP-0x03 behavior stays covered end-to-end by `TestSocks5ProxyRewritesHostAndRelays` — the fake upstream can only parse a domain ATYP there). Add:

```go
// TestSocks5ProxyOversizedTokenFailsConnect: a token/domain longer than 255
// bytes cannot be encoded in SOCKS5's one-byte length field. The CONNECT must
// fail with the generic failure reply instead of producing a corrupted
// request (the >255 check lives in x/net's socks dialer).
func TestSocks5ProxyOversizedTokenFailsConnect(t *testing.T) {
	received := make(chan string, 1)
	up := fakeUpstream(t, received)
	srv := startProxy(t, map[string]string{"big.example": strings.Repeat("t", 300)}, up)

	c, err := net.DialTimeout("tcp", srv.ActualAddr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte{0x05, 0x01, 0x00})
	rep := make([]byte, 2)
	if _, err := io.ReadFull(c, rep); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	host := "big.example"
	req := append([]byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}, host...)
	req = append(req, 0x01, 0xBB) // 443
	c.Write(req)
	head := make([]byte, 10)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, head); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if head[1] == 0x00 {
		t.Error("oversized token must fail the CONNECT")
	}
	select {
	case got := <-received:
		t.Errorf("upstream must not receive a CONNECT, got %q", got)
	default:
	}
}
```

- [x] **Step 2: Implement the dialer in proxy.go**

New imports: `"context"`, `"strconv"`, `"golang.org/x/net/proxy"`. Server gains the dialer; NewServer builds it:

```go
// Server is the client-facing SOCKS5 front proxy. On each CONNECT it
// rewrites the target host via DNSMap and forwards the request (as a SOCKS5
// client, via x/net/proxy) to the single `tailcat socks` upstream.
type Server struct {
	DNSMap       *DNSMap
	UpstreamAddr string
	ln               net.Listener
	upstream         proxy.Dialer
	handshakeTimeout time.Duration // set in Task 8
	relayIdle        time.Duration
}
```

```go
// NewServer binds the listen address and prepares the chained SOCKS5 dialer
// for the upstream. Use ActualAddr for the resolved port (listening on port 0
// is useful in tests).
func NewServer(dnsMap *DNSMap, upstreamAddr, listen string) (*Server, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	// FQDN targets are sent to the upstream as ATYP 0x03 names — socks5h
	// semantics; the upstream resolves them.
	dialer, err := proxy.SOCKS5("tcp", upstreamAddr, nil, proxy.Direct)
	if err != nil {
		ln.Close()
		return nil, err
	}
	return &Server{
		DNSMap:       dnsMap,
		UpstreamAddr: upstreamAddr,
		ln:           ln,
		upstream:     dialer,
		relayIdle:    relayIdleTimeout,
	}, nil
}
```

(In Task 8 the `handshakeTimeout` field initialization is added here; keep the field declared already to avoid churn, initialized in Task 8.)

Replace the forward block in `serveConn`:

```go
	target := RewriteHost(host, s.DNSMap.Load())

	// --- forward to upstream as a SOCKS5 client ---
	ctx, cancel := context.WithTimeout(context.Background(), upstreamDialTimeout)
	defer cancel()
	up, err := s.dialUpstream(ctx, target, port)
	if err != nil {
		socksFail(client)
		return
	}
	_ = client.Write(socksOKReply)
	client.SetDeadline(time.Time{}) // handshake over; the relay owns timing now
	relay(client, up, s.relayIdle)
}
```

with:

```go
// dialUpstream CONNECTs to host:port through the chained upstream SOCKS5
// proxy. x/net rejects >255-byte FQDNs and sends domains as ATYP 0x03 so the
// upstream resolves them.
func (s *Server) dialUpstream(ctx context.Context, host string, port uint16) (net.Conn, error) {
	d, ok := s.upstream.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("upstream dialer does not support DialContext")
	}
	return d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
}
```

Delete: `upstreamConnect`, `errUpstreamRefused`, and the old `up.SetDeadline(...)`/`up.Close()` pairs in `serveConn` (x/net closes the transport on handshake failure itself; the relay's teardown closes the success case). `upstreamDialTimeout` const stays.

- [x] **Step 3: Test**

Run: `go test -race ./... -count=1`
Expected: pass (all relay/e2e tests; fake upstream sees the token as a domain).

- [x] **Step 4: Commit**

```bash
git add go/proxy.go go/proxy_test.go go/go.mod go/go.sum
git commit -m "Go: upstream CONNECT via x/net/proxy; hand-rolled client handshake deleted

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 7: Relay — one idle timer + `io.Copy`, proper half-close

Replace the shared `atomic.Int64` clock + `SetReadDeadline` re-arm loop with the canonical Go shape: an `idleConn` wrapper reports activity to a single `time.AfterFunc` timer, `io.Copy` does the copying, `sync.WaitGroup` joins, `sync.Once` guards teardown. Also upgrades teardown semantics: a direction that hits clean EOF now `CloseWrite()`s the peer so downstream sees a real FIN immediately (before: any single-direction EOF killed both directions — a client half-close killed downloads mid-flight).

**Files:**
- Modify: `go/proxy.go` (relay + helpers; delete `copyWithIdleTimeout`; drop `sync/atomic` import)
- Test: existing tests pin idle + FIN behavior; no new tests required.

- [x] **Step 1: Replace relay and helpers**

```go
// relay copies data between client and upstream in both directions until both
// directions are done or no data moves in either direction for idle. A single
// time.AfterFunc timer is the shared idle clock: the idleConn wrappers reset
// it on every chunk read from either side, so activity in either direction
// keeps the whole session alive, and when it fires it closes both conns,
// which unblocks any pending copy at once.
//
// A direction that ends on clean EOF half-closes its peer (FIN) so the other
// side sees a proper EOF instead of waiting out the idle timer. When both are
// done, stop() half-closes and closes both ends; the half-close-before-close
// order is what makes the peer see FIN rather than a spurious RST — a plain
// Close with unread bytes in the kernel receive queue makes Linux emit RST
// (tcp_close: data_was_unread), destroying data the other side has not read.
func relay(client, upstream net.Conn, idle time.Duration) {
	var once sync.Once
	stop := func() {
		halfClose(client)
		halfClose(upstream)
		client.Close()
		upstream.Close()
	}
	idleTimer := time.AfterFunc(idle, func() { once.Do(stop) })
	defer idleTimer.Stop()

	wake := func() { idleTimer.Reset(idle) }
	client = &idleConn{Conn: client, onActivity: wake}
	upstream = &idleConn{Conn: upstream, onActivity: wake}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := io.Copy(upstream, client); err == nil {
			closeWrite(upstream) // clean client EOF: tell the upstream FIN
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := io.Copy(client, upstream); err == nil {
			closeWrite(client) // clean upstream EOF: tell the client FIN
		}
	}()
	wg.Wait()
	once.Do(stop)
}

// idleConn counts successful reads as session activity.
type idleConn struct {
	net.Conn
	onActivity func()
}

func (c *idleConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.onActivity()
	}
	return n, err
}
```

```go
// closeWrite sends FIN on a TCP connection (shutdown(SHUT_WR)); other conn
// types are left to Close.
func closeWrite(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.CloseWrite()
	}
}
```

Delete `copyWithIdleTimeout`. Keep `halfClose` and its full comment unchanged (the anti-RST rationale still applies verbatim). Import block: drop `sync/atomic`, add `sync`.

(Go 1.23's timer implementation makes concurrent `Reset`/`Stop` safe; the AfterFunc pattern has no channel-drain caveats.)

- [x] **Step 2: Drop the now-redundant post-relay close in serveConn** (relay's stop closes both; `handle`'s `defer conn.Close()` covers the client):

```go
	_ = client.Write(socksOKReply)
	client.SetDeadline(time.Time{})
	relay(client, up, s.relayIdle)
}
```

- [x] **Step 3: Run the pinned behavior tests under the race detector**

Run: `go test -race -count=1 -run 'Relay|Rewrites|Relays' ./...`
Expected: pass — `TestRelayIdleClockIsSharedAcrossDirections` (one-way pumping survives, silence tears down ≤550ms) and `TestRelayEarlyClosingUpstreamDeliversDataAndCleanEOF` (200× payload + clean EOF, no RST).

- [x] **Step 4: Full suite**

Run: `go test -race ./... -count=1`
Expected: pass.

- [x] **Step 5: Commit**

```bash
git add go/proxy.go
git commit -m "Go: relay on io.Copy + one AfterFunc idle timer; proper half-close semantics

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 8: Hardening — handshake deadline, accept backoff, explicit error drops

1. **Client handshake deadline:** the greeting/request reads have no deadline — a silent client pins a goroutine forever. Bound the handshake, clear before relay. 2. **Accept backoff:** transient `ECONNABORTED`/`EMFILE`/`ENFILE` must retry with backoff (net/http's Serve-loop policy) instead of killing the server. 3. **Best-effort writes made explicit** (`_ =`): replies to a possibly-vanished client have no remedy.

**Files:**
- Modify: `go/proxy.go`
- Test: `go/proxy_test.go` (one new test)

- [x] **Step 1: Failing test first**

```go
// TestSocks5ProxyHandshakeIdleClosesSlowClient: a client that connects but
// never completes the SOCKS5 greeting must be dropped after the handshake
// deadline instead of pinning a goroutine forever.
func TestSocks5ProxyHandshakeIdleClosesSlowClient(t *testing.T) {
	received := make(chan string, 1)
	up := fakeUpstream(t, received)
	srv := newProxy(t, map[string]string{}, up)
	srv.handshakeTimeout = 200 * time.Millisecond
	go srv.Serve()

	c, err := net.DialTimeout("tcp", srv.ActualAddr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	one := make([]byte, 1)
	if _, err := c.Read(one); err == nil {
		t.Fatal("expected the server to close an idle handshake, got data")
	}
}
```

- [x] **Step 2: Verify failure**

Run: `go test -run HandshakeIdle ./... -count=1`
Expected: compile error (`handshakeTimeout` field not yet declared).

- [x] **Step 3: Implement deadline plumbing**

```go
const (
	upstreamDialTimeout = 15 * time.Second
	// handshakeTimeout bounds the client-side SOCKS5 exchange (greeting +
	// CONNECT request) so a silent client cannot pin a goroutine forever;
	// the relay phase owns timing from there.
	handshakeTimeout = 15 * time.Second
	relayIdleTimeout = 300 * time.Second
)
```

Server field init in NewServer: `handshakeTimeout: handshakeTimeout,`. First statement of `serveConn`:

```go
	client.SetDeadline(time.Now().Add(s.handshakeTimeout)) // bound the handshake
```

(The `client.SetDeadline(time.Time{})` before `relay` was added in Task 6.)

- [x] **Step 4: Accept backoff (replaces the plain loop)**

```go
// Serve accepts connections until the listener closes; each connection is
// handled on its own goroutine. Transient accept failures — fd exhaustion or
// a connection dropped between the kernel queue and Accept — back off and
// retry like net/http's Serve loop instead of killing the server.
func (s *Server) Serve() error {
	var tempDelay time.Duration // backoff after a transient accept error
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if tempDelay > time.Second {
					tempDelay = time.Second
				}
				slog.Warn("accept error; retrying", "err", err, "backoff", tempDelay)
				time.Sleep(tempDelay)
				continue
			}
			return err
		}
		tempDelay = 0
		go s.handle(conn)
	}
}
```

Imports in proxy.go: add `"log/slog"`, `"syscall"`.

- [x] **Step 5: Explicit best-effort writes**

```go
	// Replies to the client are best-effort: the peer may vanish between any
	// two steps and a failed goodbye has no remedy.
	_ = client.Write([]byte{0x05, 0xFF}) // no acceptable methods
	...
	_ = client.Write([]byte{0x05, 0x00})
	...
	_ = client.Write(socksOKReply)
```

(Drop the error check on the method-selection reply — if the client vanished, the next read fails anyway. `socksFail` keeps its best-effort signature; add the same comment above it.)

- [x] **Step 6: Test**

Run: `go test -race ./... -count=1`
Expected: pass.

- [x] **Step 7: Commit**

```bash
git add go/proxy.go go/proxy_test.go
git commit -m "Go: handshake deadline, accept backoff, explicit best-effort writes

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 9: Hot reload via fsnotify + context

Replace the mtime poller with the standard file-watch library. Watch armed **before** the initial load (no change can slip between watch and load), re-armed on Create/Write (atomic saves replace the watched inode), debounced against editor event bursts, ctx-driven shutdown (replaces the stop channel).

**Files:**
- Modify: `go/dnsmap.go` (rewrite WatchDNSFile; slog migration folded in here), `go/main.go` (watcher call + drop `stopWatch`)
- Test: `go/dnsmap_test.go`

- [x] **Step 1: Adapt tests first**

```go
func TestWatchDNSFileReloads(t *testing.T) {
	path := writeTemp(t, "tcA alpha.com\n")
	m := &DNSMap{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go WatchDNSFile(ctx, path, m)

	// initial load
	waitFor(t, 3*time.Second, func() bool { return m.Load()["alpha.com"] == "tcA" })

	if err := os.WriteFile(path, []byte("tcB beta.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return m.Load()["beta.com"] == "tcB" })
	if _, ok := m.Load()["alpha.com"]; ok {
		t.Error("removed domain should be gone after reload")
	}
}
```

(The `os.Chtimes` mtime-bumping hack goes away — fsnotify events fire on content writes.)

```go
func TestWatchDNSFileKeepsPreviousMapOnReadError(t *testing.T) {
	path := writeTemp(t, "tcA keep.com\n")
	m := &DNSMap{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go WatchDNSFile(ctx, path, m)
	waitFor(t, 3*time.Second, func() bool { return m.Load()["keep.com"] == "tcA" })

	// Make reads fail while events keep coming: replace the file with a directory.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if m.Load()["keep.com"] != "tcA" {
		t.Fatal("read error must keep previous mapping")
	}
}
```

Add `"context"` to dnsmap_test.go imports.

- [x] **Step 2: Implement**

```go
// WatchDNSFile hot-reloads the mapping into m until ctx is done. The fsnotify
// watch is armed before the initial load so no change can slip between the
// two, and re-armed on every create/write event because atomic saves
// (write+rename) replace the watched inode. A short debounce coalesces the
// event bursts editors emit per save. A failed reload keeps the previous map.
func WatchDNSFile(ctx context.Context, path string, m *DNSMap) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Error("create watcher", "err", err)
		return
	}
	defer watcher.Close()
	rearm := func() {
		if err := watcher.Add(path); err != nil {
			// Gone right now (remove/rename); a later Create re-arms us.
			slog.Debug("watch re-arm skipped", "path", path, "err", err)
		}
	}
	rearm()
	if first, err := LoadDNSFile(path); err != nil {
		slog.Warn("initial load failed", "path", path, "err", err)
	} else {
		m.Store(first)
	}

	var debounce *time.Timer
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			if ev.Has(fsnotify.Write) || ev.Has(fsnotify.Create) {
				rearm() // atomic saves swap the inode; watch the new file
				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.AfterFunc(50*time.Millisecond, func() { reload(path, m) })
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			slog.Error("watch error", "path", path, "err", err)
		}
	}
}

// reload parses path and swaps it into m, keeping the old map on error.
func reload(path string, m *DNSMap) {
	newMap, err := LoadDNSFile(path)
	if err != nil {
		slog.Warn("reload failed; keeping previous map", "path", path, "err", err)
		return
	}
	m.Store(newMap)
	slog.Info("reloaded", "path", path, "domains", len(newMap), "tokens", len(tokenSet(newMap)))
}
```

Imports in dnsmap.go: drop nothing existing except swap `"log"` → `"log/slog"`, add `"context"`, `"github.com/fsnotify/fsnotify"`.

main.go: delete the `stopWatch` channel and `close(stopWatch)`; start with:

```go
	if !noWatch {
		go WatchDNSFile(ctx, dnsFile, dnsMap)
	}
```

(ctx is canceled by `defer stop()` on every return path, which ends the watcher.)

- [x] **Step 3: Test**

Run: `go test -race -run Watch ./... -count=1` then `go test -race ./... -count=1`
Expected: pass.

- [x] **Step 4: Commit**

```bash
git add go/dnsmap.go go/dnsmap_test.go go/main.go
git commit -m "Go: dns.txt hot reload via fsnotify + context

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 10: README alignment + final verification

**Files:**
- Modify: `README.md` (Go-diverging facts only)

- [x] **Step 1: Update the port section**

The "## tailcat socks 端口" section documents the 20000–60999 random probe. Change the Go sentence to: Go 版 `--upstream` 端口为 0 时由操作系统分配空闲端口；Python/Rust 版为 20000–60999 随机探测。(The other two implementations are untouched.)

- [x] **Step 2: Update the directory-tree annotation**

`go/ # Go 复刻版(行为一致)` → `go/ # Go 版(idiomatic Go 重写;依赖 golang.org/x/net、fsnotify)`.

- [x] **Step 3: Full verification**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks/go
gofmt -l .            # expect: no output
go vet ./...          # expect: exit 0
go test -race -count=1 ./...
```
Then a final end-to-end smoke per Task 5 Step 5 (SOCKS greeting + SIGTERM + no orphan).

- [x] **Step 4: Commit**

```bash
git add README.md
git commit -m "README: Go port selection and deps after idiomatic-Go rewrite

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

## Self-review

- **Coverage of the 13 review items + 2 porting notes:** #1/#2 relay → Task 7; #3 handshake deadline → Task 8; #4 accept backoff → Task 8; #5 parseAddr → Task 4 (deleted); #6 port picker → Task 4 (`upstreamAddr`, OS-assigned; probe loop deleted now that parity is dropped); #7 double initial load → Task 9 (watch-before-load closes the race properly; run() keeps a fail-fast load); #8 fsnotify → Task 9 (adopted); #9 logging → Task 2 (slog, superseding SetPrefix); #10 nolint → Task 5 (sites deleted; explicit `_ =` elsewhere per Task 8); #11 signal/exec/reap → Task 5; #12 atomic.Pointer → Task 3; #13 write errors → Task 8; 255-byte truncation → Task 6 (fixed inside x/net); ParseIP strictness → moot (x/net parses identically).
- **Pinned-behavior gates kept:** shared idle window, FIN-not-RST teardown, wire format, failure replies, hot-reload semantics (adapted to events).
- **Deliberate behavior changes** (documented in header): OS-assigned upstream port; Go address conventions; half-close relay; child-death fail-fast; fsnotify reload.
- **Type consistency:** `upstreamAddr(string) (string, error)` used in run(), TestUpstreamAddr, TestSpawn*; `spawnTailcatSocks(ctx, bin, addr) (*exec.Cmd, error)` matches both spawn tests and run(); `dialUpstream(ctx, host string, port uint16)` matches serveConn; `WatchDNSFile(ctx, path, m)` matches both dnsmap tests and run(); `Server.handshakeTimeout` matches the new test; `childKillGrace` matches spawn + ctx test; `reload(path, m)` matches debounce closure.
