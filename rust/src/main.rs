//! tailcat-dns-proxy: a SOCKS5 front proxy that rewrites real domain names to
//! tailcat tokens (tc...) and chains to a single standalone `tailcat socks`
//! upstream. Rust implementation of the Python/Go versions; see README.md.
use clap::Parser;
use tailcat_dns_proxy::config::{free_high_port, join_host_port, parse_addr, Config};
use tailcat_dns_proxy::dnsmap::{load_dns_file, token_set, watch, DnsMap};
use tailcat_dns_proxy::logging::log_line;
use tailcat_dns_proxy::proxy::Server;
use tailcat_dns_proxy::tailcat::{spawn_socks, terminate, wait_ready};
use tokio::signal::unix::{signal, SignalKind};
use tokio_util::sync::CancellationToken;

fn main() {
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .expect("tokio runtime");
    let code = runtime.block_on(run(Config::parse()));
    std::process::exit(code);
}

async fn run(cfg: Config) -> i32 {
    // Install the signal handler before doing anything else, so SIGINT/
    // SIGTERM arriving during launch cannot orphan the tailcat child.
    let mut sigterm = signal(SignalKind::terminate()).expect("install SIGTERM handler");
    let mut sigint = signal(SignalKind::interrupt()).expect("install SIGINT handler");

    let first = match load_dns_file(std::path::Path::new(&cfg.dns_file)) {
        Ok(m) => m,
        Err(err) => {
            log_line(format!("{err}")); // DnsFileError Display already says "cannot load <path>: ..."
            return 1;
        }
    };
    // Count before the store consumes `first`; the pre-store below only
    // matters under --no-watch (watch() does its own initial load).
    let counts = (first.len(), token_set(&first).len());
    let dns_map = DnsMap::new();
    dns_map.store(first);

    // Normalize --listen with the Python parse_addr semantics: a bare port
    // or an empty host means loopback, so ":8080" cannot bind every
    // interface.
    let (l_host, l_port) = match parse_addr(&cfg.listen) {
        Ok(v) => v,
        Err(err) => {
            log_line(format!("bad --listen: {err}"));
            return 1;
        }
    };
    let listen = join_host_port(&l_host, l_port);

    // Resolve the tailcat socks listen port: explicit port wins, else high random.
    let (up_host, up_port) = match parse_addr(&cfg.upstream) {
        Ok(v) => v,
        Err(err) => {
            log_line(format!("bad --upstream: {err}"));
            return 1;
        }
    };
    let up_port = if up_port == 0 {
        free_high_port(&up_host)
    } else {
        up_port
    };
    let up_addr = join_host_port(&up_host, up_port);

    let (ln, srv) = match Server::bind(dns_map.clone(), &up_addr, &listen).await {
        Ok(v) => v,
        Err(err) => {
            log_line(format!("cannot listen on {listen}: {err}"));
            return 1;
        }
    };
    // Read the bound address BEFORE the listener is moved into the serve
    // task: with port 0 the OS replaces it, and the Go version logs the
    // actual bound port (ActualAddr) as well.
    let actual = ln
        .local_addr()
        .map(|a| a.to_string())
        .unwrap_or_else(|_| listen.clone());

    let mut child = None;
    if !cfg.no_autolaunch {
        match spawn_socks(&cfg.tailcat_bin, &up_host, up_port).await {
            Ok(c) => child = Some(c),
            Err(err) => {
                log_line(format!("failed to launch {}: {err}", cfg.tailcat_bin));
                return 1;
            }
        }
        if !wait_ready(&up_addr, std::time::Duration::from_secs(15)).await {
            log_line(format!("upstream {up_addr} not ready; aborting"));
            if let Some(c) = child.as_mut() {
                terminate(c).await;
            }
            return 1;
        }
        log_line(format!(
            "auto-launched {} socks on {up_addr}",
            cfg.tailcat_bin
        ));
    }

    let stop = CancellationToken::new();
    if !cfg.no_watch {
        let dns_path = std::path::PathBuf::from(&cfg.dns_file);
        tokio::spawn(watch(
            dns_path,
            dns_map.clone(),
            std::time::Duration::from_secs(1),
            stop.clone(),
        ));
    }

    // `mut` so the select branch can await it by reference and the handle
    // stays owned for abort() after the select.
    let mut serve = tokio::spawn(async move { srv.serve(ln).await });

    log_line(format!("listening socks5h://{actual}"));
    log_line(format!(
        "{} domain(s) mapped -> {} token(s)",
        counts.0, counts.1
    ));
    log_line(format!("upstream {up_addr}"));

    tokio::select! {
        _ = sigterm.recv() => {}
        _ = sigint.recv() => {}
        err = async { (&mut serve).await.expect("serve task panicked") } => {
            // Nothing ever closes the listener on the shutdown path, so this
            // branch only fires on a genuine early serve failure; signal
            // exits go through abort() below and stay quiet by construction,
            // like the Go version's net.ErrClosed suppression.
            if let Err(e) = err {
                log_line(format!("server error: {e}"));
            }
        }
    }
    // Abort the serve task before reaping the child (Go's srv.Close() at
    // go/main.go:118): it owns `ln`, so aborting drops the listener and new
    // clients are refused during the terminate window instead of completing
    // handshakes against a dying upstream. No-op if serve already finished.
    serve.abort();
    stop.cancel();
    if let Some(c) = child.as_mut() {
        terminate(c).await;
    }
    0
}
