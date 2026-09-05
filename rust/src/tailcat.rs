use std::process::Stdio;
use std::time::Duration;
use tokio::net::TcpStream;
use tokio::process::{Child, Command};
use tokio::time::Instant;

/// Spawn `tailcat socks --listen=host:port`. Child stdout/stderr are
/// inherited so its messages land in our log, like the Go version.
pub async fn spawn_socks(bin_path: &str, host: &str, port: u16) -> std::io::Result<Child> {
    Command::new(bin_path)
        .arg("socks")
        .arg(format!("--listen={host}:{port}"))
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit())
        .spawn()
}

/// Poll TCP-connect to `addr` until it accepts or the timeout hits.
pub async fn wait_ready(addr: &str, timeout: Duration) -> bool {
    let deadline = Instant::now() + timeout;
    while Instant::now() < deadline {
        if tokio::time::timeout(Duration::from_secs(1), TcpStream::connect(addr))
            .await
            .is_ok()
        {
            return true;
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
    false
}

/// Stop the child: SIGTERM, then SIGKILL after 5s. tokio's Child only offers
/// SIGKILL (start_kill); SIGTERM needs libc.
pub async fn terminate(child: &mut Child) {
    let Some(pid) = child.id() else {
        return; // already exited
    };
    // SAFETY: kill(2) with a pid we own; only signal delivery, no memory access.
    unsafe { libc::kill(pid as libc::pid_t, libc::SIGTERM) };
    match tokio::time::timeout(Duration::from_secs(5), child.wait()).await {
        Ok(_) => {}
        Err(_) => {
            let _ = child.start_kill();
            let _ = child.wait().await;
        }
    }
}
