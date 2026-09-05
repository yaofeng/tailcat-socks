use crate::dnsmap::{rewrite_host, DnsMap};
use crate::error::SocksError;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

/// Bound on the upstream dial + SOCKS5 handshake, like the Python version's
/// create_connection(timeout=15) / Go's SetDeadline.
pub const UPSTREAM_DIAL_TIMEOUT: Duration = Duration::from_secs(15);
/// Mirror of the Python/Go versions: the relay breaks after 300s with no
/// data in either direction.
pub const RELAY_IDLE_TIMEOUT: Duration = Duration::from_secs(300);

/// Byte-identical success/failure replies (BOUND.ADDR 0.0.0.0:0, as in Go).
const SOCKS_OK_REPLY: [u8; 10] = [0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0];
const SOCKS_FAIL_REPLY: [u8; 10] = [0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0];

/// Client-facing SOCKS5 front proxy. On each CONNECT it rewrites the target
/// host via DnsMap and forwards the request (as a SOCKS5 client) to the
/// single `tailcat socks` upstream. Cheaply clonable: every connection task
/// holds a clone, all sharing the same DnsMap.
#[derive(Clone)]
pub struct Server {
    pub dns_map: DnsMap,
    pub upstream_addr: String,
    /// Relay shared-idle window; tests shrink it.
    pub relay_idle: Duration,
}

impl Server {
    /// Bind `listen`. Drive the accept loop with `serve(ln)`.
    pub async fn bind(
        dns_map: DnsMap,
        upstream_addr: &str,
        listen: &str,
    ) -> std::io::Result<(TcpListener, Self)> {
        let ln = TcpListener::bind(listen).await?;
        let srv = Self {
            dns_map,
            upstream_addr: upstream_addr.to_string(),
            relay_idle: RELAY_IDLE_TIMEOUT,
        };
        Ok((ln, srv))
    }

    /// Accept loop: one spawned task per connection. Returns when the
    /// listener errors (the caller drops it on shutdown).
    pub async fn serve(&self, ln: TcpListener) -> std::io::Result<()> {
        loop {
            let (conn, _peer) = ln.accept().await?;
            let me = self.clone();
            tokio::spawn(async move { me.handle(conn).await });
        }
    }

    async fn handle(&self, conn: TcpStream) {
        // Per-connection protocol noise is silently dropped, like Go's
        // early returns.
        let _ = self.serve_conn(conn).await;
    }

    async fn serve_conn(&self, mut client: TcpStream) -> Result<(), SocksError> {
        // --- greeting ---
        let mut v = [0u8; 1];
        client.read_exact(&mut v).await?;
        if v[0] != 0x05 {
            return Ok(());
        }
        let mut nm = [0u8; 1];
        client.read_exact(&mut nm).await?;
        let mut methods = vec![0u8; nm[0] as usize];
        client.read_exact(&mut methods).await?;
        if !methods.contains(&0x00) {
            client.write_all(&[0x05, 0xFF]).await?; // no acceptable methods
            return Ok(());
        }
        client.write_all(&[0x05, 0x00]).await?;

        // --- request ---
        let mut req = [0u8; 4];
        client.read_exact(&mut req).await?;
        let (cmd, atyp) = (req[1], req[3]);
        let host = read_atyp_addr(&mut client, atyp).await?;
        let mut pb = [0u8; 2];
        client.read_exact(&mut pb).await?;
        let port = u16::from_be_bytes(pb);

        if cmd != 0x01 {
            // only CONNECT (BIND / UDP ASSOCIATE fail, as in Go)
            client.write_all(&SOCKS_FAIL_REPLY).await?;
            return Ok(());
        }

        let target = rewrite_host(&host, &self.dns_map.load());

        // --- forward to upstream as a SOCKS5 client; dial + handshake share
        // one bounded window, like Go's SetDeadline around the handshake ---
        let upstream_fut = async {
            let mut up = TcpStream::connect(self.upstream_addr.as_str()).await?;
            upstream_connect(&mut up, &target, port).await?;
            Ok::<TcpStream, SocksError>(up)
        };
        let mut up = match tokio::time::timeout(UPSTREAM_DIAL_TIMEOUT, upstream_fut).await {
            Ok(Ok(up)) => up,
            Ok(Err(_)) | Err(_) => {
                client.write_all(&SOCKS_FAIL_REPLY).await?;
                return Ok(());
            }
        };

        client.write_all(&SOCKS_OK_REPLY).await?;
        relay(&mut client, &mut up, self.relay_idle).await;
        Ok(())
    }
}

/// Read a SOCKS5 address of the given ATYP. IPv4 textualization matches Go;
/// IPv6 matches Go for ordinary addresses (see the 0x04 branch for the one
/// IPv4-mapped divergence). Domains keep their raw bytes, non-UTF-8 included
/// (deliberate, shared with Go).
async fn read_atyp_addr<R: AsyncRead + Unpin>(r: &mut R, atyp: u8) -> Result<Vec<u8>, SocksError> {
    match atyp {
        0x01 => {
            let mut b = [0u8; 4];
            r.read_exact(&mut b).await?;
            Ok(std::net::Ipv4Addr::new(b[0], b[1], b[2], b[3])
                .to_string()
                .into_bytes())
        }
        0x03 => {
            let mut l = [0u8; 1];
            r.read_exact(&mut l).await?;
            let mut b = vec![0u8; l[0] as usize];
            r.read_exact(&mut b).await?;
            Ok(b)
        }
        0x04 => {
            let mut b = [0u8; 16];
            r.read_exact(&mut b).await?;
            // Divergence from Go (matches Python's inet_ntop instead): Rust
            // textualizes IPv4-mapped IPv6 keeping the `::ffff:` prefix
            // ("::ffff:127.0.0.1"), where Go strips it ("127.0.0.1") — so a
            // mapped client address re-forwards as ATYP 4 here, ATYP 1 in Go.
            Ok(std::net::Ipv6Addr::from(b).to_string().into_bytes())
        }
        other => Err(SocksError::UnsupportedAtyp(other)),
    }
}

/// SOCKS ATYP classification on raw bytes: textual IPv4/IPv6 -> ATYP 1/4,
/// everything else (domains, tokens, non-UTF-8) -> ATYP 3.
enum Atyp {
    V4(std::net::Ipv4Addr),
    V6(std::net::Ipv6Addr),
    Domain,
}

fn atyp_for(host: &[u8]) -> Atyp {
    match std::str::from_utf8(host) {
        Ok(s) => {
            if let Ok(ip4) = s.parse::<std::net::Ipv4Addr>() {
                Atyp::V4(ip4)
            } else if let Ok(ip6) = s.parse::<std::net::Ipv6Addr>() {
                Atyp::V6(ip6)
            } else {
                Atyp::Domain
            }
        }
        // raw bytes cannot be an IP string
        Err(_) => Atyp::Domain,
    }
}

/// SOCKS5 client handshake with the upstream, asking it to connect to
/// host:port. Domains go as ATYP 0x03 (socks5h: the upstream resolves);
/// IPs use ATYP 0x01/0x04.
pub(crate) async fn upstream_connect(
    up: &mut TcpStream,
    host: &[u8],
    port: u16,
) -> Result<(), SocksError> {
    up.write_all(&[0x05, 0x01, 0x00]).await?;
    let mut greet = [0u8; 2];
    up.read_exact(&mut greet).await?;
    if greet != [0x05, 0x00] {
        return Err(SocksError::UpstreamRefused);
    }
    let mut req = match atyp_for(host) {
        Atyp::V4(ip) => {
            let mut r = vec![0x05, 0x01, 0x00, 0x01];
            r.extend_from_slice(&ip.octets());
            r
        }
        Atyp::V6(ip6) => {
            let mut r = vec![0x05, 0x01, 0x00, 0x04];
            r.extend_from_slice(&ip6.octets());
            r
        }
        Atyp::Domain => {
            // >255 truncates exactly like Go's byte(len(host))
            let mut r = vec![0x05, 0x01, 0x00, 0x03, host.len() as u8];
            r.extend_from_slice(host);
            r
        }
    };
    req.extend_from_slice(&port.to_be_bytes());
    up.write_all(&req).await?;

    let mut rep = [0u8; 4];
    up.read_exact(&mut rep).await?;
    if rep[0] != 0x05 || rep[1] != 0x00 {
        return Err(SocksError::UpstreamRefused);
    }
    // drain the bound address
    read_atyp_addr(up, rep[3]).await?;
    let mut bnd = [0u8; 2];
    up.read_exact(&mut bnd).await?;
    Ok(())
}

fn now_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0)
}

/// Copy one direction until EOF/error/idle-timeout. The idle clock is SHARED
/// across directions (mirrors the Python select() loop): traffic either way
/// resets the window, so a healthy one-way transfer is never killed as idle.
/// A blocked Read cannot observe clock updates, so after a timeout firing we
/// re-arm whenever another direction has moved the clock past the deadline
/// that just expired — only true idleness returns. Writes stay deadline-free.
async fn pump(
    rd: &mut tokio::net::tcp::ReadHalf<'_>,
    wr: &mut tokio::net::tcp::WriteHalf<'_>,
    idle: Duration,
    clock: &AtomicU64,
) -> std::io::Result<()> {
    let idle_ms = idle.as_millis() as u64;
    let mut buf = vec![0u8; 65536];
    loop {
        let deadline = clock.load(Ordering::Relaxed) + idle_ms;
        let wait = deadline.saturating_sub(now_ms());
        match tokio::time::timeout(Duration::from_millis(wait), rd.read(&mut buf)).await {
            Err(_elapsed) => {
                let new_deadline = clock.load(Ordering::Relaxed) + idle_ms;
                if new_deadline > deadline {
                    continue; // activity elsewhere extended the window
                }
                return Err(std::io::Error::new(
                    std::io::ErrorKind::TimedOut,
                    "relay idle timeout",
                ));
            }
            Ok(Err(e)) => return Err(e),
            Ok(Ok(0)) => return Ok(()), // EOF
            Ok(Ok(n)) => {
                clock.store(now_ms(), Ordering::Relaxed);
                wr.write_all(&buf[..n]).await?;
            }
        }
    }
}

/// Bidirectional relay with the shared idle clock and the Go/Python anti-RST
/// teardown: when either direction finishes, queue a FIN on both sockets
/// (`shutdown(Write)` — once FIN is queued, the kernel close answers FIN, not
/// RST, even with unread data still queued) and then drop both. Dropping the
/// losing pump cancels it and releases its halves — Go unblocks the second
/// direction by closing the fds; dropping the future is the tokio equivalent.
pub async fn relay(client: &mut TcpStream, upstream: &mut TcpStream, idle: Duration) {
    let clock = AtomicU64::new(now_ms());
    {
        let (mut c_rd, mut c_wr) = client.split();
        let (mut u_rd, mut u_wr) = upstream.split();
        let _ = tokio::select! {
            r = pump(&mut u_rd, &mut c_wr, idle, &clock) => r,
            r = pump(&mut c_rd, &mut u_wr, idle, &clock) => r,
        };
    }
    // half-close both ends (FIN), then drop = close
    let _ = client.shutdown().await;
    let _ = upstream.shutdown().await;
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;
    use std::sync::Arc;

    /// Minimal SOCKS5 upstream (stand-in for `tailcat socks`): accepts any
    /// CONNECT, records the target on the channel, echoes data back
    /// "ECHO:"-prefixed. Mirrors go/proxy_test.go fakeUpstream.
    async fn fake_upstream(
        received: tokio::sync::mpsc::UnboundedSender<String>,
    ) -> std::io::Result<String> {
        let ln = TcpListener::bind("127.0.0.1:0").await?;
        let addr = ln.local_addr()?.to_string();
        tokio::spawn(async move {
            loop {
                let Ok((mut conn, _)) = ln.accept().await else {
                    return;
                };
                let received = received.clone();
                tokio::spawn(async move {
                    let mut hdr = [0u8; 2];
                    if conn.read_exact(&mut hdr).await.is_err() {
                        return;
                    }
                    let mut methods = vec![0u8; hdr[1] as usize];
                    if conn.read_exact(&mut methods).await.is_err() {
                        return;
                    }
                    if conn.write_all(&[0x05, 0x00]).await.is_err() {
                        return;
                    }
                    let mut req = [0u8; 4];
                    if conn.read_exact(&mut req).await.is_err() {
                        return;
                    }
                    let host: Vec<u8> = match req[3] {
                        0x03 => {
                            let mut l = [0u8; 1];
                            if conn.read_exact(&mut l).await.is_err() {
                                return;
                            }
                            let mut b = vec![0u8; l[0] as usize];
                            if conn.read_exact(&mut b).await.is_err() {
                                return;
                            }
                            b
                        }
                        0x01 => {
                            let mut b = [0u8; 4];
                            if conn.read_exact(&mut b).await.is_err() {
                                return;
                            }
                            std::net::Ipv4Addr::new(b[0], b[1], b[2], b[3])
                                .to_string()
                                .into_bytes()
                        }
                        _ => b"?".to_vec(),
                    };
                    let mut pb = [0u8; 2];
                    if conn.read_exact(&mut pb).await.is_err() {
                        return;
                    }
                    let port = u16::from_be_bytes(pb);
                    let _ = received.send(format!("{}:{}", String::from_utf8_lossy(&host), port));
                    if conn.write_all(&SOCKS_OK_REPLY).await.is_err() {
                        return;
                    }
                    let mut buf = [0u8; 4096];
                    loop {
                        match conn.read(&mut buf).await {
                            Ok(0) | Err(_) => return,
                            Ok(n) => {
                                let mut out = b"ECHO:".to_vec();
                                out.extend_from_slice(&buf[..n]);
                                if conn.write_all(&out).await.is_err() {
                                    return;
                                }
                            }
                        }
                    }
                });
            }
        });
        Ok(addr)
    }

    /// Dial the proxy and complete greeting + CONNECT (domain ATYP).
    async fn socks_connect(proxy_addr: &str, host: &[u8], port: u16) -> TcpStream {
        let mut c = tokio::time::timeout(Duration::from_secs(2), TcpStream::connect(proxy_addr))
            .await
            .expect("dial proxy timed out")
            .expect("dial proxy failed");
        c.write_all(&[0x05, 0x01, 0x00]).await.unwrap();
        let mut rep = [0u8; 2];
        c.read_exact(&mut rep).await.unwrap();
        assert_eq!(&rep, &[0x05, 0x00], "greeting refused: {rep:?}");
        let mut req = vec![0x05, 0x01, 0x00, 0x03, host.len() as u8];
        req.extend_from_slice(host);
        req.extend_from_slice(&port.to_be_bytes());
        c.write_all(&req).await.unwrap();
        let mut head = [0u8; 10];
        c.read_exact(&mut head).await.unwrap();
        assert_eq!(head[1], 0x00, "CONNECT failed: {head:?}");
        c
    }

    /// Build a Server from (domain, token) pairs and run its accept loop.
    /// Returns the bound address, the server, and the serve task handle.
    async fn start_proxy(
        mapping: &[(&[u8], &[u8])],
        upstream: &str,
    ) -> (String, Server, tokio::task::JoinHandle<std::io::Result<()>>) {
        let m = mapping
            .iter()
            .map(|(d, t)| (d.to_ascii_lowercase(), t.to_vec()))
            .collect::<HashMap<Vec<u8>, Vec<u8>>>();
        let dns_map = DnsMap::new();
        dns_map.store(m);
        let (ln, srv) = Server::bind(dns_map, upstream, "127.0.0.1:0")
            .await
            .unwrap();
        let addr = ln.local_addr().unwrap().to_string();
        let handle = tokio::spawn({
            let srv = srv.clone();
            async move { srv.serve(ln).await }
        });
        (addr, srv, handle)
    }

    async fn recv_target(rx: &mut tokio::sync::mpsc::UnboundedReceiver<String>) -> String {
        tokio::time::timeout(Duration::from_secs(2), rx.recv())
            .await
            .expect("upstream never received the CONNECT")
            .expect("sender dropped")
    }

    // Mirrors go/proxy_test.go TestSocks5ProxyRewritesHostAndRelays.
    #[tokio::test]
    async fn rewrites_host_and_relays() {
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        let up = fake_upstream(tx).await.unwrap();
        let (addr, _srv, handle) = start_proxy(&[(b"www.example.com", b"tcXYZabc123")], &up).await;

        let mut c = socks_connect(&addr, b"www.example.com", 8081).await;
        c.write_all(b"hello").await.unwrap();
        let mut buf = vec![0u8; b"ECHO:hello".len()];
        tokio::time::timeout(Duration::from_secs(2), c.read_exact(&mut buf))
            .await
            .expect("relay read timed out")
            .unwrap();
        assert_eq!(buf, b"ECHO:hello");
        drop(c);
        handle.abort();

        assert_eq!(recv_target(&mut rx).await, "tcXYZabc123:8081");
    }

    // Mirrors TestSocks5ProxyRewritesCaseInsensitive.
    #[tokio::test]
    async fn rewrites_case_insensitive() {
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        let up = fake_upstream(tx).await.unwrap();
        let (addr, _srv, handle) = start_proxy(&[(b"www.example.com", b"tcXYZabc123")], &up).await;
        let _c = socks_connect(&addr, b"WWW.Example.COM", 80).await;
        drop(_c);
        handle.abort();
        assert_eq!(recv_target(&mut rx).await, "tcXYZabc123:80");
    }

    // Mirrors TestSocks5ProxyPassesUnmatchedHostThrough.
    #[tokio::test]
    async fn passes_unmatched_host_through() {
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        let up = fake_upstream(tx).await.unwrap();
        let (addr, _srv, handle) = start_proxy(&[(b"www.example.com", b"tcXYZabc123")], &up).await;
        let _c = socks_connect(&addr, b"other.com", 9999).await;
        drop(_c);
        handle.abort();
        assert_eq!(recv_target(&mut rx).await, "other.com:9999");
    }

    // Mirrors TestSocks5ProxyRejectsBind.
    #[tokio::test]
    async fn rejects_bind() {
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        let up = fake_upstream(tx).await.unwrap();
        let (addr, _srv, handle) = start_proxy(&[], &up).await;

        let mut c = TcpStream::connect(&addr).await.unwrap();
        c.write_all(&[0x05, 0x01, 0x00]).await.unwrap();
        let mut rep = [0u8; 2];
        c.read_exact(&mut rep).await.unwrap();
        // CMD=2 (BIND) with an IPv4 ATYP
        c.write_all(&[0x05, 0x02, 0x00, 0x01, 127, 0, 0, 1, 0x1F, 0x90])
            .await
            .unwrap();
        let mut head = [0u8; 10];
        c.read_exact(&mut head).await.unwrap();
        assert_eq!(head[1], 0x01, "BIND must fail with 0x01, got {head:?}");
        handle.abort();
        assert!(rx.try_recv().is_err(), "upstream must not receive anything");
    }

    // Mirrors TestSocks5ProxyNoAcceptableMethod.
    #[tokio::test]
    async fn no_acceptable_method() {
        let (addr, _srv, handle) = start_proxy(&[], "127.0.0.1:1").await;
        let mut c = TcpStream::connect(&addr).await.unwrap();
        c.write_all(&[0x05, 0x01, 0xFF]).await.unwrap(); // only user/pass auth
        let mut rep = [0u8; 2];
        c.read_exact(&mut rep).await.unwrap();
        assert_eq!(&rep, &[0x05, 0xFF], "want 05 FF, got {rep:?}");
        handle.abort();
    }

    /// start_proxy with a short relay idle window (for exercising the shared
    /// idle clock without waiting 300s).
    async fn start_proxy_with_idle(
        mapping: &[(&[u8], &[u8])],
        upstream: &str,
        idle: Duration,
    ) -> (String, tokio::task::JoinHandle<std::io::Result<()>>) {
        let m = mapping
            .iter()
            .map(|(d, t)| (d.to_ascii_lowercase(), t.to_vec()))
            .collect::<HashMap<Vec<u8>, Vec<u8>>>();
        let dns_map = DnsMap::new();
        dns_map.store(m);
        let (ln, mut srv) = Server::bind(dns_map, upstream, "127.0.0.1:0")
            .await
            .unwrap();
        srv.relay_idle = idle;
        let addr = ln.local_addr().unwrap().to_string();
        let handle = tokio::spawn({
            let srv = srv.clone();
            async move { srv.serve(ln).await }
        });
        (addr, handle)
    }

    /// Completes the SOCKS5 handshake and then swallows everything without
    /// ever writing back, counting payload bytes.
    async fn silent_upstream() -> std::io::Result<(String, Arc<AtomicU64>)> {
        let ln = TcpListener::bind("127.0.0.1:0").await?;
        let addr = ln.local_addr()?.to_string();
        let got = Arc::new(AtomicU64::new(0));
        let counter = Arc::clone(&got);
        tokio::spawn(async move {
            let Ok((mut conn, _)) = ln.accept().await else {
                return;
            };
            let mut hdr = [0u8; 2];
            let _ = conn.read_exact(&mut hdr).await;
            let mut methods = vec![0u8; hdr[1] as usize];
            let _ = conn.read_exact(&mut methods).await;
            let _ = conn.write_all(&[0x05, 0x00]).await;
            let mut req = [0u8; 4];
            let _ = conn.read_exact(&mut req).await;
            match req[3] {
                0x03 => {
                    let mut l = [0u8; 1];
                    let _ = conn.read_exact(&mut l).await;
                    let mut b = vec![0u8; l[0] as usize];
                    let _ = conn.read_exact(&mut b).await;
                }
                0x01 => {
                    let mut b = [0u8; 4];
                    let _ = conn.read_exact(&mut b).await;
                }
                0x04 => {
                    let mut b = [0u8; 16];
                    let _ = conn.read_exact(&mut b).await;
                }
                _ => {}
            }
            let mut pb = [0u8; 2];
            let _ = conn.read_exact(&mut pb).await;
            let _ = conn.write_all(&SOCKS_OK_REPLY).await;
            let mut buf = [0u8; 4096];
            loop {
                match conn.read(&mut buf).await {
                    Ok(0) | Err(_) => return,
                    Ok(n) => {
                        counter.fetch_add(n as u64, Ordering::SeqCst);
                    }
                }
            }
        });
        Ok((addr, got))
    }

    /// Answer CONNECT, write payload, close WITHOUT reading anything more
    /// (the client's HTTP GET arrives after our close — the original
    /// curl-exit-56 RST scenario).
    async fn early_close_upstream(payload: &[u8]) -> std::io::Result<String> {
        let ln = TcpListener::bind("127.0.0.1:0").await?;
        let addr = ln.local_addr()?.to_string();
        let payload = payload.to_vec();
        tokio::spawn(async move {
            loop {
                let Ok((mut conn, _)) = ln.accept().await else {
                    return;
                };
                let payload = payload.clone();
                tokio::spawn(async move {
                    let mut hdr = [0u8; 2];
                    if conn.read_exact(&mut hdr).await.is_err() {
                        return;
                    }
                    let mut methods = vec![0u8; hdr[1] as usize];
                    if conn.read_exact(&mut methods).await.is_err() {
                        return;
                    }
                    if conn.write_all(&[0x05, 0x00]).await.is_err() {
                        return;
                    }
                    let mut req = [0u8; 4];
                    if conn.read_exact(&mut req).await.is_err() {
                        return;
                    }
                    let skip = match req[3] {
                        0x03 => {
                            let mut l = [0u8; 1];
                            if conn.read_exact(&mut l).await.is_err() {
                                return;
                            }
                            l[0] as usize
                        }
                        0x01 => 4,
                        0x04 => 16,
                        _ => 0,
                    };
                    let mut rest = vec![0u8; skip + 2];
                    if conn.read_exact(&mut rest).await.is_err() {
                        return;
                    }
                    let _ = conn.write_all(&SOCKS_OK_REPLY).await;
                    let _ = conn.write_all(&payload).await;
                    // ...and close without reading the client's request bytes.
                });
            }
        });
        Ok(addr)
    }

    /// Replies 05 00 to the greeting, then reads exactly `total` bytes
    /// (greeting + request) and hands them to the receiver. For byte-level
    /// assertions on what the proxy sends upstream.
    async fn capture_upstream(
        total: usize,
    ) -> std::io::Result<(String, tokio::sync::mpsc::Receiver<Vec<u8>>)> {
        let ln = TcpListener::bind("127.0.0.1:0").await?;
        let addr = ln.local_addr()?.to_string();
        let (tx, rx) = tokio::sync::mpsc::channel(1);
        tokio::spawn(async move {
            let Ok((mut conn, _)) = ln.accept().await else {
                return;
            };
            let _ = conn.write_all(&[0x05, 0x00]).await;
            let mut buf = vec![0u8; total];
            if conn.read_exact(&mut buf).await.is_err() {
                return;
            }
            let _ = tx.send(buf).await;
        });
        Ok((addr, rx))
    }

    /// Completes the handshake then refuses the CONNECT.
    async fn refuse_upstream() -> std::io::Result<String> {
        let ln = TcpListener::bind("127.0.0.1:0").await?;
        let addr = ln.local_addr()?.to_string();
        tokio::spawn(async move {
            let Ok((mut conn, _)) = ln.accept().await else {
                return;
            };
            let mut hdr = [0u8; 2];
            let _ = conn.read_exact(&mut hdr).await;
            let mut methods = vec![0u8; hdr[1] as usize];
            let _ = conn.read_exact(&mut methods).await;
            let _ = conn.write_all(&[0x05, 0x00]).await;
            let mut req = [0u8; 4];
            let _ = conn.read_exact(&mut req).await;
            let mut rest = vec![0u8; 4 + 2]; // ATYP 1 addr + port
            let _ = conn.read_exact(&mut rest).await;
            let _ = conn.write_all(&SOCKS_FAIL_REPLY).await;
        });
        Ok(addr)
    }

    // Mirrors go/proxy_test.go TestRelayIdleClockIsSharedAcrossDirections:
    // one shared clock reset by traffic in EITHER direction. A one-way
    // transfer (client pumps, upstream never answers) must not be killed by
    // the idle timeout just because one side is silent.
    #[tokio::test]
    async fn relay_idle_clock_is_shared_across_directions() {
        let (up, got) = silent_upstream().await.unwrap();
        // 200ms idle; the pump below runs 1.2s (6x idle) one-way only.
        let (addr, handle) = start_proxy_with_idle(&[], &up, Duration::from_millis(200)).await;

        let mut c = socks_connect(&addr, b"plain.example", 80).await;

        const PINGS: usize = 24;
        for _ in 0..PINGS {
            tokio::time::timeout(Duration::from_secs(2), c.write_all(b"PING\n"))
                .await
                .expect("write timed out")
                .expect("relay died during one-way transfer");
            tokio::time::sleep(Duration::from_millis(50)).await;
        }

        // Every ping must have made it through the relay (5 bytes each).
        let deadline = tokio::time::Instant::now() + Duration::from_secs(2);
        while got.load(Ordering::SeqCst) < (PINGS * 5) as u64
            && tokio::time::Instant::now() < deadline
        {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        assert!(
            got.load(Ordering::SeqCst) >= (PINGS * 5) as u64,
            "upstream got {} bytes, want {} (relay dropped one-way traffic)",
            got.load(Ordering::SeqCst),
            PINGS * 5
        );

        // Silence now: the relay must tear down within ~2x idle (client
        // sees EOF from the queued FIN).
        let quiet = std::time::Instant::now();
        let res = tokio::time::timeout(Duration::from_secs(5), c.read_u8()).await;
        let elapsed = quiet.elapsed();
        handle.abort();
        match res {
            Err(_) => panic!("no teardown after silence"),
            Ok(Ok(_)) => panic!("expected teardown, but read data instead"),
            Ok(Err(_)) => {}
        }
        assert!(
            elapsed <= Duration::from_millis(2 * 200 + 150),
            "teardown took {elapsed:?} after silence, want <= ~550ms"
        );
    }

    // Mirrors TestRelayEarlyClosingUpstreamDeliversDataAndCleanEOF: the
    // upstream answers CONNECT, writes a payload and closes WITHOUT reading
    // the client's request. The relay must still deliver the payload AND end
    // with a clean FIN — closing with unread data queued and no FIN queued
    // makes Linux answer RST, destroying data the peer has not read (the
    // original curl exit 56 bug).
    #[tokio::test]
    async fn relay_early_closing_upstream_delivers_clean_eof() {
        let payload = b"SAW:tcSMOKE123:9999\n".to_vec();
        let up = early_close_upstream(&payload).await.unwrap();
        let (addr, _srv, handle) = start_proxy(&[(b"www.example.com", b"tcSMOKE123")], &up).await;

        let mut failures: Vec<String> = Vec::new();
        for i in 0..200 {
            let mut c = socks_connect(&addr, b"www.example.com", 9999).await;
            // The client sends its GET the moment the success reply lands;
            // the upstream is already writing its payload and closing.
            c.write_all(b"GET / HTTP/1.0\r\n\r\n").await.unwrap();
            let mut buf = Vec::new();
            match tokio::time::timeout(Duration::from_secs(2), c.read_to_end(&mut buf)).await {
                Err(_) => failures.push(format!("iteration {}: read timed out", i + 1)),
                Ok(Err(e)) => failures.push(format!(
                    "iteration {}: read error after {} bytes: {e} (want clean EOF)",
                    i + 1,
                    buf.len()
                )),
                Ok(Ok(_)) => {
                    if buf != payload {
                        failures.push(format!(
                            "iteration {}: got {:?}, want {:?}",
                            i + 1,
                            buf,
                            payload
                        ));
                    }
                }
            }
        }
        handle.abort();
        assert!(
            failures.is_empty(),
            "{}/200 iterations failed:\n{}",
            failures.len(),
            failures.join("\n")
        );
    }

    // Mirrors TestUpstreamConnectUsesDomainATYP: capture the exact bytes the
    // proxy sends upstream; domains must go as ATYP 0x03 (socks5h).
    #[tokio::test]
    async fn upstream_connect_uses_domain_atyp() {
        let (up, mut captured) = capture_upstream(3 + 4 + 1 + "tcXYZabc123".len() + 2)
            .await
            .unwrap();
        let mut up_conn = TcpStream::connect(&up).await.unwrap();
        // The capture never sends the final reply, so upstream_connect ends
        // with an error once the capture side is dropped — the request bytes
        // are already captured and asserted below.
        let _ = crate::proxy::upstream_connect(&mut up_conn, b"tcXYZabc123", 443).await;
        drop(up_conn);
        let got = tokio::time::timeout(Duration::from_secs(2), captured.recv())
            .await
            .expect("no request captured")
            .unwrap();
        assert_eq!(&got[..3], &[0x05, 0x01, 0x00], "bad greeting bytes");
        let mut want = vec![0x05, 0x01, 0x00, 0x03, "tcXYZabc123".len() as u8];
        want.extend_from_slice(b"tcXYZabc123");
        want.extend_from_slice(&443u16.to_be_bytes());
        assert_eq!(&got[3..], &want[..], "request bytes differ");
    }

    // Mirrors TestSocks5ProxyForwardsIPv6AsATYP4: the upstream must receive
    // ATYP 4 plus the raw 16 address bytes and the port.
    #[tokio::test]
    async fn forwards_ipv6_as_atyp4() {
        let (up, mut captured) = capture_upstream(3 + 4 + 16 + 2).await.unwrap();
        let (addr, _srv, handle) = start_proxy(&[], &up).await;

        let mut c = TcpStream::connect(&addr).await.unwrap();
        c.write_all(&[0x05, 0x01, 0x00]).await.unwrap();
        let mut rep = [0u8; 2];
        c.read_exact(&mut rep).await.unwrap();
        let ip: std::net::Ipv6Addr = "2001:db8::1".parse().unwrap();
        let mut req = vec![0x05, 0x01, 0x00, 0x04];
        req.extend_from_slice(&ip.octets());
        req.extend_from_slice(&443u16.to_be_bytes());
        c.write_all(&req).await.unwrap();
        drop(c);
        handle.abort();

        let got = tokio::time::timeout(Duration::from_secs(2), captured.recv())
            .await
            .expect("no request captured")
            .unwrap();
        assert_eq!(got[6], 0x04, "upstream ATYP = {:#x}, want 0x04", got[6]);
        assert_eq!(&got[7..23], &ip.octets()[..], "upstream addr bytes");
        assert_eq!(&got[23..25], &[0x01, 0xBB], "upstream port bytes (443)");
    }

    // Mirrors TestSocks5ProxyUpstreamDialFailureRepliesFail: unreachable
    // upstream -> generic failure reply to the client.
    #[tokio::test]
    async fn upstream_dial_failure_replies_fail() {
        let ln = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let closed = ln.local_addr().unwrap().to_string();
        drop(ln); // nothing is listening there anymore

        let (addr, _srv, handle) = start_proxy(&[], &closed).await;
        let mut c = TcpStream::connect(&addr).await.unwrap();
        c.write_all(&[0x05, 0x01, 0x00]).await.unwrap();
        let mut rep = [0u8; 2];
        c.read_exact(&mut rep).await.unwrap();
        c.write_all(&[0x05, 0x01, 0x00, 0x03, 3, b'a', b'b', b'c', 0x00, 0x50])
            .await
            .unwrap();
        let mut head = [0u8; 10];
        c.read_exact(&mut head).await.unwrap();
        handle.abort();
        assert_eq!(&head, &SOCKS_FAIL_REPLY, "want generic failure reply");
    }

    // Mirrors TestSocks5ProxyUpstreamRefusalRepliesFail: a reachable upstream
    // that rejects the CONNECT also produces the generic failure reply.
    #[tokio::test]
    async fn upstream_refusal_replies_fail() {
        let up = refuse_upstream().await.unwrap();
        let (addr, _srv, handle) = start_proxy(&[], &up).await;
        let mut c = TcpStream::connect(&addr).await.unwrap();
        c.write_all(&[0x05, 0x01, 0x00]).await.unwrap();
        let mut rep = [0u8; 2];
        c.read_exact(&mut rep).await.unwrap();
        c.write_all(&[0x05, 0x01, 0x00, 0x01, 127, 0, 0, 1, 0x00, 0x50])
            .await
            .unwrap();
        let mut head = [0u8; 10];
        c.read_exact(&mut head).await.unwrap();
        handle.abort();
        assert_eq!(&head, &SOCKS_FAIL_REPLY, "want generic failure reply");
    }
}
