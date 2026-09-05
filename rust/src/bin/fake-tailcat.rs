//! fake-tailcat stands in for `tailcat socks` in tests: it parses
//! `socks --listen=host:port` and runs a minimal SOCKS5 server that accepts
//! any CONNECT, replies success, and echoes data back. cargo builds it as a
//! normal bin target, so integration tests get its path for free via
//! env!("CARGO_BIN_EXE_fake-tailcat").
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

fn main() {
    let runtime = tokio::runtime::Runtime::new().expect("tokio runtime");
    runtime.block_on(async_main());
}

async fn async_main() {
    // Like the real `tailcat socks --listen=...`, the subcommand comes
    // before the flag; clap stops at the first non-flag argument, so drop a
    // leading "socks" before parsing.
    let args: Vec<String> = std::env::args()
        .skip(1)
        .skip_while(|a| a == "socks")
        .collect();
    let mut listen = String::from("127.0.0.1:0");
    let mut iter = args.iter();
    while let Some(a) = iter.next() {
        if a == "--listen" {
            listen = iter.next().cloned().unwrap_or(listen);
        } else if let Some(v) = a.strip_prefix("--listen=") {
            listen = v.to_string();
        }
    }

    let ln = TcpListener::bind(&listen).await.expect("fake tailcat bind");
    let addr = ln.local_addr().expect("local addr");
    eprintln!("fake tailcat socks listening on {addr}");
    loop {
        let Ok((conn, _)) = ln.accept().await else {
            return;
        };
        tokio::spawn(async move { serve(conn).await });
    }
}

async fn serve(mut conn: TcpStream) {
    let mut v = [0u8; 1];
    if conn.read_exact(&mut v).await.is_err() {
        return;
    }
    if v[0] != 0x05 {
        return;
    }
    let mut nm = [0u8; 1];
    if conn.read_exact(&mut nm).await.is_err() {
        return;
    }
    let mut methods = vec![0u8; nm[0] as usize];
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
    if conn
        .write_all(&[0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0])
        .await
        .is_err()
    {
        return;
    }
    // echo everything back
    let mut buf = [0u8; 4096];
    loop {
        match conn.read(&mut buf).await {
            Ok(0) | Err(_) => return,
            Ok(n) => {
                if conn.write_all(&buf[..n]).await.is_err() {
                    return;
                }
            }
        }
    }
}
