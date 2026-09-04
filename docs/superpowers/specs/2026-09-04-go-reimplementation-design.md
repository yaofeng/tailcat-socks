# tailcat-dns-proxy 双语言重构设计（Python 迁移 + Go 复刻）

日期：2026-09-04
状态：已批准

## 目标

1. 新建 `python/` 与 `go/` 目录，将现有 Python 实现移入 `python/`。
2. 用 Go 复刻一个**行为完全一致**的代理（full parity）。
3. Docker 镜像改为基于 Go 版本构建（runtime 不再含 Python）。
4. `bin/start.sh` 支持选择启动两个版本。

## 目录结构

```
tailcat-dns-proxy/
├── python/                          # ← src/ 与 tests/ 移入（纯移动）
│   ├── tailcat_dns_proxy.py
│   └── tests/test_tailcat_dns_proxy.py
├── go/
│   ├── go.mod                       # 纯标准库，零第三方依赖
│   ├── main.go                      # 启动编排：flag / autolaunch / 信号 / 热加载
│   ├── dnsmap.go                    # dns.txt 解析 + 原子热加载
│   ├── dnsmap_test.go
│   ├── proxy.go                     # SOCKS5 服务端 + 上游客户端握手 + relay
│   ├── proxy_test.go
│   └── bin/                         # 构建产物（gitignore）
├── bin/{start,stop,restart}.sh
├── docker/{Dockerfile,docker-compose.yml}
├── dns.txt.example
└── run/                             # pid + log（脚本生成，gitignore）
```

Python 文件用 `git mv` 移动；测试脚本内 `sys.path` 相对路径改一行指向新位置。

## Go 组件设计

### dnsmap（dnsmap.go）

- `LoadDNSFile(path) (map[string]string, error)`：逐行解析；`#` 注释与空行跳过；每行首个字段为 token，其余字段为域名；域名小写化存储，token 原样保留大小写；后行覆盖前行（同名域名最后一条生效）；只有 token 无域名的行不产生映射。
- `DNSMap`：`atomic.Value` 包裹 `map[string]string`，读端 `Load()` 无锁，替换即热加载。
- `WatchDNSFile(path, m, interval, stop <-chan struct{})`：每秒轮询 mtime；变化即重载并打日志；读失败保留旧映射并打警告；启动时先做一次初始加载。

### proxy（proxy.go）

- `Server`：监听 → 每连接一个 goroutine，连接结束即回收。
- SOCKS5 协议逐字节对齐 Python 版：
  - greeting：校验 VER=5，收集客户端 methods，含 `0x00` 则回 `05 00`，否则回 `05 FF` 并断开。
  - 请求：仅支持 CONNECT（CMD=1），其余回失败；解析 ATYP 1（IPv4）/ 3（域名）/ 4（IPv6）。
  - 改写：查 map（大小写不敏感）命中换 token，未命中原样透传。
  - 上游：作为 SOCKS5 客户端连 `--upstream`，greeting `05 01 00`；目标为 IP 时用 ATYP 1/4，域名（含 token）用 ATYP=3（socks5h 语义，解析交给上游）。
  - 回复：成功回 `05 00 00 01 + 6×00`；任何失败回 `05 01 00 01 + 6×00`（general failure）。
- relay：双向 `io.Copy`（两 goroutine），任一方向结束即关闭两端；空闲超时 300s 用 `SetDeadline` 等价实现（对齐 Python select 300s 语义）。

### main（main.go）

- flag 与 Python 完全同名同默认：
  - `--listen 127.0.0.1:1080`
  - `--dns-file dns.txt`
  - `--upstream 127.0.0.1:0`（端口 0 = 随机高位空闲口）
  - `--tailcat-bin tailcat`
  - `--no-autolaunch` / `--no-watch`
- autolaunch：端口 0 时从 20000–60999 随机探测空闲口（20 次，失败回退 OS 分配）；`exec.Command(tailcat, "socks", "--listen=host:port")` 拉起子进程；轮询 TCP 就绪（15s 超时），不就绪则终止子进程并退出码 1。
- 信号：SIGINT/SIGTERM → 停监听 → 子进程先 `SIGTERM`，5s 不退 `SIGKILL`（复刻 Python atexit + terminate 语义）。
- 日志：前缀 `[tailcat-dns-proxy]`，格式与 Python 输出一致（listening / mapped 数量 / upstream / relaunched 等）。

## Docker

多阶段构建（`docker/Dockerfile`）：

- stage1 `golang:1.23-bookworm`：`CGO_ENABLED=0 go build` 出静态代理二进制；`go install github.com/tailscale/tailcat/cmd/tailcat@latest`。
- stage2 `debian:bookworm-slim`：仅装 `ca-certificates`；拷入代理二进制与 tailcat；`COPY dns.txt.example /app/dns.txt` 兜底。
- ENV / EXPOSE 1080 / exec 形式 CMD 结构保持不变（PID 1 转发信号）。
- `docker-compose.yml` 不变（挂载、端口、环境变量照旧）。

## bin/start.sh 双版本支持

```
bin/start.sh          # 默认 go
bin/start.sh python   # Python 版
```

- 第一个位置参数选版本，缺省 `go`。
- Go 二进制缺失时自动 `go build -o go/bin/tailcat-dns-proxy ./go`。
- PID/LOG 按版本分离：`run/tailcat-dns-proxy-{go,python}.pid` / `.log`，两版本可并存但同版本互斥（已有 pid 存活则拒绝启动）。
- `stop.sh` 同样接受可选版本参数：给参数只停该版本，不给则两个都尝试停。
- 环境变量 LISTEN / DNS_FILE / UPSTREAM / TAILCAT_BIN / PROXY_ARGS 两版本共用。
- `restart.sh` 透传版本参数。

## 测试计划（验收标准）

1. `gofmt` / `go vet` 干净。
2. `go test ./...` 全绿，覆盖：
   - dns.txt 解析：注释、空行、tab 分隔、缺域名行、后行覆盖；
   - 大小写改写：命中换 token、大小写不敏感匹配、未命中透传；
   - 热加载：mtime 变化触发重载、读失败保留旧映射；
   - 端到端：假上游验证 CONNECT 的 host 被改写为 token、端口正确、数据双向 relay（echo）。
3. Python 原测试从新路径跑通（确认移动无破坏）。
4. 集成冒烟（真机）：
   - Go 版 `--no-autolaunch` + 假上游，`curl --socks5-hostname` 走通；
   - `bin/start.sh go` / `bin/start.sh python` 启动、日志落盘、`stop.sh` 停干净（无残留子进程）。
5. `docker compose build` 成功，镜像内代理二进制 `--help` 可运行。

## 已否决的备选方案：每 token 专属 tailcat socks 进程池

曾提议为每个 token 启动独立 `tailcat socks` 子进程以"避免每请求重复打洞"。经查阅
tailcat 源码（`cmd/tailcat/tailcat.go`，`clientSOCKSMode`）证实该前提不成立：

- `tailcat socks` 内部维护 `map[tailcat.Addr]*tailcat.Client`，按目标 token 缓存
  Client，隧道只在每个 token 的首个 CONNECT 时建立，后续请求复用已建隧道；
- 打洞在首连后台并行进行，失败自动 DERP 兜底，不阻塞数据传输；
- 所有 token 共享同一客户端身份密钥。

因此单进程模式原生具备"每 token 隧道仅建一次"的特性，进程池仅能省下首连延迟，
却带来 N 倍进程/内存/DERP 连接开销。方案否决，维持单共享上游架构。
（代价说明：每个新 token 的首次 CONNECT 需承担握手延迟，通常几百毫秒，最坏约 15s，
此为 tailcat 侧固有行为，两版本一致。）

## 非目标（YAGNI，与 Python 版一致）

- 不支持 UDP ASSOCIATE / BIND（回失败）。
- 不支持用户名/密码认证。
- 不引入第三方 Go 依赖。
