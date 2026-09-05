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

/// Read a SOCKS5 address of the given ATYP. IPv4/IPv6 are textualized like
/// Go (so IP-literal mappings rewrite identically); domains keep their raw
/// bytes, non-UTF-8 included (deliberate, shared with Go).
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
async fn upstream_connect(up: &mut TcpStream, host: &[u8], port: u16) -> Result<(), SocksError> {
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
}
