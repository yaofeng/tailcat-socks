# Rust (tokio) 复刻版实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 `rust/` 下实现与 `python/`、`go/` 行为一致的 SOCKS5 前置代理（域名→tailcat token 改写），tokio 生态、生产可用，并接入 `bin/` 脚本与 README。

**Architecture:** cargo 包 = 薄 `main.rs` + `tailcat_socks` 库（config/dnsmap/proxy/tailcat/error/logging 六模块）。每连接一个 tokio task；dns 映射 `ArcSwap<HashMap<Vec<u8>, Vec<u8>>>` 原子热替换；relay 用 `select!` 双向泵 + `AtomicU64` 共享空闲钟 + `shutdown(Write)` 半关防 RST。规格：`docs/superpowers/specs/2026-09-05-rust-reimplementation-design.md`。

**Tech Stack:** Rust 2021 edition、tokio(net/process/signal/time/macros/io-util/rt-multi-thread)、clap(derive)、arc-swap、tokio-util(CancellationToken)、thiserror、libc(unix, SIGTERM)。

**对规格的四处微调**（规划期锁定，随本计划一并更新规格文档）：

1. 映射类型用 `HashMap<Vec<u8>, Vec<u8>>`（value 也是原始字节），避免 token 经 `from_utf8_lossy` 有损转换后上线（Go 是原样字节转发）。
2. 新增 `src/lib.rs`（库目标）、`src/logging.rs`（`[tailcat-socks]` 前缀日志函数）与 `src/tailcat.rs`（子进程拉起/wait_ready/terminate，规格里此职责内含于 main 装配节，独立成模块便于测试）——集成测试经库目标访问内部函数（Rust 惯用法），日志函数被多模块共用。
3. `anyhow` 从依赖中移除：main 的错误路径都是「打固定格式日志 + 退出码 1」，用不上错误冒泡。
4. `free_high_port` 的 port-0 兜底若也失败则 panic（实践上不可达；不值得为此串 Result，接受与 Go `log.Fatalf` 的退出码差异）。

**行为对照基准：** `go/main.go`、`go/dnsmap.go`、`go/proxy.go` 及其三份测试。测试覆盖面逐条对齐 Go 测试。

---

### Task 1: 安装 Rust 工具链（幂等）

**Files:** 无（用户级 `~/.cargo`，不动系统包）

- [ ] **Step 1: 检查是否已有 cargo**

```bash
source "$HOME/.cargo/env" 2>/dev/null; cargo --version
```

Expected: 已装则打印 `cargo 1.8x.x`，直接跳到 Task 2；否则继续 Step 2。

- [ ] **Step 2: rustup 官方脚本安装（minimal profile）**

```bash
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --default-toolchain stable --profile minimal
source "$HOME/.cargo/env" && cargo --version && rustc --version
```

Expected: `cargo 1.8x.x (...)`、`rustc 1.8x.x`。

> 后续任何 `cargo` 命令报「command not found」，先执行 `source "$HOME/.cargo/env"`。

---

### Task 2: 脚手架（cargo 包、error、logging、.gitignore）

**Files:**
- Create: `rust/Cargo.toml`
- Create: `rust/src/lib.rs`
- Create: `rust/src/main.rs`（占位，Task 11 重写）
- Create: `rust/src/error.rs`
- Create: `rust/src/logging.rs`
- Modify: `.gitignore`

- [ ] **Step 1: 创建 Cargo.toml**

```toml
[package]
name = "tailcat-socks"
version = "0.1.0"
edition = "2021"
description = "SOCKS5 front proxy that rewrites domains to tailcat tokens (Rust implementation)"

[dependencies]
tokio = { version = "1", features = ["rt-multi-thread", "net", "process", "signal", "time", "macros", "io-util"] }
clap = { version = "4", features = ["derive"] }
arc-swap = "1"
tokio-util = "0.7"        # CancellationToken
thiserror = "2"

[target.'cfg(unix)'.dependencies]
libc = "0.2"              # SIGTERM to the tailcat child (tokio only offers SIGKILL)
```

说明：`src/lib.rs` 自动成为库目标 `tailcat_socks`；`src/main.rs` 自动成为 bin `tailcat-socks`；`src/bin/fake-tailcat.rs`（Task 10）自动成为第二个 bin。集成测试可用 `env!("CARGO_BIN_EXE_fake-tailcat")` 拿其二进制路径。

- [ ] **Step 2: 创建 src/lib.rs（当前仅两模块，后续任务逐步追加）**

```rust
//! tailcat-socks: a SOCKS5 front proxy that rewrites real domain names to
//! tailcat tokens (tc...) and chains to a single standalone `tailcat socks`
//! upstream. Rust implementation; see README.md for usage.

pub mod error;
pub mod logging;
```

- [ ] **Step 3: 创建 src/main.rs（占位，Task 11 重写）**

```rust
//! Placeholder entry point; replaced by the full assembly in Task 11.
fn main() {}
```

- [ ] **Step 4: 创建 src/logging.rs**

```rust
use std::fmt;

/// Log in the exact format the Go/Python versions use:
/// `[tailcat-socks] <msg>`, to stderr (bin/start.sh redirects to the log file).
pub fn log_line(msg: impl fmt::Display) {
    eprintln!("[tailcat-socks] {msg}");
}
```

- [ ] **Step 5: 创建 src/error.rs**

```rust
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
```

- [ ] **Step 6: 根 .gitignore 追加一行**

在 `.gitignore` 末尾追加：

```
rust/target/
```

- [ ] **Step 7: 首次构建验证（拉取依赖并编译）**

```bash
cd rust && source "$HOME/.cargo/env" && cargo build 2>&1 | tail -3
```

Expected: `Finished dev [unoptimized + debuginfo] target(s)`，无 error。

- [ ] **Step 8: Commit**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
git add rust/ .gitignore
git commit -m "Rust: scaffolding (cargo pkg, error/logging modules, lib+bin targets)"
```

---

### Task 3: config.rs —— parse_addr / join_host_port（TDD）

**Files:**
- Create: `rust/src/config.rs`
- Modify: `rust/src/lib.rs`（加 `pub mod config;`）

- [ ] **Step 1: 先写测试（含 clap 的 Config 结构体），lib.rs 挂上模块**

`rust/src/lib.rs` 整体替换为：

```rust
//! tailcat-socks: a SOCKS5 front proxy that rewrites real domain names to
//! tailcat tokens (tc...) and chains to a single standalone `tailcat socks`
//! upstream. Rust implementation; see README.md for usage.

pub mod config;
pub mod error;
pub mod logging;
```

创建 `rust/src/config.rs`（此时只有 Config 结构体与测试，没有被测函数）：

```rust
use crate::error::ConfigError;
use clap::Parser;

/// CLI flags, identical names/defaults to the Go/Python versions.
#[derive(Parser, Debug)]
#[command(name = "tailcat-socks", about = "SOCKS5 front proxy that rewrites domains to tailcat tokens")]
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

#[cfg(test)]
mod tests {
    use super::*;

    // Case table mirrors go/main_test.go TestParseAddr.
    #[test]
    fn parse_addr_matches_go_semantics() {
        let cases: &[(&str, &str, u16)] = &[
            ("127.0.0.1:1080", "127.0.0.1", 1080),
            (":8080", "127.0.0.1", 8080),      // empty host -> loopback
            ("127.0.0.1:0", "127.0.0.1", 0),
            ("example.com:9999", "example.com", 9999),
            ("[::1]:1080", "[::1]", 1080),     // brackets preserved, caller re-joins
            ("1080", "127.0.0.1", 1080),       // bare port, empty host -> default
            ("host:", "host", 0),              // empty port -> 0
        ];
        for (input, want_host, want_port) in cases {
            let (host, port) = parse_addr(input).unwrap_or_else(|e| panic!("parse_addr({input}): {e}"));
            assert_eq!(host, *want_host, "parse_addr({input}) host");
            assert_eq!(port, *want_port, "parse_addr({input}) port");
        }
    }

    #[test]
    fn parse_addr_rejects_bad_ports() {
        // bare IP: the whole string is the port part and fails to parse
        // (parity with Python/Go); likewise a non-numeric port.
        for input in ["127.0.0.1", "host:abc"] {
            assert!(parse_addr(input).is_err(), "parse_addr({input}) should fail");
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
```

- [ ] **Step 2: 运行测试确认红**

```bash
cd rust && cargo test config:: 2>&1 | tail -5
```

Expected: 编译失败，`cannot find function 'parse_addr' in this scope`（以及 `join_host_port`）。

- [ ] **Step 3: 实现 parse_addr / join_host_port**

在 `config.rs` 的 Config 结构体之后、tests 模块之前插入：

```rust
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
        port_str
            .parse()
            .map_err(|source| ConfigError { addr: s.to_string(), source })?
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
```

- [ ] **Step 4: 运行测试确认绿**

```bash
cd rust && cargo test config:: 2>&1 | tail -5
```

Expected: `test result: ok. 3 passed`。

- [ ] **Step 5: Commit**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
git add rust/src/config.rs rust/src/lib.rs
git commit -m "Rust: config (clap flags, parse_addr/join_host_port with Python rpartition semantics)"
```

---

### Task 4: config.rs —— free_high_port（随机高位端口，TDD）

**Files:**
- Modify: `rust/src/config.rs`（追加函数与测试）

- [ ] **Step 1: 追加失败测试（tests 模块内）**

```rust
    #[test]
    fn free_high_port_returns_bindable_high_port() {
        let p = free_high_port("127.0.0.1");
        assert!((20000..=60999).contains(&p), "port {p} out of high range");
        // typically still bindable right after (same assertion as Go)
        if let Ok(ln) = std::net::TcpListener::bind(join_host_port("127.0.0.1", p)) {
            drop(ln);
        }
    }
```

- [ ] **Step 2: 运行确认红**

```bash
cd rust && cargo test config:: 2>&1 | tail -5
```

Expected: 编译失败，`cannot find function 'free_high_port'`。

- [ ] **Step 3: 实现**

文件顶部 use 区追加：

```rust
use std::io::Read;
use std::net::TcpListener;
```

在 `join_host_port` 之后追加：

```rust
/// 64 bits from /dev/urandom, falling back to wall-clock nanos (no rand dep).
fn random_u64() -> u64 {
    if let Ok(mut f) = std::fs::File::open("/dev/urandom") {
        let mut b = [0u8; 8];
        if f.read_exact(&mut b).is_ok() {
            return u64::from_le_bytes(b);
        }
    }
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos() as u64)
        .unwrap_or(0x9E37_79B9_7F4A_7C15)
}

/// Probe a free high port (20000..=60999) at random, like the Go/Python
/// versions; after 20 failed tries fall back to an OS-assigned port. The
/// port-0 fallback failing is practically unreachable (TCP stack broken);
/// panic there, mirroring Go's log.Fatalf.
pub fn free_high_port(host: &str) -> u16 {
    for _ in 0..20 {
        let p = 20000 + (random_u64() % 41000) as u16;
        if TcpListener::bind(join_host_port(host, p)).is_ok() {
            return p;
        }
    }
    let ln = TcpListener::bind(join_host_port(host, 0)).expect("cannot find a free port");
    ln.local_addr().expect("local_addr").port()
}
```

- [ ] **Step 4: 运行确认绿**

```bash
cd rust && cargo test config:: 2>&1 | tail -5
```

Expected: `test result: ok. 4 passed`。

- [ ] **Step 5: Commit**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
git add rust/src/config.rs
git commit -m "Rust: free_high_port (urandom-backed high-port probe, port-0 fallback)"
```

---

### Task 5: dnsmap.rs —— 解析 / DnsMap / rewrite_host（TDD）

**Files:**
- Create: `rust/src/dnsmap.rs`
- Modify: `rust/src/lib.rs`（加 `pub mod dnsmap;`）

- [ ] **Step 1: lib.rs 挂模块；先写测试**

`rust/src/lib.rs` 的模块区追加一行：

```rust
pub mod dnsmap;
```

创建 `rust/src/dnsmap.rs`（先只有类型与测试；注意 value 也是原始字节，见计划头部微调 1）：

```rust
use crate::error::DnsFileError;
use arc_swap::ArcSwap;
use std::collections::{HashMap, HashSet};
use std::fs;
use std::path::Path;

/// domain (raw bytes, ascii-lowercased) -> token (raw bytes, case preserved).
/// Keys AND values stay raw bytes: Go forwards the ATYP-domain bytes as-is,
/// and a Rust String could not even represent non-UTF-8 domains.
pub type Mapping = HashMap<Vec<u8>, Vec<u8>>;

#[cfg(test)]
mod tests {
    use super::*;

    // Mirrors go/dnsmap_test.go TestLoadDNSFileBasic.
    #[test]
    fn parse_basic() {
        let m = parse_dns(b"
# comment line
tcXYZabc123   www.example.com  api.example.com
tcDEF456ghi   foo.com

tcGGG         bar.com\tbaz.com
");
        for (d, tok) in [
            (b"www.example.com".as_slice(), "tcXYZabc123"),
            (b"api.example.com".as_slice(), "tcXYZabc123"),
            (b"foo.com".as_slice(), "tcDEF456ghi"),
            (b"bar.com".as_slice(), "tcGGG"),
            (b"baz.com".as_slice(), "tcGGG"),
        ] {
            assert_eq!(m.get(d).map(Vec::as_slice), Some(tok.as_bytes()), "m[{d:?}]");
        }
        assert!(!m.contains_key(b"tcxyzabc123".as_slice()), "token line must not become a key");
    }

    #[test]
    fn parse_crlf_and_whitespace() {
        // Windows line endings: the trailing CR must not pollute the token.
        let m = parse_dns(b"tcA dup.com\r\ntcB\tsecond.org\r\n");
        assert_eq!(m.get(b"dup.com".as_slice()).map(Vec::as_slice), Some(b"tcA".as_slice()));
        assert_eq!(m.get(b"second.org".as_slice()).map(Vec::as_slice), Some(b"tcB".as_slice()));
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
        assert_eq!(m.get(b"dup.com".as_slice()).map(Vec::as_slice), Some(b"tcB".as_slice()));
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
        let raw = [0xff, b'a', 0xfe];
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
```

注意：`mapping_from`/被测函数还没写，这就是下一步的红。

- [ ] **Step 2: 运行确认红**

```bash
cd rust && cargo test dnsmap:: 2>&1 | tail -5
```

Expected: 编译失败，找不到 `parse_dns` / `load_dns_file` / `mapping_from` / `rewrite_host` / `token_set`。

- [ ] **Step 3: 实现**

在 `dnsmap.rs` 的 Mapping 类型之后、tests 之前插入全部实现：

```rust
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
        Self { map: ArcSwap::from_pointee(HashMap::new()) }
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
    entries.iter().map(|(d, t)| (d.to_vec(), t.to_vec())).collect()
}
```

- [ ] **Step 4: 运行确认绿**

```bash
cd rust && cargo test dnsmap:: 2>&1 | tail -5
```

Expected: `test result: ok. 8 passed`。

- [ ] **Step 5: Commit**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
git add rust/src/dnsmap.rs rust/src/lib.rs
git commit -m "Rust: dnsmap (raw-byte parse, ArcSwap map, rewrite_host)"
```

---

### Task 6: dnsmap.rs —— watch 热加载（TDD）

**Files:**
- Modify: `rust/src/dnsmap.rs`（追加 watch 与测试）

- [ ] **Step 1: 追加失败测试（tests 模块内；tokio 测试宏需要 macros feature，Cargo.toml 已含）**

```rust
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
        assert!(!map.load().contains_key(b"alpha.com".as_slice()), "removed domain gone after reload");

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
```

（上面测试用了 `map.clone()` 给 watch 任务——DnsMap 为 Arc 内部句柄风格，Step 3 中 `#[derive(Clone)]`。测试代码保持原样即可。）

- [ ] **Step 2: 运行确认红**

```bash
cd rust && cargo test dnsmap::watch 2>&1 | tail -5
```

Expected: 编译失败，找不到 `watch`。

- [ ] **Step 3: 实现 watch；DnsMap 改为可 Clone**

把 Task 5 中 DnsMap 的定义替换为（内部包一层 `Arc`，使句柄可 clone）：

```rust
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
        Self { map: std::sync::Arc::new(ArcSwap::from_pointee(HashMap::new())) }
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
```

并在文件末尾追加：

```rust
/// Poll `path`'s mtime every `interval`; on change reload the mapping into
/// `map`. Mirrors go/dnsmap.go WatchDNSFile, including two subtle details:
/// - Stat BEFORE loading: a write landing between the two leaves last_mod at
///   the older mtime, so the next tick reloads instead of swallowing it.
/// - A failed initial load zeroes last_mod so every tick retries until the
///   file becomes readable.
pub async fn watch(path: std::path::PathBuf, map: DnsMap, interval: std::time::Duration, cancel: tokio_util::sync::CancellationToken) {
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
        let Ok(meta) = fs::metadata(&path) else { continue };
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
```

同时文件顶部 use 区补充：

```rust
use std::collections::HashSet; // 若 Task 5 已引入则跳过
```

- [ ] **Step 4: 运行确认绿**

```bash
cd rust && cargo test dnsmap:: 2>&1 | tail -5
```

Expected: `test result: ok. 10 passed`（Task 5 的 7 个 + watch 的 3 个）。

- [ ] **Step 5: Commit**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
git add rust/src/dnsmap.rs
git commit -m "Rust: dnsmap watch (mtime hot-reload, initial-load retry, clonable DnsMap)"
```

---


---
### Task 7: proxy.rs —— SOCKS5 服务端全链路（含 relay）（TDD）

**Files:**
- Create: `rust/src/proxy.rs`
- Modify: `rust/src/lib.rs`（加 `pub mod proxy;`）

- [ ] **Step 1: lib.rs 挂模块；创建 proxy.rs：常量/Server 骨架/测试先行（serve_conn 留 unimplemented!）**

`rust/src/lib.rs` 模块区追加：

```rust
pub mod proxy;
```

创建 `rust/src/proxy.rs`（Step 3 才填被测实现；tests 模块本步就完整写好）：

```rust
use crate::dnsmap::{rewrite_host, DnsMap};
use crate::error::SocksError;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

/// Bound on the upstream dial + SOCKS5 handshake, like the Python version's
/// create_connection(timeout=15) / Go's SetDeadline.
pub const UPSTREAM_DIAL_TIMEOUT: Duration = Duration::from_secs(15);
/// Mirror of the Python/Go versions: the relay breaks after 300s with no
/// data in either direction.
pub const RELAY_IDLE_TIMEOUT: Duration = Duration::from_secs(300);

/// Byte-identical success/failure replies (BOUND.ADDR 0.0.0.0:0, as in Go).
const SOCKS_OK_REPLY: [u8; 10] = [0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0];
const SOCKS_FAIL_REPLY: [u8; 10] = [0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0];

/// Client-facing SOCKS5 front proxy. On each CONNECT it rewrites the target
/// host via DnsMap and forwards the request (as a SOCKS5 client) to the
/// single `tailcat socks` upstream. Cheaply clonable: every connection task
/// holds a clone, all sharing the same DnsMap.
#[derive(Clone)]
pub struct Server {
    pub dns_map: DnsMap,
    pub upstream_addr: String,
    /// Relay shared-idle window; tests shrink it.
    pub relay_idle: Duration,
}

impl Server {
    /// Bind `listen`. Drive the accept loop with `serve(ln)`.
    pub async fn bind(
        dns_map: DnsMap,
        upstream_addr: &str,
        listen: &str,
    ) -> std::io::Result<(TcpListener, Self)> {
        let ln = TcpListener::bind(listen).await?;
        let srv = Self {
            dns_map,
            upstream_addr: upstream_addr.to_string(),
            relay_idle: RELAY_IDLE_TIMEOUT,
        };
        Ok((ln, srv))
    }

    /// Accept loop: one spawned task per connection. Returns when the
    /// listener errors (the caller drops it on shutdown).
    pub async fn serve(&self, ln: TcpListener) -> std::io::Result<()> {
        loop {
            let (conn, _peer) = ln.accept().await?;
            let me = self.clone();
            tokio::spawn(async move { me.handle(conn).await });
        }
    }

    async fn handle(&self, conn: TcpStream) {
        // Per-connection protocol noise is silently dropped, like Go's
        // early returns.
        let _ = self.serve_conn(conn).await;
    }

    async fn serve_conn(&self, _client: TcpStream) -> Result<(), SocksError> {
        unimplemented!("Task 7 Step 3")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Minimal SOCKS5 upstream (stand-in for `tailcat socks`): accepts any
    /// CONNECT, records the target on the channel, echoes data back
    /// "ECHO:"-prefixed. Mirrors go/proxy_test.go fakeUpstream.
    async fn fake_upstream(
        received: &tokio::sync::mpsc::UnboundedSender<String>,
    ) -> std::io::Result<String> {
        let ln = TcpListener::bind("127.0.0.1:0").await?;
        let addr = ln.local_addr()?.to_string();
        tokio::spawn(async move {
            loop {
                let Ok((mut conn, _)) = ln.accept().await else { return };
                tokio::spawn(async move {
                    let mut hdr = [0u8; 2];
                    if conn.read_exact(&mut hdr).await.is_err() {
                        return;
                    }
                    let mut methods = vec![0u8; hdr[1] as usize];
                    if conn.read_exact(&mut methods).await.is_err() {
                        return;
                    }
                    if conn.write_all(&[0x05, 0x00]).await.is_err() {
                        return;
                    }
                    let mut req = [0u8; 4];
                    if conn.read_exact(&mut req).await.is_err() {
                        return;
                    }
                    let host: Vec<u8> = match req[3] {
                        0x03 => {
                            let mut l = [0u8; 1];
                            if conn.read_exact(&mut l).await.is_err() {
                                return;
                            }
                            let mut b = vec![0u8; l[0] as usize];
                            if conn.read_exact(&mut b).await.is_err() {
                                return;
                            }
                            b
                        }
                        0x01 => {
                            let mut b = [0u8; 4];
                            if conn.read_exact(&mut b).await.is_err() {
                                return;
                            }
                            std::net::Ipv4Addr::new(b[0], b[1], b[2], b[3])
                                .to_string()
                                .into_bytes()
                        }
                        _ => b"?".to_vec(),
                    };
                    let mut pb = [0u8; 2];
                    if conn.read_exact(&mut pb).await.is_err() {
                        return;
                    }
                    let port = u16::from_be_bytes(pb);
                    let _ = received.send(format!("{}:{}", String::from_utf8_lossy(&host), port));
                    if conn.write_all(&SOCKS_OK_REPLY).await.is_err() {
                        return;
                    }
                    let mut buf = [0u8; 4096];
                    loop {
                        match conn.read(&mut buf).await {
                            Ok(0) | Err(_) => return,
                            Ok(n) => {
                                let mut out = b"ECHO:".to_vec();
                                out.extend_from_slice(&buf[..n]);
                                if conn.write_all(&out).await.is_err() {
                                    return;
                                }
                            }
                        }
                    }
                });
            }
        });
        Ok(addr)
    }

    /// Dial the proxy and complete greeting + CONNECT (domain ATYP).
    async fn socks_connect(proxy_addr: &str, host: &[u8], port: u16) -> TcpStream {
        let mut c = tokio::time::timeout(Duration::from_secs(2), TcpStream::connect(proxy_addr))
            .await
            .expect("dial proxy timed out")
            .expect("dial proxy failed");
        c.write_all(&[0x05, 0x01, 0x00]).await.unwrap();
        let mut rep = [0u8; 2];
        c.read_exact(&mut rep).await.unwrap();
        assert_eq!(&rep, &[0x05, 0x00], "greeting refused: {rep:?}");
        let mut req = vec![0x05, 0x01, 0x00, 0x03, host.len() as u8];
        req.extend_from_slice(host);
        req.extend_from_slice(&port.to_be_bytes());
        c.write_all(&req).await.unwrap();
        let mut head = [0u8; 10];
        c.read_exact(&mut head).await.unwrap();
        assert_eq!(head[1], 0x00, "CONNECT failed: {head:?}");
        c
    }

    /// Build a Server from (domain, token) pairs and run its accept loop.
    /// Returns the bound address, the server, and the serve task handle.
    async fn start_proxy(
        mapping: &[(&[u8], &[u8])],
        upstream: &str,
    ) -> (
        String,
        Server,
        tokio::task::JoinHandle<std::io::Result<()>>,
    ) {
        let m = mapping
            .iter()
            .map(|(d, t)| (d.to_ascii_lowercase(), t.to_vec()))
            .collect::<HashMap<Vec<u8>, Vec<u8>>>();
        let dns_map = DnsMap::new();
        dns_map.store(m);
        let (ln, srv) = Server::bind(dns_map, upstream, "127.0.0.1:0")
            .await
            .unwrap();
        let addr = ln.local_addr().unwrap().to_string();
        let handle = tokio::spawn({
            let srv = srv.clone();
            async move { srv.serve(ln).await }
        });
        (addr, srv, handle)
    }

    async fn recv_target(rx: &mut tokio::sync::mpsc::UnboundedReceiver<String>) -> String {
        tokio::time::timeout(Duration::from_secs(2), rx.recv())
            .await
            .expect("upstream never received the CONNECT")
            .expect("sender dropped")
    }

    // Mirrors go/proxy_test.go TestSocks5ProxyRewritesHostAndRelays.
    #[tokio::test]
    async fn rewrites_host_and_relays() {
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        let up = fake_upstream(&tx).await.unwrap();
        let (addr, _srv, handle) =
            start_proxy(&[(b"www.example.com", b"tcXYZabc123")], &up).await;

        let mut c = socks_connect(&addr, b"www.example.com", 8081).await;
        c.write_all(b"hello").await.unwrap();
        let mut buf = vec![0u8; b"ECHO:hello".len()];
        tokio::time::timeout(Duration::from_secs(2), c.read_exact(&mut buf))
            .await
            .expect("relay read timed out")
            .unwrap();
        assert_eq!(buf, b"ECHO:hello");
        drop(c);
        handle.abort();

        assert_eq!(recv_target(&mut rx).await, "tcXYZabc123:8081");
    }

    // Mirrors TestSocks5ProxyRewritesCaseInsensitive.
    #[tokio::test]
    async fn rewrites_case_insensitive() {
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        let up = fake_upstream(&tx).await.unwrap();
        let (addr, _srv, handle) =
            start_proxy(&[(b"www.example.com", b"tcXYZabc123")], &up).await;
        let _c = socks_connect(&addr, b"WWW.Example.COM", 80).await;
        drop(_c);
        handle.abort();
        assert_eq!(recv_target(&mut rx).await, "tcXYZabc123:80");
    }

    // Mirrors TestSocks5ProxyPassesUnmatchedHostThrough.
    #[tokio::test]
    async fn passes_unmatched_host_through() {
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        let up = fake_upstream(&tx).await.unwrap();
        let (addr, _srv, handle) =
            start_proxy(&[(b"www.example.com", b"tcXYZabc123")], &up).await;
        let _c = socks_connect(&addr, b"other.com", 9999).await;
        drop(_c);
        handle.abort();
        assert_eq!(recv_target(&mut rx).await, "other.com:9999");
    }

    // Mirrors TestSocks5ProxyRejectsBind.
    #[tokio::test]
    async fn rejects_bind() {
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        let up = fake_upstream(&tx).await.unwrap();
        let (addr, _srv, handle) = start_proxy(&[], &up).await;

        let mut c = TcpStream::connect(&addr).await.unwrap();
        c.write_all(&[0x05, 0x01, 0x00]).await.unwrap();
        let mut rep = [0u8; 2];
        c.read_exact(&mut rep).await.unwrap();
        // CMD=2 (BIND) with an IPv4 ATYP
        c.write_all(&[0x05, 0x02, 0x00, 0x01, 127, 0, 0, 1, 0x1F, 0x90])
            .await
            .unwrap();
        let mut head = [0u8; 10];
        c.read_exact(&mut head).await.unwrap();
        assert_eq!(head[1], 0x01, "BIND must fail with 0x01, got {head:?}");
        handle.abort();
        assert!(rx.try_recv().is_err(), "upstream must not receive anything");
    }

    // Mirrors TestSocks5ProxyNoAcceptableMethod.
    #[tokio::test]
    async fn no_acceptable_method() {
        let (addr, _srv, handle) = start_proxy(&[], "127.0.0.1:1").await;
        let mut c = TcpStream::connect(&addr).await.unwrap();
        c.write_all(&[0x05, 0x01, 0xFF]).await.unwrap(); // only user/pass auth
        let mut rep = [0u8; 2];
        c.read_exact(&mut rep).await.unwrap();
        assert_eq!(&rep, &[0x05, 0xFF], "want 05 FF, got {rep:?}");
        handle.abort();
    }
}
```

tests 模块头部还需一行（`start_proxy` 用到）：

```rust
    use std::collections::HashMap;
```

- [ ] **Step 2: 运行确认红**

```bash
cd rust && cargo test proxy:: 2>&1 | tail -8
```

Expected: 5 个测试全部失败，panic 消息含 `not yet implemented: Task 7 Step 3`。

- [ ] **Step 3: 实现 serve_conn 与全部协议函数**

把 `serve_conn` 的占位体替换为（并保持 `handle` 不变）：

```rust
    async fn serve_conn(&self, mut client: TcpStream) -> Result<(), SocksError> {
        // --- greeting ---
        let mut v = [0u8; 1];
        client.read_exact(&mut v).await?;
        if v[0] != 0x05 {
            return Ok(());
        }
        let mut nm = [0u8; 1];
        client.read_exact(&mut nm).await?;
        let mut methods = vec![0u8; nm[0] as usize];
        client.read_exact(&mut methods).await?;
        if !methods.contains(&0x00) {
            client.write_all(&[0x05, 0xFF]).await?; // no acceptable methods
            return Ok(());
        }
        client.write_all(&[0x05, 0x00]).await?;

        // --- request ---
        let mut req = [0u8; 4];
        client.read_exact(&mut req).await?;
        let (cmd, atyp) = (req[1], req[3]);
        let host = read_atyp_addr(&mut client, atyp).await?;
        let mut pb = [0u8; 2];
        client.read_exact(&mut pb).await?;
        let port = u16::from_be_bytes(pb);

        if cmd != 0x01 {
            // only CONNECT (BIND / UDP ASSOCIATE fail, as in Go)
            client.write_all(&SOCKS_FAIL_REPLY).await?;
            return Ok(());
        }

        let target = rewrite_host(&host, &self.dns_map.load());

        // --- forward to upstream as a SOCKS5 client; dial + handshake share
        // one bounded window, like Go's SetDeadline around the handshake ---
        let upstream_fut = async {
            let mut up = TcpStream::connect(self.upstream_addr.as_str()).await?;
            upstream_connect(&mut up, &target, port).await?;
            Ok::<TcpStream, SocksError>(up)
        };
        let mut up = match tokio::time::timeout(UPSTREAM_DIAL_TIMEOUT, upstream_fut).await {
            Ok(Ok(up)) => up,
            Ok(Err(_)) | Err(_) => {
                client.write_all(&SOCKS_FAIL_REPLY).await?;
                return Ok(());
            }
        };

        client.write_all(&SOCKS_OK_REPLY).await?;
        relay(&mut client, &mut up, self.relay_idle).await;
        Ok(())
    }
```

在 `impl Server` 块结束之后追加（全部模块级函数）：

```rust
/// Read a SOCKS5 address of the given ATYP. IPv4/IPv6 are textualized like
/// Go (so IP-literal mappings rewrite identically); domains keep their raw
/// bytes, non-UTF-8 included (deliberate, shared with Go).
async fn read_atyp_addr<R: AsyncRead + Unpin>(
    r: &mut R,
    atyp: u8,
) -> Result<Vec<u8>, SocksError> {
    match atyp {
        0x01 => {
            let mut b = [0u8; 4];
            r.read_exact(&mut b).await?;
            Ok(std::net::Ipv4Addr::new(b[0], b[1], b[2], b[3]).to_string().into_bytes())
        }
        0x03 => {
            let mut l = [0u8; 1];
            r.read_exact(&mut l).await?;
            let mut b = vec![0u8; l[0] as usize];
            r.read_exact(&mut b).await?;
            Ok(b)
        }
        0x04 => {
            let mut b = [0u8; 16];
            r.read_exact(&mut b).await?;
            Ok(std::net::Ipv6Addr::from(b).to_string().into_bytes())
        }
        other => Err(SocksError::UnsupportedAtyp(other)),
    }
}

/// SOCKS ATYP classification on raw bytes: textual IPv4/IPv6 -> ATYP 1/4,
/// everything else (domains, tokens, non-UTF-8) -> ATYP 3.
enum Atyp {
    V4(std::net::Ipv4Addr),
    V6(std::net::Ipv6Addr),
    Domain,
}

fn atyp_for(host: &[u8]) -> Atyp {
    match std::str::from_utf8(host) {
        Ok(s) => {
            if let Ok(ip4) = s.parse::<std::net::Ipv4Addr>() {
                Atyp::V4(ip4)
            } else if let Ok(ip6) = s.parse::<std::net::Ipv6Addr>() {
                Atyp::V6(ip6)
            } else {
                Atyp::Domain
            }
        }
        // raw bytes cannot be an IP string
        Err(_) => Atyp::Domain,
    }
}

/// SOCKS5 client handshake with the upstream, asking it to connect to
/// host:port. Domains go as ATYP 0x03 (socks5h: the upstream resolves);
/// IPs use ATYP 0x01/0x04.
async fn upstream_connect(up: &mut TcpStream, host: &[u8], port: u16) -> Result<(), SocksError> {
    up.write_all(&[0x05, 0x01, 0x00]).await?;
    let mut greet = [0u8; 2];
    up.read_exact(&mut greet).await?;
    if greet != [0x05, 0x00] {
        return Err(SocksError::UpstreamRefused);
    }
    let mut req = match atyp_for(host) {
        Atyp::V4(ip) => {
            let mut r = vec![0x05, 0x01, 0x00, 0x01];
            r.extend_from_slice(&ip.octets());
            r
        }
        Atyp::V6(ip6) => {
            let mut r = vec![0x05, 0x01, 0x00, 0x04];
            r.extend_from_slice(&ip6.octets());
            r
        }
        Atyp::Domain => {
            // >255 truncates exactly like Go's byte(len(host))
            let mut r = vec![0x05, 0x01, 0x00, 0x03, host.len() as u8];
            r.extend_from_slice(host);
            r
        }
    };
    req.extend_from_slice(&port.to_be_bytes());
    up.write_all(&req).await?;

    let mut rep = [0u8; 4];
    up.read_exact(&mut rep).await?;
    if rep[0] != 0x05 || rep[1] != 0x00 {
        return Err(SocksError::UpstreamRefused);
    }
    // drain the bound address
    read_atyp_addr(up, rep[3]).await?;
    let mut bnd = [0u8; 2];
    up.read_exact(&mut bnd).await?;
    Ok(())
}

fn now_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0)
}

/// Copy one direction until EOF/error/idle-timeout. The idle clock is SHARED
/// across directions (mirrors the Python select() loop): traffic either way
/// resets the window, so a healthy one-way transfer is never killed as idle.
/// A blocked Read cannot observe clock updates, so after a timeout firing we
/// re-arm whenever another direction has moved the clock past the deadline
/// that just expired — only true idleness returns. Writes stay deadline-free.
async fn pump(
    rd: &tokio::net::tcp::ReadHalf<'_>,
    wr: &tokio::net::tcp::WriteHalf<'_>,
    idle: Duration,
    clock: &AtomicU64,
) -> std::io::Result<()> {
    let idle_ms = idle.as_millis() as u64;
    let mut buf = vec![0u8; 65536];
    loop {
        let deadline = clock.load(Ordering::Relaxed) + idle_ms;
        let wait = deadline.saturating_sub(now_ms());
        match tokio::time::timeout(Duration::from_millis(wait), rd.read(&mut buf)).await {
            Err(_elapsed) => {
                let new_deadline = clock.load(Ordering::Relaxed) + idle_ms;
                if new_deadline > deadline {
                    continue; // activity elsewhere extended the window
                }
                return Err(std::io::Error::new(
                    std::io::ErrorKind::TimedOut,
                    "relay idle timeout",
                ));
            }
            Ok(Err(e)) => return Err(e),
            Ok(Ok(0)) => return Ok(()), // EOF
            Ok(Ok(n)) => {
                clock.store(now_ms(), Ordering::Relaxed);
                wr.write_all(&buf[..n]).await?;
            }
        }
    }
}

/// Bidirectional relay with the shared idle clock and the Go/Python anti-RST
/// teardown: when either direction finishes, queue a FIN on both sockets
/// (`shutdown(Write)` — once FIN is queued, the kernel close answers FIN, not
/// RST, even with unread data still queued) and then drop both. Dropping the
/// losing pump cancels it and releases its halves — Go unblocks the second
/// direction by closing the fds; dropping the future is the tokio equivalent.
pub async fn relay(client: &mut TcpStream, upstream: &mut TcpStream, idle: Duration) {
    let clock = AtomicU64::new(now_ms());
    {
        let (c_rd, c_wr) = client.split();
        let (u_rd, u_wr) = upstream.split();
        let _ = tokio::select! {
            r = pump(&u_rd, &c_wr, idle, &clock) => r,
            r = pump(&c_rd, &u_wr, idle, &clock) => r,
        };
    }
    // half-close both ends (FIN), then drop = close
    let _ = client.shutdown().await;
    let _ = upstream.shutdown().await;
}
```

- [ ] **Step 4: 运行确认绿**

```bash
cd rust && cargo test proxy:: 2>&1 | tail -8
```

Expected: `test result: ok. 5 passed`。

- [ ] **Step 5: Commit**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
git add rust/src/proxy.rs rust/src/lib.rs
git commit -m "Rust: SOCKS5 proxy (server, upstream client, relay with shared idle clock + anti-RST)"
```

---

### Task 8: relay 性质 + 上游边界测试（共享空闲钟 / 半关防 RST / ATYP 字节级）

**Files:**
- Modify: `rust/src/proxy.rs`（tests 模块追加 helper 与 6 个测试）

- [ ] **Step 1: 追加 4 个 helper 与 6 个测试（tests 模块内，紧跟 Task 7 测试之后）**

tests 模块头部 use 区补充：

```rust
    use std::sync::Arc;
```

helper 与测试：

```rust
    /// start_proxy with a short relay idle window (for exercising the shared
    /// idle clock without waiting 300s).
    async fn start_proxy_with_idle(
        mapping: &[(&[u8], &[u8])],
        upstream: &str,
        idle: Duration,
    ) -> (String, tokio::task::JoinHandle<std::io::Result<()>>) {
        let m = mapping
            .iter()
            .map(|(d, t)| (d.to_ascii_lowercase(), t.to_vec()))
            .collect::<HashMap<Vec<u8>, Vec<u8>>>();
        let dns_map = DnsMap::new();
        dns_map.store(m);
        let (ln, mut srv) = Server::bind(dns_map, upstream, "127.0.0.1:0")
            .await
            .unwrap();
        srv.relay_idle = idle;
        let addr = ln.local_addr().unwrap().to_string();
        let handle = tokio::spawn({
            let srv = srv.clone();
            async move { srv.serve(ln).await }
        });
        (addr, handle)
    }

    /// Completes the SOCKS5 handshake and then swallows everything without
    /// ever writing back, counting payload bytes.
    async fn silent_upstream() -> std::io::Result<(String, Arc<AtomicU64>)> {
        let ln = TcpListener::bind("127.0.0.1:0").await?;
        let addr = ln.local_addr()?.to_string();
        let got = Arc::new(AtomicU64::new(0));
        let counter = Arc::clone(&got);
        tokio::spawn(async move {
            let Ok((mut conn, _)) = ln.accept().await else { return };
            let mut hdr = [0u8; 2];
            let _ = conn.read_exact(&mut hdr).await;
            let mut methods = vec![0u8; hdr[1] as usize];
            let _ = conn.read_exact(&mut methods).await;
            let _ = conn.write_all(&[0x05, 0x00]).await;
            let mut req = [0u8; 4];
            let _ = conn.read_exact(&mut req).await;
            match req[3] {
                0x03 => {
                    let mut l = [0u8; 1];
                    let _ = conn.read_exact(&mut l).await;
                    let mut b = vec![0u8; l[0] as usize];
                    let _ = conn.read_exact(&mut b).await;
                }
                0x01 => {
                    let mut b = [0u8; 4];
                    let _ = conn.read_exact(&mut b).await;
                }
                0x04 => {
                    let mut b = [0u8; 16];
                    let _ = conn.read_exact(&mut b).await;
                }
                _ => {}
            }
            let mut pb = [0u8; 2];
            let _ = conn.read_exact(&mut pb).await;
            let _ = conn.write_all(&SOCKS_OK_REPLY).await;
            let mut buf = [0u8; 4096];
            loop {
                match conn.read(&mut buf).await {
                    Ok(0) | Err(_) => return,
                    Ok(n) => counter.fetch_add(n as u64, Ordering::SeqCst),
                }
            }
        });
        Ok((addr, got))
    }

    /// Answer CONNECT, write payload, close WITHOUT reading anything more
    /// (the client's HTTP GET arrives after our close — the original
    /// curl-exit-56 RST scenario).
    async fn early_close_upstream(payload: &[u8]) -> std::io::Result<String> {
        let ln = TcpListener::bind("127.0.0.1:0").await?;
        let addr = ln.local_addr()?.to_string();
        let payload = payload.to_vec();
        tokio::spawn(async move {
            loop {
                let Ok((mut conn, _)) = ln.accept().await else { return };
                let payload = payload.clone();
                tokio::spawn(async move {
                    let mut hdr = [0u8; 2];
                    if conn.read_exact(&mut hdr).await.is_err() {
                        return;
                    }
                    let mut methods = vec![0u8; hdr[1] as usize];
                    if conn.read_exact(&mut methods).await.is_err() {
                        return;
                    }
                    if conn.write_all(&[0x05, 0x00]).await.is_err() {
                        return;
                    }
                    let mut req = [0u8; 4];
                    if conn.read_exact(&mut req).await.is_err() {
                        return;
                    }
                    let skip = match req[3] {
                        0x03 => {
                            let mut l = [0u8; 1];
                            if conn.read_exact(&mut l).await.is_err() {
                                return;
                            }
                            l[0] as usize
                        }
                        0x01 => 4,
                        0x04 => 16,
                        _ => 0,
                    };
                    let mut rest = vec![0u8; skip + 2];
                    if conn.read_exact(&mut rest).await.is_err() {
                        return;
                    }
                    let _ = conn.write_all(&SOCKS_OK_REPLY).await;
                    let _ = conn.write_all(&payload).await;
                    // ...and close without reading the client's request bytes.
                });
            }
        });
        Ok(addr)
    }

    /// Replies 05 00 to the greeting, then reads exactly `total` bytes
    /// (greeting + request) and hands them to the receiver. For byte-level
    /// assertions on what the proxy sends upstream.
    async fn capture_upstream(
        total: usize,
    ) -> std::io::Result<(String, tokio::sync::mpsc::Receiver<Vec<u8>>)> {
        let ln = TcpListener::bind("127.0.0.1:0").await?;
        let addr = ln.local_addr()?.to_string();
        let (tx, rx) = tokio::sync::mpsc::channel(1);
        tokio::spawn(async move {
            let Ok((mut conn, _)) = ln.accept().await else { return };
            let _ = conn.write_all(&[0x05, 0x00]).await;
            let mut buf = vec![0u8; total];
            if conn.read_exact(&mut buf).await.is_err() {
                return;
            }
            let _ = tx.send(buf).await;
        });
        Ok((addr, rx))
    }

    /// Completes the handshake then refuses the CONNECT.
    async fn refuse_upstream() -> std::io::Result<String> {
        let ln = TcpListener::bind("127.0.0.1:0").await?;
        let addr = ln.local_addr()?.to_string();
        tokio::spawn(async move {
            let Ok((mut conn, _)) = ln.accept().await else { return };
            let mut hdr = [0u8; 2];
            let _ = conn.read_exact(&mut hdr).await;
            let mut methods = vec![0u8; hdr[1] as usize];
            let _ = conn.read_exact(&mut methods).await;
            let _ = conn.write_all(&[0x05, 0x00]).await;
            let mut req = [0u8; 4];
            let _ = conn.read_exact(&mut req).await;
            let mut rest = vec![0u8; 4 + 2]; // ATYP 1 addr + port
            let _ = conn.read_exact(&mut rest).await;
            let _ = conn.write_all(&SOCKS_FAIL_REPLY).await;
        });
        Ok(addr)
    }

    // Mirrors go/proxy_test.go TestRelayIdleClockIsSharedAcrossDirections:
    // one shared clock reset by traffic in EITHER direction. A one-way
    // transfer (client pumps, upstream never answers) must not be killed by
    // the idle timeout just because one side is silent.
    #[tokio::test]
    async fn relay_idle_clock_is_shared_across_directions() {
        let (up, got) = silent_upstream().await.unwrap();
        // 200ms idle; the pump below runs 1.2s (6x idle) one-way only.
        let (addr, handle) =
            start_proxy_with_idle(&[], &up, Duration::from_millis(200)).await;

        let mut c = socks_connect(&addr, b"plain.example", 80).await;

        const PINGS: usize = 24;
        for _ in 0..PINGS {
            tokio::time::timeout(Duration::from_secs(2), c.write_all(b"PING\n"))
                .await
                .expect("write timed out")
                .expect("relay died during one-way transfer");
            tokio::time::sleep(Duration::from_millis(50)).await;
        }

        // Every ping must have made it through the relay (5 bytes each).
        let deadline = tokio::time::Instant::now() + Duration::from_secs(2);
        while got.load(Ordering::SeqCst) < (PINGS * 5) as u64
            && tokio::time::Instant::now() < deadline
        {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        assert!(
            got.load(Ordering::SeqCst) >= (PINGS * 5) as u64,
            "upstream got {} bytes, want {} (relay dropped one-way traffic)",
            got.load(Ordering::SeqCst),
            PINGS * 5
        );

        // Silence now: the relay must tear down within ~2x idle (client
        // sees EOF from the queued FIN).
        let quiet = std::time::Instant::now();
        let res = tokio::time::timeout(Duration::from_secs(5), c.read_u8()).await;
        let elapsed = quiet.elapsed();
        handle.abort();
        match res {
            Err(_) => panic!("no teardown after silence"),
            Ok(Ok(_)) => panic!("expected teardown, but read data instead"),
            Ok(Err(_)) => {}
        }
        assert!(
            elapsed <= Duration::from_millis(2 * 200 + 150),
            "teardown took {elapsed:?} after silence, want <= ~550ms"
        );
    }

    // Mirrors TestRelayEarlyClosingUpstreamDeliversDataAndCleanEOF: the
    // upstream answers CONNECT, writes a payload and closes WITHOUT reading
    // the client's request. The relay must still deliver the payload AND end
    // with a clean FIN — closing with unread data queued and no FIN queued
    // makes Linux answer RST, destroying data the peer has not read (the
    // original curl exit 56 bug).
    #[tokio::test]
    async fn relay_early_closing_upstream_delivers_clean_eof() {
        let payload = b"SAW:tcSMOKE123:9999\n".to_vec();
        let up = early_close_upstream(&payload).await.unwrap();
        let (addr, handle) = start_proxy(&[(b"www.example.com", b"tcSMOKE123")], &up).await;

        let mut failures: Vec<String> = Vec::new();
        for i in 0..200 {
            let mut c = socks_connect(&addr, b"www.example.com", 9999).await;
            // The client sends its GET the moment the success reply lands;
            // the upstream is already writing its payload and closing.
            c.write_all(b"GET / HTTP/1.0\r\n\r\n").await.unwrap();
            let mut buf = Vec::new();
            match tokio::time::timeout(Duration::from_secs(2), c.read_to_end(&mut buf)).await {
                Err(_) => failures.push(format!("iteration {}: read timed out", i + 1)),
                Ok(Err(e)) => failures.push(format!(
                    "iteration {}: read error after {} bytes: {e} (want clean EOF)",
                    i + 1,
                    buf.len()
                )),
                Ok(Ok(_)) => {
                    if buf != payload {
                        failures.push(format!(
                            "iteration {}: got {:?}, want {:?}",
                            i + 1,
                            buf,
                            payload
                        ));
                    }
                }
            }
        }
        handle.abort();
        assert!(
            failures.is_empty(),
            "{}/200 iterations failed:\n{}",
            failures.len(),
            failures.join("\n")
        );
    }

    // Mirrors TestUpstreamConnectUsesDomainATYP: capture the exact bytes the
    // proxy sends upstream; domains must go as ATYP 0x03 (socks5h).
    #[tokio::test]
    async fn upstream_connect_uses_domain_atyp() {
        let (up, mut captured) =
            capture_upstream(3 + 4 + 1 + "tcXYZabc123".len() + 2).await.unwrap();
        let mut up_conn = TcpStream::connect(&up).await.unwrap();
        // The capture never sends the final reply, so upstream_connect ends
        // with an error once the capture side is dropped — the request bytes
        // are already captured and asserted below.
        let _ = crate::proxy::upstream_connect(&mut up_conn, b"tcXYZabc123", 443).await;
        drop(up_conn);
        let got = tokio::time::timeout(Duration::from_secs(2), captured.recv())
            .await
            .expect("no request captured")
            .unwrap();
        assert_eq!(&got[..3], &[0x05, 0x01, 0x00], "bad greeting bytes");
        let mut want = vec![0x05, 0x01, 0x00, 0x03, "tcXYZabc123".len() as u8];
        want.extend_from_slice(b"tcXYZabc123");
        want.extend_from_slice(&443u16.to_be_bytes());
        assert_eq!(&got[3..], &want[..], "request bytes differ");
    }

    // Mirrors TestSocks5ProxyForwardsIPv6AsATYP4: the upstream must receive
    // ATYP 4 plus the raw 16 address bytes and the port.
    #[tokio::test]
    async fn forwards_ipv6_as_atyp4() {
        let (up, mut captured) = capture_upstream(3 + 4 + 16 + 2).await.unwrap();
        let (addr, handle) = start_proxy(&[], &up).await;

        let mut c = TcpStream::connect(&addr).await.unwrap();
        c.write_all(&[0x05, 0x01, 0x00]).await.unwrap();
        let mut rep = [0u8; 2];
        c.read_exact(&mut rep).await.unwrap();
        let ip: std::net::Ipv6Addr = "2001:db8::1".parse().unwrap();
        let mut req = vec![0x05, 0x01, 0x00, 0x04];
        req.extend_from_slice(&ip.octets());
        req.extend_from_slice(&443u16.to_be_bytes());
        c.write_all(&req).await.unwrap();
        drop(c);
        handle.abort();

        let got = tokio::time::timeout(Duration::from_secs(2), captured.recv())
            .await
            .expect("no request captured")
            .unwrap();
        assert_eq!(got[6], 0x04, "upstream ATYP = {:#x}, want 0x04", got[6]);
        assert_eq!(&got[7..23], &ip.octets()[..], "upstream addr bytes");
        assert_eq!(&got[23..25], &[0x01, 0xBB], "upstream port bytes (443)");
    }

    // Mirrors TestSocks5ProxyUpstreamDialFailureRepliesFail: unreachable
    // upstream -> generic failure reply to the client.
    #[tokio::test]
    async fn upstream_dial_failure_replies_fail() {
        let ln = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let closed = ln.local_addr().unwrap().to_string();
        drop(ln); // nothing is listening there anymore

        let (addr, handle) = start_proxy(&[], &closed).await;
        let mut c = TcpStream::connect(&addr).await.unwrap();
        c.write_all(&[0x05, 0x01, 0x00]).await.unwrap();
        let mut rep = [0u8; 2];
        c.read_exact(&mut rep).await.unwrap();
        c.write_all(&[0x05, 0x01, 0x00, 0x03, 3, b'a', b'b', b'c', 0x00, 0x50])
            .await
            .unwrap();
        let mut head = [0u8; 10];
        c.read_exact(&mut head).await.unwrap();
        handle.abort();
        assert_eq!(&head, &SOCKS_FAIL_REPLY, "want generic failure reply");
    }

    // Mirrors TestSocks5ProxyUpstreamRefusalRepliesFail: a reachable upstream
    // that rejects the CONNECT also produces the generic failure reply.
    #[tokio::test]
    async fn upstream_refusal_replies_fail() {
        let up = refuse_upstream().await.unwrap();
        let (addr, handle) = start_proxy(&[], &up).await;
        let mut c = TcpStream::connect(&addr).await.unwrap();
        c.write_all(&[0x05, 0x01, 0x00]).await.unwrap();
        let mut rep = [0u8; 2];
        c.read_exact(&mut rep).await.unwrap();
        c.write_all(&[0x05, 0x01, 0x00, 0x01, 127, 0, 0, 1, 0x00, 0x50])
            .await
            .unwrap();
        let mut head = [0u8; 10];
        c.read_exact(&mut head).await.unwrap();
        handle.abort();
        assert_eq!(&head, &SOCKS_FAIL_REPLY, "want generic failure reply");
    }
```

注意其中一处：`upstream_connect` 目前是模块私有 `async fn`。把它的签名改为 `pub(crate) async fn upstream_connect(...)`（仅可见性变化，测试经 `crate::proxy::upstream_connect` 调用）。

- [ ] **Step 2: 运行**

```bash
cd rust && cargo test proxy:: 2>&1 | tail -8
```

Expected: `test result: ok. 11 passed`。这些测试同时是 Task 7 实现的回归锚点——若 `relay_idle_clock_is_shared_across_directions` 或 `relay_early_closing_upstream_delivers_clean_eof` 失败，按 go/proxy.go relay 注释里的语义修 pump/relay（共享时钟重算、先 shutdown(Write) 再 drop），不要改测试期望。

- [ ] **Step 3: Commit**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
git add rust/src/proxy.rs
git commit -m "Rust: relay property tests (shared idle clock, anti-RST) + upstream edge cases"
```

---

### Task 9: tailcat.rs —— 子进程拉起 / wait_ready / terminate（TDD）

**Files:**
- Create: `rust/src/tailcat.rs`
- Create: `rust/src/bin/fake-tailcat.rs`
- Create: `rust/tests/autolaunch.rs`
- Modify: `rust/src/lib.rs`（加 `pub mod tailcat;`）

> **为何测试放 `tests/autolaunch.rs` 而非 `src/tailcat.rs` 内联单测**：cargo 只为
> **集成测试**（tests/ 目录）注入 `CARGO_BIN_EXE_<name>` 环境变量，lib 单测在编译期
> 就会因 `env!` 找不到该变量而失败。所以子进程测试必须写成集成测试。

- [ ] **Step 1: 创建假 tailcat（测试替身，替代 Go 的 testdata/fake-tailcat）**

创建 `rust/src/bin/fake-tailcat.rs`：

```rust
//! fake-tailcat stands in for `tailcat socks` in tests: it parses
//! `socks --listen=host:port` and runs a minimal SOCKS5 server that accepts
//! any CONNECT, replies success, and echoes data back. cargo builds it as a
//! normal bin target, so integration tests get its path for free via
//! env!("CARGO_BIN_EXE_fake-tailcat").
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

fn main() {
    let runtime = tokio::runtime::Runtime::new().expect("tokio runtime");
    runtime.block_on(async_main());
}

async fn async_main() {
    // Like the real `tailcat socks --listen=...`, the subcommand comes
    // before the flag; clap stops at the first non-flag argument, so drop a
    // leading "socks" before parsing.
    let args: Vec<String> = std::env::args()
        .skip(1)
        .skip_while(|a| a == "socks")
        .collect();
    let mut listen = String::from("127.0.0.1:0");
    let mut iter = args.iter();
    while let Some(a) = iter.next() {
        if a == "--listen" {
            listen = iter.next().cloned().unwrap_or(listen);
        } else if let Some(v) = a.strip_prefix("--listen=") {
            listen = v.to_string();
        }
    }

    let ln = TcpListener::bind(&listen).await.expect("fake tailcat bind");
    eprintln!("fake tailcat socks listening on {}", ln.local_addr().unwrap());
    loop {
        let Ok((mut conn, _)) = ln.accept().await else { return };
        tokio::spawn(async move { serve(conn).await });
    }
}

async fn serve(mut conn: TcpStream) {
    let mut v = [0u8; 1];
    if conn.read_exact(&mut v).await.is_err() {
        return;
    }
    if v[0] != 0x05 {
        return;
    }
    let mut nm = [0u8; 1];
    if conn.read_exact(&mut nm).await.is_err() {
        return;
    }
    let mut methods = vec![0u8; nm[0] as usize];
    if conn.read_exact(&mut methods).await.is_err() {
        return;
    }
    if conn.write_all(&[0x05, 0x00]).await.is_err() {
        return;
    }
    let mut req = [0u8; 4];
    if conn.read_exact(&mut req).await.is_err() {
        return;
    }
    let skip = match req[3] {
        0x03 => {
            let mut l = [0u8; 1];
            if conn.read_exact(&mut l).await.is_err() {
                return;
            }
            l[0] as usize
        }
        0x01 => 4,
        0x04 => 16,
        _ => 0,
    };
    let mut rest = vec![0u8; skip + 2];
    if conn.read_exact(&mut rest).await.is_err() {
        return;
    }
    if conn
        .write_all(&[0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0])
        .await
        .is_err()
    {
        return;
    }
    // echo everything back
    let mut buf = [0u8; 4096];
    loop {
        match conn.read(&mut buf).await {
            Ok(0) | Err(_) => return,
            Ok(n) => {
                if conn.write_all(&buf[..n]).await.is_err() {
                    return;
                }
            }
        }
    }
}
```

- [ ] **Step 2: 先写失败测试（tests/autolaunch.rs；此时 tailcat 模块尚不存在）**

创建 `rust/tests/autolaunch.rs`：

```rust
//! Child-lifecycle tests for the auto-launched `tailcat socks`, using the
//! fake-tailcat bin target (path via CARGO_BIN_EXE, which cargo only exposes
//! to integration tests — hence this file, not unit tests in src/).

use std::time::Duration;
use tailcat_socks::config::free_high_port;
use tailcat_socks::tailcat::{spawn_socks, terminate, wait_ready};

// Mirrors go/main_test.go TestSpawnTailcatSocksAndWaitReady.
#[tokio::test]
async fn spawn_and_wait_ready() {
    let port = free_high_port("127.0.0.1");
    let mut child = spawn_socks(&fake_bin(), "127.0.0.1", port)
        .await
        .expect("spawn failed");
    assert!(
        wait_ready(&format!("127.0.0.1:{port}"), Duration::from_secs(5)).await,
        "fake tailcat never became ready"
    );
    terminate(&mut child).await;
}

// Mirrors TestTerminateStopsChild: after terminate() the child is reaped, so
// a follow-up wait() completes immediately.
#[tokio::test]
async fn terminate_stops_child() {
    let port = free_high_port("127.0.0.1");
    let mut child = spawn_socks(&fake_bin(), "127.0.0.1", port)
        .await
        .expect("spawn failed");
    terminate(&mut child).await;
    tokio::time::timeout(Duration::from_secs(2), child.wait())
        .await
        .expect("child did not exit after terminate")
        .expect("wait error");
}

fn fake_bin() -> String {
    env!("CARGO_BIN_EXE_fake-tailcat").to_string()
}
```

- [ ] **Step 3: 运行确认红**

```bash
cd rust && cargo test --test autolaunch 2>&1 | tail -5
```

Expected: 编译失败，`could not find tailcat in tailcat_socks`（模块未创建）。

- [ ] **Step 4: 实现 tailcat.rs + 挂模块，运行确认绿**

`rust/src/lib.rs` 模块区追加：

```rust
pub mod tailcat;
```

创建 `rust/src/tailcat.rs`：

```rust
use std::process::Stdio;
use std::time::Duration;
use tokio::net::TcpStream;
use tokio::process::{Child, Command};
use tokio::time::Instant;

/// Spawn `tailcat socks --listen=host:port`. Child stdout/stderr are
/// inherited so its messages land in our log, like the Go version.
pub async fn spawn_socks(bin_path: &str, host: &str, port: u16) -> std::io::Result<Child> {
    Command::new(bin_path)
        .arg("socks")
        .arg(format!("--listen={host}:{port}"))
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit())
        .spawn()
}

/// Poll TCP-connect to `addr` until it accepts or the timeout hits.
pub async fn wait_ready(addr: &str, timeout: Duration) -> bool {
    let deadline = Instant::now() + timeout;
    while Instant::now() < deadline {
        // timeout() returns Result<io::Result<TcpStream>, Elapsed> — the
        // inner Result is the connect itself; only Ok(Ok(_)) means ready
        // (checked via is_ok_and, else connection-refused reads as ready).
        if tokio::time::timeout(Duration::from_secs(1), TcpStream::connect(addr))
            .await
            .is_ok_and(|r| r.is_ok())
        {
            return true;
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
    false
}

/// Stop the child: SIGTERM, then SIGKILL after 5s. tokio's Child only offers
/// SIGKILL (start_kill); SIGTERM needs libc.
pub async fn terminate(child: &mut Child) {
    let Some(pid) = child.id() else { return }; // already exited
    // SAFETY: kill(2) with a pid we own; only signal delivery, no memory access.
    unsafe { libc::kill(pid as libc::pid_t, libc::SIGTERM) };
    match tokio::time::timeout(Duration::from_secs(5), child.wait()).await {
        Ok(_) => {}
        Err(_) => {
            let _ = child.start_kill();
            let _ = child.wait().await;
        }
    }
}
```

运行：

```bash
cd rust && cargo test --test autolaunch 2>&1 | tail -5
```

Expected: `test result: ok. 2 passed`。

- [ ] **Step 5: Commit**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
git add rust/src/tailcat.rs rust/src/lib.rs rust/src/bin/fake-tailcat.rs rust/tests/autolaunch.rs
git commit -m "Rust: tailcat child lifecycle (spawn/wait_ready/terminate) + fake-tailcat bin"
```

---

### Task 10: e2e 集成测试（tests/e2e.rs —— 假 tailcat 全链路）

**Files:**
- Create: `rust/tests/e2e.rs`

- [ ] **Step 1: 写 e2e 测试**

```rust
//! End-to-end: the real proxy main-loop wiring (minus signal handling) driven
//! against the fake-tailcat binary — the same shape as the Go e2e/smoke tests.

use std::time::Duration;
use tailcat_socks::config::{free_high_port, join_host_port};
use tailcat_socks::dnsmap::{load_dns_file, DnsMap};
use tailcat_socks::proxy::Server;
use tailcat_socks::tailcat::{spawn_socks, terminate, wait_ready};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;

fn fake_bin() -> String {
    env!("CARGO_BIN_EXE_fake-tailcat").to_string()
}

/// Mirrors go/main.go run() happy path: bind server, spawn tailcat socks on
/// a free high port, wait until ready.
async fn boot(dns_txt: &str) -> (String, tokio::process::Child, tokio::task::JoinHandle<std::io::Result<()>>, std::path::PathBuf) {
    let dir = std::env::temp_dir().join(format!("tcdns-e2e-{}-{}", std::process::id(), free_high_port("127.0.0.1")));
    std::fs::create_dir_all(&dir).unwrap();
    let dns_path = dir.join("dns.txt");
    std::fs::write(&dns_path, dns_txt).unwrap();

    let mapping = load_dns_file(&dns_path).expect("load dns.txt");
    let dns_map = DnsMap::new();
    dns_map.store(mapping);

    let port = free_high_port("127.0.0.1");
    let up_addr = join_host_port("127.0.0.1", port);
    let child = spawn_socks(&fake_bin(), "127.0.0.1", port)
        .await
        .expect("spawn tailcat");
    assert!(wait_ready(&up_addr, Duration::from_secs(5)).await, "tailcat not ready");

    let (ln, srv) = Server::bind(dns_map, &up_addr, "127.0.0.1:0").await.unwrap();
    let listen_addr = ln.local_addr().unwrap().to_string();
    let handle = tokio::spawn(async move { srv.serve(ln).await });
    (listen_addr, child, handle, dir)
}

/// SOCKS5 CONNECT via domain ATYP, returns the connected stream.
async fn socks_connect(proxy_addr: &str, host: &[u8], port: u16) -> TcpStream {
    let mut c = TcpStream::connect(proxy_addr).await.unwrap();
    c.write_all(&[0x05, 0x01, 0x00]).await.unwrap();
    let mut rep = [0u8; 2];
    c.read_exact(&mut rep).await.unwrap();
    assert_eq!(&rep, &[0x05, 0x00]);
    let mut req = vec![0x05, 0x01, 0x00, 0x03, host.len() as u8];
    req.extend_from_slice(host);
    req.extend_from_slice(&port.to_be_bytes());
    c.write_all(&req).await.unwrap();
    let mut head = [0u8; 10];
    c.read_exact(&mut head).await.unwrap();
    assert_eq!(head[1], 0x00, "CONNECT failed: {head:?}");
    c
}

#[tokio::test]
async fn full_chain_rewrite_via_fake_tailcat() {
    let (addr, mut child, handle, dir) =
        boot("tcXYZabc123   www.example.com  api.example.com\ntcDEF456ghi   foo.com\n").await;

    // mapped domain: fake tailcat echoes, so we see our own payload back
    let mut c = socks_connect(&addr, b"www.example.com", 8888).await;
    c.write_all(b"ping").await.unwrap();
    let mut buf = [0u8; 4];
    c.read_exact(&mut buf).await.unwrap();
    assert_eq!(&buf, b"ping", "fake tailcat echo through full chain");
    drop(c);

    // second domain on the same token still works
    let mut c = socks_connect(&addr, b"api.example.com", 8888).await;
    c.write_all(b"pong").await.unwrap();
    let mut buf = [0u8; 4];
    c.read_exact(&mut buf).await.unwrap();
    assert_eq!(&buf, b"pong");
    drop(c);

    handle.abort();
    terminate(&mut child).await;
    let _ = child.wait().await;
    let _ = std::fs::remove_dir_all(&dir);
}
```

- [ ] **Step 2: 运行**

```bash
cd rust && cargo test --test e2e 2>&1 | tail -5
```

Expected: `test result: ok. 1 passed`。

- [ ] **Step 3: Commit**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
git add rust/tests/e2e.rs
git commit -m "Rust: e2e test through fake-tailcat (autolaunch chain)"
```

---

### Task 11: main.rs —— 装配与生命周期（信号/退出/日志）

**Files:**
- Modify: `rust/src/main.rs`（整体替换占位）

- [ ] **Step 1: 写完整 main.rs**

```rust
//! tailcat-socks: a SOCKS5 front proxy that rewrites real domain names to
//! tailcat tokens (tc...) and chains to a single standalone `tailcat socks`
//! upstream. Rust implementation of the Python/Go versions; see README.md.
use clap::Parser;
use tailcat_socks::config::{free_high_port, join_host_port, parse_addr, Config};
use tailcat_socks::dnsmap::{load_dns_file, token_set, watch, DnsMap};
use tailcat_socks::logging::log_line;
use tailcat_socks::proxy::Server;
use tailcat_socks::tailcat::{spawn_socks, terminate, wait_ready};
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
    use tokio::signal::unix::{signal, SignalKind};
    let mut sigterm = signal(SignalKind::terminate()).expect("install SIGTERM handler");
    let mut sigint = signal(SignalKind::interrupt()).expect("install SIGINT handler");

    let first = match load_dns_file(std::path::Path::new(&cfg.dns_file)) {
        Ok(m) => m,
        Err(err) => {
            log_line(format!("{err}")); // DnsFileError Display already says "cannot load <path>: ..."
            return 1;
        }
    };
    let dns_map = DnsMap::new();
    dns_map.store(first.clone());

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
    let up_port = if up_port == 0 { free_high_port(&up_host) } else { up_port };
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
        tokio::spawn(watch(dns_path, dns_map.clone(), std::time::Duration::from_secs(1), stop.clone()));
    }

    let serve = tokio::spawn(async move { srv.serve(ln).await });

    log_line(format!("listening socks5h://{actual}"));
    log_line(format!(
        "{} domain(s) mapped -> {} token(s)",
        first.len(),
        token_set(&first).len()
    ));
    log_line(format!("upstream {up_addr}"));

    tokio::select! {
        _ = sigterm.recv() => {}
        _ = sigint.recv() => {}
        err = async { serve.await.expect("serve task panicked") } => {
            // Server errors on shutdown are net.ErrClosed-equivalent noise;
            // keep signal exits quiet like the Go version.
            if let Err(e) = err {
                log_line(format!("server error: {e}"));
            }
        }
    }
    stop.cancel();
    if let Some(c) = child.as_mut() {
        terminate(c).await;
    }
    0
}
```

- [ ] **Step 2: 编译 + 全量测试**

```bash
cd rust && cargo build 2>&1 | tail -3 && cargo test 2>&1 | tail -6
```

Expected: build 无 error；全部测试通过。

- [ ] **Step 3: 手动冒烟（--no-autolaunch + fake-tailcat 做上游）**

```bash
cd rust
cargo build --bins 2>/dev/null
# 用 fake-tailcat 冒充真 tailcat（端口 127.0.0.1:18099）
(target/debug/fake-tailcat socks --listen=127.0.0.1:18099 &>/tmp/fake-tailcat.log & echo $! > /tmp/fake-tailcat.pid)
printf 'tcSMOKE123   smoke.example.com\n' > /tmp/dns-smoke.txt
(target/debug/tailcat-socks --listen 127.0.0.1:18098 --dns-file /tmp/dns-smoke.txt --upstream 127.0.0.1:18099 --no-watch &>/tmp/proxy-smoke.log & echo $! > /tmp/proxy-smoke.pid)
sleep 0.5
curl -s --socks5-hostname 127.0.0.1:18098 http://smoke.example.com:9999/hello -m 5 | head -c 40; echo
kill $(cat /tmp/proxy-smoke.pid) $(cat /tmp/fake-tailcat.pid) 2>/dev/null
cat /tmp/proxy-smoke.log
```

Expected: curl 输出 `hello`（fake tailcat echo 的响应体），proxy 日志含 `listening socks5h://127.0.0.1:18098`、`1 domain(s) mapped -> 1 token(s)`、`upstream 127.0.0.1:18099`，且无 `failed to launch`。

（fake-tailcat 只回 SOCKS5 成功 + echo：curl 的 HTTP 请求会被原样 echo 回来，因此 body 是 HTTP 请求文本本身——冒烟只验证链路通。）

- [ ] **Step 4: Commit**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
git add rust/src/main.rs
git commit -m "Rust: main assembly (signals, autolaunch, shutdown order)"
```

---

### Task 12: bin/ 脚本支持 rust + README + 规格微调回写

**Files:**
- Modify: `bin/start.sh`
- Modify: `bin/stop.sh`
- Modify: `bin/restart.sh`
- Modify: `README.md`
- Modify: `docs/superpowers/specs/2026-09-05-rust-reimplementation-design.md`

- [ ] **Step 1: start.sh 支持 rust**

`bin/start.sh` 三处修改：

(a) 用法行（文件头与 usage 报错）改为 `[go|python|rust]`：

```bash
# Usage: bin/start.sh [go|python|rust]    (default: go)
```

(b) case 分支在 `python)` 之后追加：

```bash
  rust)
    BIN="$ROOT/rust/target/release/tailcat-socks"
    PID_FILE="$RUN_DIR/tailcat-socks-rust.pid"
    LOG_FILE="$RUN_DIR/tailcat-socks-rust.log"
    ;;
```

(c) 构建块：在 go 构建块之后追加（并改 Go 构建注释为通用措辞）：

```bash
# Build the proxy if the binary is missing (requires the toolchain).
if [[ "$IMPL" == rust && ! -x "$BIN" ]]; then
  echo "rust binary missing; building (cargo build --release)..."
  if command -v cargo >/dev/null 2>&1; then
    (cd "$ROOT/rust" && cargo build --release)
  elif [[ -f "$HOME/.cargo/env" ]]; then
    (source "$HOME/.cargo/env" && cd "$ROOT/rust" && cargo build --release)
  else
    echo "cargo not found (install via https://rustup.rs)" >&2
    exit 1
  fi
fi
```

(d) 运行分支：`if [[ "$IMPL" == go ]]` 改为 `if [[ "$IMPL" != python ]]`（go 与 rust 同为「直接跑二进制」）。

- [ ] **Step 2: stop.sh / restart.sh**

`bin/stop.sh`：

```bash
IMPL="${1:-}"
case "$IMPL" in
  "")     PID_FILES=("$RUN_DIR/tailcat-socks-go.pid" "$RUN_DIR/tailcat-socks-python.pid" "$RUN_DIR/tailcat-socks-rust.pid") ;;
  go)     PID_FILES=("$RUN_DIR/tailcat-socks-go.pid") ;;
  python) PID_FILES=("$RUN_DIR/tailcat-socks-python.pid") ;;
  rust)   PID_FILES=("$RUN_DIR/tailcat-socks-rust.pid") ;;
  *)      echo "usage: $0 [go|python|rust]" >&2; exit 2 ;;
esac
```

（用法注释同样加 `rust`。）`bin/restart.sh` 头注释改为 `[go|python|rust]`，其余透传逻辑不变（`stop.sh ""` → `start.sh ""` 语义保持：无参数 = 停全部 + 启 Go）。

- [ ] **Step 3: is_ours 匹配 rust 二进制**

两个脚本的 `is_ours` 已按 `*tailcat-socks` 通配匹配，rust 二进制名 `tailcat-socks` 自然命中，无需改动。验证：

```bash
bash -n bin/start.sh bin/stop.sh bin/restart.sh && echo syntax-ok
```

Expected: `syntax-ok`。

- [ ] **Step 4: README 更新**

`README.md` 五处：

(a) 仓库描述段（第 7 行附近）改为：

```markdown
本仓库包含行为一致的**三个实现**:`python/`(原版,纯标准库)、`go/`(零第三方依赖,单静态二进制)与 `rust/`(tokio 生态)。Docker 镜像基于 Go 版构建。
```

(b) 「安装依赖」列表追加：

```markdown
- Rust 版:Rust 1.75+(构建用;或直接用已构建的 `rust/target/release/tailcat-socks`)
```

(c) 「快速开始(裸机)」的运行命令区追加：

```bash
# Rust 版(推荐:先构建一次)
cd rust && cargo build --release && cd ..
rust/target/release/tailcat-socks
```

以及 `bin/start.sh` 用法注释区追加一行：

```bash
bin/start.sh rust       # 后台启动 Rust 版
```

(d) 「启停脚本」节的文件名清单加 rust：`run/tailcat-socks-{go,python,rust}.{pid,log}`；用法块加 `bin/stop.sh rust` 与 `bin/restart.sh rust`。

(e) 「目录结构」树在 `go/` 之后插入：

```
├── rust/                             # Rust 复刻版(tokio 生态)
│   ├── Cargo.toml
│   ├── src/{main,lib,config,dnsmap,proxy,tailcat,error,logging}.rs
│   ├── src/bin/fake-tailcat.rs       # 测试用假 tailcat
│   └── tests/{e2e,autolaunch}.rs
```

「测试」节追加：

```bash
cd rust && cargo test                # Rust 版:解析/改写/热加载/端到端
```

(f) 「实现范围」节支持列表追加 `tailcat 子进程自动拉起/回收` 已有——在其后补一句：`三个实现行为一致;Docker 镜像仍基于 Go 版构建。`

- [ ] **Step 5: 规格微调回写**

`docs/superpowers/specs/2026-09-05-rust-reimplementation-design.md` 两处：

(a) 依赖清单代码块删除 `anyhow = "1"` 行，并在其下加一行说明：

```markdown
（anyhow 在规划期移除：main 的错误路径都是「打日志 + 退出码」，无错误冒泡需求。）
```

(b) 目录结构树中 `src/` 列表改为实际落地的文件（加 `lib.rs`、`logging.rs`，bin 目标说明）：

```
│   ├── src/
│   │   ├── main.rs        # bin 目标：装配与生命周期
│   │   ├── lib.rs         # 库目标：模块出口（集成测试经它访问内部）
│   │   ├── config.rs      # clap derive 的 Config + parse_addr/free_high_port
│   │   ├── dnsmap.rs      # DnsMap（ArcSwap）+ load_dns_file + watch
│   │   ├── proxy.rs       # Server / SOCKS5 服务端 / 上游握手 / relay
│   │   ├── tailcat.rs     # spawn_socks / wait_ready / terminate
│   │   ├── error.rs       # thiserror 错误类型
│   │   ├── logging.rs     # [tailcat-socks] 前缀日志
│   │   └── bin/fake-tailcat.rs  # 测试用假 tailcat（cargo bin 目标）
```

- [ ] **Step 6: 全量验证**

```bash
cd rust && cargo fmt --check 2>&1 | head -5; cargo clippy --all-targets -- -D warnings 2>&1 | tail -5 && cargo test 2>&1 | tail -4
```

Expected: fmt 无 diff（或 `cargo fmt` 一遍后归零）；clippy 无告警；测试全绿。

（若 fmt 报 diff：先跑 `cargo fmt`，把改动一并提交。）

- [ ] **Step 7: 真机冒烟（bin 脚本 + 热加载）**

```bash
bin/stop.sh rust 2>/dev/null; bin/start.sh rust && sleep 1 && tail -5 run/tailcat-socks-rust.log && bin/stop.sh rust
```

Expected: start 输出 `started [rust] pid …`，日志含 `listening socks5h://…`、`N domain(s) mapped -> M token(s)`（dns.txt 已存在），stop 输出 `stopped`。

热加载冒烟——用 /tmp 副本做 `DNS_FILE`，**不动真实 dns.txt**（规格冒烟清单要求验证 reload 日志行）：

```bash
cp dns.txt /tmp/dns-hot.txt
DNS_FILE=/tmp/dns-hot.txt bin/start.sh rust && sleep 1
printf 'tcHotReload hot.example.org\n' >> /tmp/dns-hot.txt
sleep 2
grep reloaded run/tailcat-socks-rust.log
bin/stop.sh rust && rm -f /tmp/dns-hot.txt
```

Expected: grep 命中一行 `reloaded /tmp/dns-hot.txt: N domain(s) -> M token(s)`，token 数比首启多 1；stop 输出 `stopped`。

- [ ] **Step 8: Commit**

```bash
cd /data/yaofeng/workspace/popeye/tailcat-socks
git add bin/ README.md docs/superpowers/specs/2026-09-05-rust-reimplementation-design.md docs/superpowers/plans/2026-09-05-rust-reimplementation.md
git commit -m "bin: rust support (start/stop/restart); README triple-implementation; spec sync"
```

---

## 完成定义（验收）

1. `cargo fmt --check`、`cargo clippy --all-targets -- -D warnings`、`cargo test` 全绿（单测 + 集成 + e2e）。
2. `bin/start.sh rust` / `bin/stop.sh rust` 往返正常，日志格式与 go 版一致。
3. 真机 `curl --socks5-hostname` 经 rust 版代理 + 真 tailcat 走通（用户环境有真实 token 时）。
4. README/spec 与实现一致；`go test ./...` 与 `python3 -m pytest python/tests -v` 仍绿（两版未受影响）。
