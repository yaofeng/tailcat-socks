# tailcat-dns-proxy Rust 复刻设计（tokio 生态，第三实现）

日期：2026-09-05
状态：已批准

## 目标

1. 新建 `rust/` 目录，作为与 `python/`、`go/` 并列的**第三个生产行为一致**的实现。
2. 技术选型：tokio 生态的「地道 Rust」写法（方案 B）——`clap` derive 配置结构、`arc-swap` 原子映射、`tokio_util::CancellationToken` 生命周期、`thiserror` 错误类型。
3. 可观察行为与 Go/Python 版逐段对齐：flag 同名同默认、SOCKS5 字节级行为、改写规则、热加载、tailcat 自动拉起/回收、relay 半关防 RST 与共享空闲超时。
4. `bin/{start,stop,restart}.sh` 支持 `rust`；README 更新。**Docker 不动**（镜像继续基于 Go 版构建）。

## 目录结构

```
tailcat-socks/
├── rust/
│   ├── Cargo.toml
│   ├── src/
│   │   ├── main.rs        # bin 目标：装配与生命周期
│   │   ├── lib.rs         # 库目标：模块出口（集成测试经它访问内部）
│   │   ├── config.rs      # clap derive 的 Config + parse_addr/free_high_port
│   │   ├── dnsmap.rs      # DnsMap（ArcSwap）+ load_dns_file + watch
│   │   ├── proxy.rs       # Server / SOCKS5 服务端 / 上游握手 / relay
│   │   ├── tailcat.rs     # spawn_socks / wait_ready / terminate
│   │   ├── error.rs       # thiserror 错误类型
│   │   ├── logging.rs     # [tailcat-dns-proxy] 前缀日志
│   │   └── bin/fake-tailcat.rs  # 测试用假 tailcat（cargo bin 目标）
│   └── tests/               # 集成测试（e2e / autolaunch / waitready_deadport；dnsmap 热加载与 relay 为 src 内单元测试）
├── bin/{start,stop,restart}.sh   # 增加 rust 选项
└── (其余不变)
```

构建产物在 `rust/target/`（根 `.gitignore` 追加）。

## 依赖清单（Cargo.toml 直接依赖，6 个）

```toml
tokio      = { version = "1", features = ["rt-multi-thread", "net", "process", "signal", "time", "macros", "io-util"] }
clap       = { version = "4", features = ["derive"] }
arc-swap   = "1"
tokio-util = "0.7"                         # CancellationToken
thiserror  = "2"
libc       = "0.2"    # 仅 unix：向 tailcat 子进程发 SIGTERM（tokio 只提供 SIGKILL）
```

（anyhow 在规划期移除：main 的错误路径都是「打日志 + 退出码」，无错误冒泡需求。）

选型理由：tokio+clap 是生态中被最广泛审计的组合；`TcpStream::shutdown` std 自带，半关无需额外 crate；随机端口读 `/dev/urandom`，不引 rand。

## 组件设计

### config（config.rs）

- `Config`（clap derive），flag 与 Go/Python 完全同名同默认：
  `--listen 127.0.0.1:1080`、`--dns-file dns.txt`、`--upstream 127.0.0.1:0`、
  `--tailcat-bin tailcat`、`--no-autolaunch`、`--no-watch`。
- `parse_addr(s)`：复刻 Python `rpartition(":")` 语义——最后一个冒号后是端口，前面是主机（空 → `127.0.0.1`）；无冒号则整串视为端口；容忍方括号 IPv6（剥括号）。
- `free_high_port(host)`：从 `/dev/urandom` 取随机数在 20000–60999 试绑（最多 20 次），全失败回退绑定 port 0 由 OS 分配。urandom 不可用时以系统时间纳秒做种子兜底。

### dnsmap（dnsmap.rs）

- 映射类型 `HashMap<Vec<u8>, String>`：**key 用原始字节而非 String**。Go 版对非 UTF-8 域名字节原样透传（`string(b)`），Rust 的 `String` 无法表示非法 UTF-8；字节 key 保住该行为。域名匹配大小写不敏感用 `to_ascii_lowercase`（逐字节，非 Unicode 折叠，与 Go `strings.ToLower` 对 ASCII 域名等效）。
- `load_dns_file(path) -> Result<HashMap, DnsFileError>`：逐行解析；`#` 注释与空行跳过；空白（空格/Tab）分字段；首字段 token 原样保留大小写，其余字段为域名（小写化存储）；后行覆盖前行；只有 token 无域名的行不产生映射。
- `DnsMap`：`arc_swap::ArcSwap<HashMap<..>>`。读端 `load()` 返回 `Guard`/`Arc` 快照（无锁，对应 Go `atomic.Value`）；写端 `store()` 原子替换。
- `watch(path, map, interval, cancel)`：每秒 stat mtime；**先 stat 后载入**（写入落在两步之间时 mtime 偏旧，下个 tick 必然重载，不吞更新）；变化即重载并打日志；载入失败保留旧映射并警告；**初次载入失败时清零 mtime 记录，每个 tick 重试直至成功**；`cancel` 取消即返回。

### proxy（proxy.rs）

- `Server`：`tokio::net::TcpListener` + accept 循环，每连接 `tokio::spawn`。持有 `Arc<DnsMap>`、upstream 地址、relay 空闲时长。
- SOCKS5 服务端（字节级对齐 Go 版）：
  - greeting：校验 VER=5；读 methods，含 `0x00` 回 `05 00`，否则回 `05 FF` 断开。
  - 请求：读 VER/CMD/RSV/ATYP + 变长地址 + 2 字节端口；**仅 CONNECT**，其余 CMD 回 `05 01 00 01 + 6×00`（general failure）；ATYP 1（4 字节 IPv4 文本化）/ 3（1 字节长度 + 原始字节域名）/ 4（16 字节 IPv6 文本化）。
  - 改写：`rewrite_host` 查 DnsMap（字节级小写匹配）命中换 token（大小写原样），未命中原样透传。
  - 上游（作为 SOCKS5 客户端）：`TcpStream::connect` 15s 超时；greeting `05 01 00`，要求回 `05 00`；目标为 IP 时 ATYP 1/4，域名（含 token）用 ATYP 3（socks5h，解析交上游）；握手整体受 15s read 约束（对应 Go `SetDeadline`），成功后清除；回复非 `05 00` 视为上游拒绝 → 回客户端失败。
  - 成功回客户端 `05 00 00 01 + 6×00`，进入 relay。
- relay（两个关键语义，Go 版注释已验证的行为）：
  - **共享空闲时钟**：`Arc<AtomicU64>` 存最后活动毫秒时间戳；每个方向读到数据即更新时钟；读超时基于 `last_activity + 300s` 的 deadline，超时触发时若时钟已被另一方向推进则继续等待（阻塞读感知不到时钟更新，须重算）；仅真空闲才断开。单向长传输（如只下行的大下载）永不被误杀。
  - **半关防 RST**：一侧结束后，对两条流各 `shutdown(Shutdown::Write)` 发 FIN，然后 drop 关闭。防 RST 的性质来自写侧 FIN（FIN 已排队后，close 不再因「队列有未读数据」改发 RST——Go 版实证结论）。tokio 无读侧 shutdown API，且读侧半关并非防 RST 的必要条件，省略；行为差异窗口与 Python `shutdown(SHUT_RD)` 的既有代价一致。
- 空闲超时常量 300s、上游拨号/握手 15s，与 Go 版同名常量对齐。

### main（main.rs）

启动顺序（Go 版防孤儿次序）：

1. **先装 SIGINT/SIGTERM 处理**（`tokio::signal::unix`），再做任何会失败的动作。
2. `load_dns_file` 初始载入，失败打日志退出码 1。
3. `parse_addr` 归一化 `--listen`（`:8080` 不绑全网卡）与 `--upstream`；端口 0 → `free_high_port`。
4. bind 监听，失败退出码 1。
5. 非 `--no-autolaunch`：`tokio::process::Command(tailcat, ["socks", "--listen=host:port"])` 拉起（stdout/stderr inherit 汇入同一日志）；`wait_ready` 轮询 TCP 连通（15s 超时、200ms 间隔）；不就绪则终止子进程、退出码 1。
6. 非 `--no-watch`：spawn watcher 任务（1s 间隔，CancellationToken）。
7. spawn serve 循环；打三条日志（`listening socks5h://host:port`、`N domain(s) mapped -> M token(s)`、`upstream addr`），前缀 `[tailcat-dns-proxy]`，格式与 Go 版一致。
8. `select!` 信号 vs serve 退出；serve 因取消产生的 `ErrClosed` 类错误静默。退出时：cancel token → 停监听 → 终止子进程（`libc::kill(pid, SIGTERM)` → 等 5s → `start_kill()` SIGKILL）。

### error（error.rs）

`thiserror`：`ConfigError`（地址解析失败）、`DnsFileError`（读/解析失败）、`SocksError`（协议违规/上游拒绝）。IO 错误直接透传 `std::io::Error`。`main` 的错误路径统一为「打日志 + 退出码」，不引入 anyhow。

## bin/ 脚本集成

- `start.sh`：`rust` 分支 → `BIN="$ROOT/rust/target/release/tailcat-dns-proxy"`、PID/LOG 为 `run/tailcat-dns-proxy-rust.{pid,log}`；二进制缺失时 `cargo build --release`（在 `rust/` 目录）；运行命令 `nohup "$BIN" …`（与 go 分支同构）。
- `stop.sh` / `restart.sh`：候选版本从 `go|python` 扩为 `go|python|rust`；默认行为（无参数）覆盖全部版本；pid 归属校验逻辑复用，无改动。
- 环境变量 `LISTEN / DNS_FILE / UPSTREAM / TAILCAT_BIN / PROXY_ARGS` 三版共用。
- 用法注释更新为 `[go|python|rust]`。

## README / 文档更新

- 目录结构加 `rust/` 树；仓库描述改为「三个行为一致的实现」。
- 安装依赖加 Rust 1.75+（构建用；或直接用已构建二进制）。
- 快速开始加 `cd rust && cargo build --release && …` 与 `bin/start.sh rust`。
- 测试节加 `cd rust && cargo test`。
- Docker 节明确标注镜像仍基于 Go 版构建。

## 测试计划（验收标准）

1. `cargo fmt --check` / `cargo clippy` 无告警（`-D warnings`）。
2. `cargo test` 全绿：
   - **单测**（内联 `#[cfg(test)]`）：`parse_addr`（裸端口/空主机/方括号 IPv6/坏端口）；`free_high_port` 返回值域；dns 解析（注释、空行、Tab、仅 token 行、后行覆盖、大小写）；`rewrite_host`（命中换 token 原样大小写、未命中透传、非 ASCII 字节透传）。
   - **dnsmap 集成**：mtime 变化触发重载；写失败/读失败保留旧映射；初次载入失败后每 tick 重试成功。
   - **e2e**（tests/，假上游为测试内起的 tokio SOCKS5 服务）：greeting 方法选择与 `05 FF`；非 CONNECT 回失败；ATYP 1/3/4；命中改写（含混合大小写 token 原样）；未命中透传；成功/失败回复字节；数据双向 echo relay。
   - **relay 专项**：半关防 RST（一端早关后对端已发数据仍可读完，对端收到 FIN 非 RST）；共享空闲钟（单向持续流不被空闲超时杀；真空闲 300s 断开——测试用缩短的注入时长）。
   - **自动拉起**：`fake-tailcat` 经 `--tailcat-bin` 拉起，端口就绪后服务可用；`SIGTERM` 后子进程被回收（无孤儿）。
3. `fake-tailcat`（`src/bin/fake-tailcat.rs`）：解析 `socks --listen=…`（子命令在 flag 前，与真 tailcat 一致），起最小 SOCKS5 服务接受任意 CONNECT 回成功并 echo。cargo 将其与主程序一并构建，集成测试用 `env!("CARGO_BIN_EXE_fake-tailcat")` 取路径——替代 Go 版 `testdata/fake-tailcat` 的 Go 小程序，无需单独编译步骤。
4. 集成冒烟（真机，dns.txt 填真实 token 后）：
   - `bin/start.sh rust` 启动、日志落盘；`curl --socks5-hostname 127.0.0.1:1080 http://<域名>:8888/health` 走通；`bin/stop.sh rust` 停干净且无 tailcat 残留。
   - 改 `dns.txt` 热加载生效（日志出现 reload 行）。

## 非目标（YAGNI，与前两版一致）

- 不支持 UDP ASSOCIATE / BIND（回失败）。
- 不支持用户名/密码认证。
- 不引入 tracing/log 框架（保留 `[tailcat-dns-proxy]` 前缀小宏，日志格式与现网一致）。
- 不做 Docker 集成（镜像继续基于 Go 版）。
- 不追求与 Go 版源码级同构（方案 B 允许地道 Rust 结构），但**可观察行为必须对齐**。
