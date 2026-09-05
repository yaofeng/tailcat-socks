//! wait_ready timeout guard. Lives in its own test binary on purpose:
//! cargo runs test binaries sequentially, but tests within one binary run
//! concurrently, and a sibling test rebinding the just-dropped ephemeral
//! port made the "dead port" assumption flaky (observed ~1 in 5 runs).

use std::time::Duration;
use tailcat_dns_proxy::tailcat::wait_ready;

// wait_ready must keep polling a port that refuses connections and report
// false at the deadline (guards the timeout() double-Result: the OUTER Ok is
// the timer, the INNER one is the connect).
#[tokio::test]
async fn wait_ready_times_out_on_dead_port() {
    let ln = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let dead = ln.local_addr().unwrap().to_string();
    drop(ln); // nothing listens there anymore
    let started = std::time::Instant::now();
    assert!(
        !wait_ready(&dead, Duration::from_millis(400)).await,
        "wait_ready reported ready for a dead port"
    );
    assert!(
        started.elapsed() >= Duration::from_millis(350),
        "wait_ready returned too fast ({:?}); refused connects must not count as ready",
        started.elapsed()
    );
}
