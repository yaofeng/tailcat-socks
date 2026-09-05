use crate::error::DnsFileError;
use arc_swap::ArcSwap;
use std::collections::{HashMap, HashSet};
use std::fs;
use std::path::Path;
use std::sync::Arc;

/// domain (raw bytes, ascii-lowercased) -> token (raw bytes, case preserved).
/// Keys AND values stay raw bytes: Go forwards the ATYP-domain bytes as-is,
/// and a Rust String could not even represent non-UTF-8 domains.
pub type Mapping = HashMap<Vec<u8>, Vec<u8>>;

fn trim_ws(b: &[u8]) -> &[u8] {
    let Some(start) = b.iter().position(|c| !c.is_ascii_whitespace()) else {
        return &[];
    };
    let end = b.iter().rposition(|c| !c.is_ascii_whitespace()).unwrap();
    &b[start..=end]
}

fn parse_dns(data: &[u8]) -> Mapping {
    let mut mapping = Mapping::new();
    for raw in data.split(|&b| b == b'\n') {
        let line = trim_ws(raw);
        if line.is_empty() || line[0] == b'#' {
            continue;
        }
        let mut fields = line
            .split(|&b| b == b' ' || b == b'\t')
            .filter(|f| !f.is_empty());
        let Some(token) = fields.next() else { continue };
        for d in fields {
            mapping.insert(d.to_ascii_lowercase(), token.to_vec());
        }
    }
    mapping
}

/// Parse dns.txt at `path`. Mirrors the Go/Python versions: `#` comments and
/// blank lines skipped, fields split on spaces/tabs, domains stored
/// ascii-lowercased, tokens keep their case, later lines override earlier.
pub fn load_dns_file(path: &Path) -> Result<Mapping, DnsFileError> {
    let data = fs::read(path).map_err(|source| DnsFileError::Read {
        path: path.display().to_string(),
        source,
    })?;
    Ok(parse_dns(&data))
}

/// domain->token mapping, hot-swappable atomically (Go's atomic.Value).
pub struct DnsMap {
    map: ArcSwap<Mapping>,
}

impl Default for DnsMap {
    fn default() -> Self {
        Self::new()
    }
}

impl DnsMap {
    pub fn new() -> Self {
        Self {
            map: ArcSwap::from_pointee(HashMap::new()),
        }
    }

    /// Snapshot of the current mapping (cheap Arc clone, lock-free).
    pub fn load(&self) -> std::sync::Arc<Mapping> {
        self.map.load_full()
    }

    /// Atomically replace the mapping.
    pub fn store(&self, m: Mapping) {
        self.map.store(Arc::new(m));
    }
}

/// Token for host if mapped, else host unchanged. Matching is
/// ascii-case-insensitive; the token keeps its case.
pub fn rewrite_host(host: &[u8], m: &Mapping) -> Vec<u8> {
    m.get(&host.to_ascii_lowercase())
        .cloned()
        .unwrap_or_else(|| host.to_vec())
}

/// Distinct tokens in a mapping (for the startup log line).
pub fn token_set(m: &Mapping) -> HashSet<&Vec<u8>> {
    m.values().collect()
}

/// Test helper: build a Mapping from (domain, token) byte pairs.
#[cfg(test)]
fn mapping_from(entries: &[(&[u8], &[u8])]) -> Mapping {
    entries
        .iter()
        .map(|(d, t)| (d.to_vec(), t.to_vec()))
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    // Mirrors go/dnsmap_test.go TestLoadDNSFileBasic.
    #[test]
    fn parse_basic() {
        let m = parse_dns(
            b"
# comment line
tcXYZabc123   www.example.com  api.example.com
tcDEF456ghi   foo.com

tcGGG         bar.com\tbaz.com
",
        );
        for (d, tok) in [
            (b"www.example.com".as_slice(), "tcXYZabc123"),
            (b"api.example.com".as_slice(), "tcXYZabc123"),
            (b"foo.com".as_slice(), "tcDEF456ghi"),
            (b"bar.com".as_slice(), "tcGGG"),
            (b"baz.com".as_slice(), "tcGGG"),
        ] {
            assert_eq!(
                m.get(d).map(Vec::as_slice),
                Some(tok.as_bytes()),
                "m[{d:?}]"
            );
        }
        assert!(
            !m.contains_key(b"tcxyzabc123".as_slice()),
            "token line must not become a key"
        );
    }

    #[test]
    fn parse_crlf_and_whitespace() {
        // Windows line endings: the trailing CR must not pollute the token.
        let m = parse_dns(b"tcA dup.com\r\ntcB\tsecond.org\r\n");
        assert_eq!(
            m.get(b"dup.com".as_slice()).map(Vec::as_slice),
            Some(b"tcA".as_slice())
        );
        assert_eq!(
            m.get(b"second.org".as_slice()).map(Vec::as_slice),
            Some(b"tcB".as_slice())
        );
    }

    // Mirrors TestLoadDNSFileMissingDomains.
    #[test]
    fn token_only_line_contributes_nothing() {
        let m = parse_dns(b"tcONLYONE\n");
        assert!(m.is_empty(), "{m:?}");
    }

    // Mirrors TestLoadDNSFileLaterOverrides.
    #[test]
    fn later_line_wins() {
        let m = parse_dns(b"tcA dup.com\ntcB dup.com\n");
        assert_eq!(
            m.get(b"dup.com".as_slice()).map(Vec::as_slice),
            Some(b"tcB".as_slice())
        );
    }

    #[test]
    fn load_dns_file_reports_read_errors() {
        let err = load_dns_file(Path::new("/nonexistent/nope.txt")).unwrap_err();
        assert!(err.to_string().contains("cannot load"), "{err}");
    }

    // Mirrors TestRewriteHost.
    #[test]
    fn rewrite_host_case_rules() {
        let m = mapping_from(&[(b"www.example.com".as_slice(), b"tcXYZ")]);
        assert_eq!(rewrite_host(b"www.example.com", &m), b"tcXYZ");
        assert_eq!(rewrite_host(b"WWW.Example.COM", &m), b"tcXYZ");
        assert_eq!(rewrite_host(b"other.com", &m), b"other.com");
    }

    #[test]
    fn rewrite_keeps_raw_bytes_and_token_case() {
        let m = mapping_from(&[(b"www.example.com".as_slice(), b"tcMiXeD123")]);
        assert_eq!(rewrite_host(b"www.EXAMPLE.com", &m), b"tcMiXeD123");
        // raw non-UTF-8 domain passes through untouched
        let raw = [0xff_u8, b'a', 0xfe];
        assert_eq!(rewrite_host(&raw, &m), raw);
    }

    #[test]
    fn token_set_counts_distinct() {
        let m = mapping_from(&[
            (b"a.com".as_slice(), b"tc1"),
            (b"b.com".as_slice(), b"tc1"),
            (b"c.com".as_slice(), b"tc2"),
        ]);
        assert_eq!(token_set(&m).len(), 2);
    }
}
