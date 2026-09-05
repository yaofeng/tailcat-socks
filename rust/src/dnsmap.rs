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
/// Cheaply clonable: all handles share one ArcSwap cell.
#[derive(Clone)]
pub struct DnsMap {
    map: std::sync::Arc<ArcSwap<Mapping>>,
}

impl Default for DnsMap {
    fn default() -> Self {
        Self::new()
    }
}

impl DnsMap {
    pub fn new() -> Self {
        Self {
            map: std::sync::Arc::new(ArcSwap::from_pointee(HashMap::new())),
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

/// Poll `path`'s mtime every `interval`; on change reload the mapping into
/// `map`. Mirrors go/dnsmap.go WatchDNSFile, including two subtle details:
/// - Stat BEFORE loading: a write landing between the two leaves last_mod at
///   the older mtime, so the next tick reloads instead of swallowing it.
/// - A failed initial load zeroes last_mod so every tick retries until the
///   file becomes readable.
pub async fn watch(
    path: std::path::PathBuf,
    map: DnsMap,
    interval: std::time::Duration,
    cancel: tokio_util::sync::CancellationToken,
) {
    let mut last_mod: Option<std::time::SystemTime> = None;
    if let Ok(meta) = fs::metadata(&path) {
        last_mod = Some(meta.modified().unwrap_or(std::time::UNIX_EPOCH));
    }
    match load_dns_file(&path) {
        Ok(first) => {
            map.store(first);
        }
        Err(err) => {
            last_mod = None; // retry the initial load on every tick
            crate::logging::log_line(format!("initial load failed: {err}")); // err Display already includes path
        }
    }

    let mut ticker = tokio::time::interval(interval);
    ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    loop {
        tokio::select! {
            _ = cancel.cancelled() => return,
            _ = ticker.tick() => {}
        }
        let Ok(meta) = fs::metadata(&path) else {
            continue;
        };
        let Ok(mtime) = meta.modified() else { continue };
        if last_mod.is_some_and(|lm| lm == mtime) {
            continue;
        }
        match load_dns_file(&path) {
            Err(err) => {
                crate::logging::log_line(format!("reload failed ({err}); keeping previous map"));
            }
            Ok(new_map) => {
                last_mod = Some(mtime);
                let (domains, tokens) = (new_map.len(), token_set(&new_map).len());
                map.store(new_map);
                crate::logging::log_line(format!(
                    "reloaded {}: {domains} domain(s) -> {tokens} token(s)",
                    path.display()
                ));
            }
        }
    }
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

    // Mirrors go/dnsmap_test.go TestWatchDNSFileReloads (50ms tick, mtime
    // bumped explicitly for coarse-grained filesystems).
    #[tokio::test]
    async fn watch_reloads_on_mtime_change() {
        let dir = std::env::temp_dir().join(format!("tcdns-test-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("dns.txt");
        std::fs::write(&path, b"tcA alpha.com\n").unwrap();

        let map = DnsMap::new();
        let cancel = tokio_util::sync::CancellationToken::new();
        let handle = tokio::spawn(watch(
            path.clone(),
            map.clone(),
            std::time::Duration::from_millis(50),
            cancel.clone(),
        ));

        // initial load
        wait_for(|| map.load().contains_key(b"alpha.com".as_slice())).await;

        // rewrite content; sleep first so the new mtime differs even on
        // coarse-grained filesystems (Go's test forces it with os.Chtimes)
        tokio::time::sleep(std::time::Duration::from_millis(1100)).await;
        std::fs::write(&path, b"tcB beta.com\n").unwrap();
        wait_for(|| map.load().contains_key(b"beta.com".as_slice())).await;
        assert!(
            !map.load().contains_key(b"alpha.com".as_slice()),
            "removed domain gone after reload"
        );

        cancel.cancel();
        handle.await.unwrap();
        let _ = std::fs::remove_dir_all(&dir);
    }

    // Mirrors TestWatchDNSFileKeepsPreviousMapOnReadError: reads fail while
    // stat still succeeds (file replaced by a directory) -> old map survives.
    #[tokio::test]
    async fn watch_keeps_previous_map_on_read_error() {
        let dir = std::env::temp_dir().join(format!("tcdns-test-err-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("dns.txt");
        std::fs::write(&path, b"tcA keep.com\n").unwrap();

        let map = DnsMap::new();
        let cancel = tokio_util::sync::CancellationToken::new();
        let handle = tokio::spawn(watch(
            path.clone(),
            map.clone(),
            std::time::Duration::from_millis(50),
            cancel.clone(),
        ));
        wait_for(|| map.load().contains_key(b"keep.com".as_slice())).await;

        std::fs::remove_file(&path).unwrap();
        std::fs::create_dir(&path).unwrap();
        tokio::time::sleep(std::time::Duration::from_millis(300)).await;
        assert_eq!(
            map.load().get(b"keep.com".as_slice()).map(Vec::as_slice),
            Some(b"tcA".as_slice()),
            "read error must keep previous mapping"
        );

        cancel.cancel();
        handle.await.unwrap();
        let _ = std::fs::remove_dir_all(&dir);
    }

    // Spec 测试计划: 初次载入失败(文件尚不存在)时,每个 tick 重试直至成功。
    #[tokio::test]
    async fn watch_retries_failed_initial_load() {
        let dir = std::env::temp_dir().join(format!("tcdns-test-init-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("dns.txt"); // 故意不先创建

        let map = DnsMap::new();
        let cancel = tokio_util::sync::CancellationToken::new();
        let handle = tokio::spawn(watch(
            path.clone(),
            map.clone(),
            std::time::Duration::from_millis(50),
            cancel.clone(),
        ));
        tokio::time::sleep(std::time::Duration::from_millis(150)).await;
        assert!(map.load().is_empty(), "file absent: map must stay empty");

        std::fs::write(&path, b"tcLate late.com\n").unwrap();
        wait_for(|| map.load().contains_key(b"late.com".as_slice())).await;

        cancel.cancel();
        handle.await.unwrap();
        let _ = std::fs::remove_dir_all(&dir);
    }

    async fn wait_for(cond: impl Fn() -> bool) {
        let deadline = tokio::time::Instant::now() + std::time::Duration::from_secs(3);
        while tokio::time::Instant::now() < deadline {
            if cond() {
                return;
            }
            tokio::time::sleep(std::time::Duration::from_millis(20)).await;
        }
        panic!("condition not met within timeout");
    }
}
