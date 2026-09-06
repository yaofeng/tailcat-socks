//! End-to-end: the real proxy main-loop wiring (minus signal handling) driven
//! against the fake-tailcat binary — the same shape as the Go e2e/smoke tests.

use std::time::Duration;
use tailcat_socks::config::{free_high_port, join_host_port};
use tailcat_socks::dnsmap::{load_dns_file, DnsMap};
use tailcat_socks::proxy::Server;
use tailcat_socks::tailcat::{spawn_socks, terminate, wait_ready};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;

fn fake_bin() -> String {
    env!("CARGO_BIN_EXE_fake-tailcat").to_string()
}

/// Mirrors go/main.go run() happy path: spawn tailcat socks on a free high
/// port, wait until ready, then bind the server. Go's run() binds BEFORE
/// spawning (so it can Close the listener if the child launch fails); this
/// mirror deliberately reorders — the test binds :0 and never needs that
/// teardown path.
async fn boot(
    dns_txt: &str,
) -> (
    String,
    tokio::process::Child,
    tokio::task::JoinHandle<std::io::Result<()>>,
    std::path::PathBuf,
) {
    let dir = std::env::temp_dir().join(format!(
        "tcdns-e2e-{}-{}",
        std::process::id(),
        free_high_port("127.0.0.1")
    ));
    std::fs::create_dir_all(&dir).unwrap();
    let dns_path = dir.join("dns.txt");
    std::fs::write(&dns_path, dns_txt).unwrap();

    let mapping = load_dns_file(&dns_path).expect("load dns.txt");
    let dns_map = DnsMap::new();
    dns_map.store(mapping);

    let port = free_high_port("127.0.0.1");
    let up_addr = join_host_port("127.0.0.1", port);
    let child = spawn_socks(&fake_bin(), "127.0.0.1", port)
        .await
        .expect("spawn tailcat");
    assert!(
        wait_ready(&up_addr, Duration::from_secs(5)).await,
        "tailcat not ready"
    );

    let (ln, srv) = Server::bind(dns_map, &up_addr, "127.0.0.1:0")
        .await
        .unwrap();
    let listen_addr = ln.local_addr().unwrap().to_string();
    let handle = tokio::spawn(async move { srv.serve(ln).await });
    (listen_addr, child, handle, dir)
}

/// SOCKS5 CONNECT via domain ATYP, returns the connected stream. Every
/// network wait is bounded (2s) so a relay/handshake regression fails the
/// test instead of hanging the suite — same shape as proxy.rs's socks_connect.
async fn socks_connect(proxy_addr: &str, host: &[u8], port: u16) -> TcpStream {
    let mut c = tokio::time::timeout(Duration::from_secs(2), TcpStream::connect(proxy_addr))
        .await
        .expect("dial proxy timed out")
        .expect("dial proxy failed");
    c.write_all(&[0x05, 0x01, 0x00]).await.unwrap();
    let mut rep = [0u8; 2];
    tokio::time::timeout(Duration::from_secs(2), c.read_exact(&mut rep))
        .await
        .expect("greeting read timed out")
        .unwrap();
    assert_eq!(&rep, &[0x05, 0x00], "greeting refused: {rep:?}");
    let mut req = vec![0x05, 0x01, 0x00, 0x03, host.len() as u8];
    req.extend_from_slice(host);
    req.extend_from_slice(&port.to_be_bytes());
    c.write_all(&req).await.unwrap();
    let mut head = [0u8; 10];
    tokio::time::timeout(Duration::from_secs(2), c.read_exact(&mut head))
        .await
        .expect("reply read timed out")
        .unwrap();
    assert_eq!(head[1], 0x00, "CONNECT failed: {head:?}");
    c
}

/// What this pins is the PROCESS BOUNDARY and full wiring: spawn_socks ->
/// the real fake-tailcat binary -> wait_ready -> Server::bind/serve ->
/// SOCKS5 handshake -> relay. The domain->token rewrite itself is NOT
/// observable here (fake-tailcat answers any CONNECT and ignores the target,
/// so the CONNECT port 8888 is unobservable too); the rewrite is pinned by
/// the unit tests in proxy.rs.
#[tokio::test]
async fn full_chain_rewrite_via_fake_tailcat() {
    let (addr, mut child, handle, dir) =
        boot("tcXYZabc123   www.example.com  api.example.com\ntcDEF456ghi   foo.com\n").await;

    // mapped domain: fake tailcat echoes, so we see our own payload back
    // (the token it was rewritten to and the CONNECT port are both invisible
    // to this fake)
    let mut c = socks_connect(&addr, b"www.example.com", 8888).await;
    c.write_all(b"ping").await.unwrap();
    let mut buf = [0u8; 4];
    tokio::time::timeout(Duration::from_secs(2), c.read_exact(&mut buf))
        .await
        .expect("echo never came back")
        .unwrap();
    assert_eq!(&buf, b"ping", "fake tailcat echo through full chain");
    drop(c);

    // second domain on the same token still works
    let mut c = socks_connect(&addr, b"api.example.com", 8888).await;
    c.write_all(b"pong").await.unwrap();
    let mut buf = [0u8; 4];
    tokio::time::timeout(Duration::from_secs(2), c.read_exact(&mut buf))
        .await
        .expect("echo never came back")
        .unwrap();
    assert_eq!(&buf, b"pong");
    drop(c);

    // domain on the SECOND token (second dns.txt line) relays as well
    let mut c = socks_connect(&addr, b"foo.com", 8888).await;
    c.write_all(b"tick").await.unwrap();
    let mut buf = [0u8; 4];
    tokio::time::timeout(Duration::from_secs(2), c.read_exact(&mut buf))
        .await
        .expect("echo never came back")
        .unwrap();
    assert_eq!(&buf, b"tick");
    drop(c);

    handle.abort();
    terminate(&mut child).await;
    let _ = child.wait().await;
    let _ = std::fs::remove_dir_all(&dir);
}
