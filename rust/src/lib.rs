//! tailcat-dns-proxy: a SOCKS5 front proxy that rewrites real domain names to
//! tailcat tokens (tc...) and chains to a single standalone `tailcat socks`
//! upstream. Rust implementation; see README.md for usage.

pub mod config;
pub mod dnsmap;
pub mod error;
pub mod logging;
