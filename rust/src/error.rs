use std::io;

/// `--listen` / `--upstream` could not be parsed (parity with Go's
/// `fmt.Errorf("bad addr %q: %w", s, err)`).
#[derive(Debug, thiserror::Error)]
#[error("bad addr {addr:?}: {source}")]
pub struct ConfigError {
    pub addr: String,
    #[source]
    pub source: std::num::ParseIntError,
}

#[derive(Debug, thiserror::Error)]
pub enum DnsFileError {
    #[error("cannot load {path}: {source}")]
    Read {
        path: String,
        #[source]
        source: io::Error,
    },
}

/// SOCKS5 protocol-level failures. Transport IO errors pass through as
/// `Io` and are silently dropped by the per-connection task, like Go's
/// early `return`s.
#[derive(Debug, thiserror::Error)]
pub enum SocksError {
    #[error("upstream refused the CONNECT")]
    UpstreamRefused,
    #[error("unsupported ATYP {0}")]
    UnsupportedAtyp(u8),
    #[error(transparent)]
    Io(#[from] io::Error),
}
