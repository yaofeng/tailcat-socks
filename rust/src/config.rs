use crate::error::ConfigError;
use clap::Parser;

/// CLI flags, identical names/defaults to the Go/Python versions.
#[derive(Parser, Debug)]
#[command(
    name = "tailcat-dns-proxy",
    about = "SOCKS5 front proxy that rewrites domains to tailcat tokens"
)]
pub struct Config {
    /// SOCKS5 listen addr:port
    #[arg(long, default_value = "127.0.0.1:1080")]
    pub listen: String,

    /// domain->token mapping file
    #[arg(long, default_value = "dns.txt")]
    pub dns_file: String,

    /// upstream tailcat socks addr:port; port 0 (default) = high random free port
    #[arg(long, default_value = "127.0.0.1:0")]
    pub upstream: String,

    /// path to tailcat binary (auto-launched)
    #[arg(long, default_value = "tailcat")]
    pub tailcat_bin: String,

    /// do not spawn tailcat socks; use an already-running upstream
    #[arg(long)]
    pub no_autolaunch: bool,

    /// disable dns.txt hot-reload
    #[arg(long)]
    pub no_watch: bool,
}

/// Split "host:port" with Python's rpartition(":") semantics: everything
/// after the LAST colon is the port, everything before it the host
/// (empty -> 127.0.0.1); a string with no colon is treated as the port part.
/// Brackets on IPv6 are preserved here; join_host_port re-normalizes them.
pub fn parse_addr(s: &str) -> Result<(String, u16), ConfigError> {
    let (host, port_str) = match s.rfind(':') {
        Some(i) => (&s[..i], &s[i + 1..]),
        None => ("", s),
    };
    let port: u16 = if port_str.is_empty() {
        0
    } else {
        port_str.parse().map_err(|source| ConfigError {
            addr: s.to_string(),
            source,
        })?
    };
    let host = if host.is_empty() { "127.0.0.1" } else { host };
    Ok((host.to_string(), port))
}

/// net.JoinHostPort equivalent: strips stale brackets, re-brackets bare IPv6.
pub fn join_host_port(host: &str, port: u16) -> String {
    let h = host.trim_start_matches('[').trim_end_matches(']');
    if h.contains(':') {
        format!("[{h}]:{port}")
    } else {
        format!("{h}:{port}")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // Case table mirrors go/main_test.go TestParseAddr.
    #[test]
    fn parse_addr_matches_go_semantics() {
        let cases: &[(&str, &str, u16)] = &[
            ("127.0.0.1:1080", "127.0.0.1", 1080),
            (":8080", "127.0.0.1", 8080), // empty host -> loopback
            ("127.0.0.1:0", "127.0.0.1", 0),
            ("example.com:9999", "example.com", 9999),
            ("[::1]:1080", "[::1]", 1080), // brackets preserved, caller re-joins
            ("1080", "127.0.0.1", 1080),   // bare port, empty host -> default
            ("host:", "host", 0),          // empty port -> 0
        ];
        for (input, want_host, want_port) in cases {
            let (host, port) =
                parse_addr(input).unwrap_or_else(|e| panic!("parse_addr({input}): {e}"));
            assert_eq!(host, *want_host, "parse_addr({input}) host");
            assert_eq!(port, *want_port, "parse_addr({input}) port");
        }
    }

    #[test]
    fn parse_addr_rejects_bad_ports() {
        // bare IP: the whole string is the port part and fails to parse
        // (parity with Python/Go); likewise a non-numeric port.
        for input in ["127.0.0.1", "host:abc"] {
            assert!(
                parse_addr(input).is_err(),
                "parse_addr({input}) should fail"
            );
        }
    }

    #[test]
    fn join_host_port_brackets_ipv6() {
        assert_eq!(join_host_port("[::1]", 1080), "[::1]:1080");
        assert_eq!(join_host_port("::1", 1080), "[::1]:1080");
        assert_eq!(join_host_port("127.0.0.1", 1080), "127.0.0.1:1080");
        assert_eq!(join_host_port("localhost", 0), "localhost:0");
    }
}
