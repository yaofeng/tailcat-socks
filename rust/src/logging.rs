use std::fmt;

/// Log in the exact format the Go/Python versions use:
/// `[tailcat-dns-proxy] <msg>`, to stderr (bin/start.sh redirects to the log file).
pub fn log_line(msg: impl fmt::Display) {
    eprintln!("[tailcat-dns-proxy] {msg}");
}
