//! Child-lifecycle tests for the auto-launched `tailcat socks`, using the
//! fake-tailcat bin target (path via CARGO_BIN_EXE, which cargo only exposes
//! to integration tests — hence this file, not unit tests in src/).

use std::time::Duration;
use tailcat_dns_proxy::config::free_high_port;
use tailcat_dns_proxy::tailcat::{spawn_socks, terminate, wait_ready};

// Mirrors go/main_test.go TestSpawnTailcatSocksAndWaitReady.
#[tokio::test]
async fn spawn_and_wait_ready() {
    let port = free_high_port("127.0.0.1");
    let mut child = spawn_socks(&fake_bin(), "127.0.0.1", port)
        .await
        .expect("spawn failed");
    let ready = wait_ready(&format!("127.0.0.1:{port}"), Duration::from_secs(5)).await;
    // Terminate BEFORE asserting so a failed assert can't leak the child
    // (Go's test uses t.Cleanup for the same reason; tokio 1.53 only has
    // kill_on_drop on Command, not on an already-spawned Child).
    terminate(&mut child).await;
    assert!(ready, "fake tailcat never became ready");
}

// Mirrors TestTerminateStopsChild: after terminate() the child is reaped, so
// a follow-up wait() completes immediately.
#[tokio::test]
async fn terminate_stops_child() {
    let port = free_high_port("127.0.0.1");
    let mut child = spawn_socks(&fake_bin(), "127.0.0.1", port)
        .await
        .expect("spawn failed");
    // terminate() always reaps (SIGTERM, then SIGKILL + wait), so no assert
    // below can leak the child.
    terminate(&mut child).await;
    tokio::time::timeout(Duration::from_secs(2), child.wait())
        .await
        .expect("child did not exit after terminate")
        .expect("wait error");
}

// wait_ready_times_out_on_dead_port lives in its own test binary
// (tests/waitready_deadport.rs): cargo runs test binaries sequentially, but
// tests within a binary share a thread pool, and a concurrent spawn test
// rebinding the just-dropped ephemeral port made "dead port" flaky.

fn fake_bin() -> String {
    env!("CARGO_BIN_EXE_fake-tailcat").to_string()
}
