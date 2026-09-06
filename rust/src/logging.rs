use std::fmt;
use std::io::Write;

/// Log in the exact format the Go/Python versions use:
/// `[tailcat-socks] <msg>`, to stderr (bin/start.sh redirects to the log file).
/// Ignore write errors like Go's log.Printf: a full/closed stderr must not
/// panic between child launch and shutdown (would skip terminate() and
/// orphan tailcat).
pub fn log_line(msg: impl fmt::Display) {
    let _ = writeln!(std::io::stderr(), "[tailcat-socks] {msg}");
}
