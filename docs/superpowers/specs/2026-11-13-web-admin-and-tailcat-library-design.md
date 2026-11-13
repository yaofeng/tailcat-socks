# 设计：Web 管理页面 + tailcat 库化 + 未命中直连

日期：2026-11-13
状态：已与用户逐节确认
前置：本仓库已仅保留 Go 实现（`src/`），dns 映射位于 `data/dns.txt`

## 1. 目标

1. 内嵌 Web 管理页（默认 `127.0.0.1:8080`，flag/env 可改），两个 Tab：
   - **当前连接**：活跃连接列表（域名、速率、可得的其他信息）+ 汇总；
   - **DNS 配置**：页面编辑 dns.txt（token / domains / 备注），保存即热生效；备注以独立 `#` 注释行落盘。
2. 未命中 dns.txt 的目标（普通域名与 IP 字面量）**直接 TCP 拨号**，不再交给上游；仅命中的域名走 tailcat 隧道。
3. tailcat 不再是子进程：直接引用 `github.com/tailscale/tailcat` 库在进程内拨号。Dockerfile 与 compose、启停脚本、README 对应调整。

## 2. 非目标

- 已关闭连接的历史/图表（`sessionsTotal` 只回答"总共多少"）；
- WebSocket 推送、前端框架与构建链（1s 轮询 + vanilla JS 足够）；
- Web 鉴权（默认绑 127.0.0.1，信任本机）；
- exit-node 透传能力（原"未命中交上游、服务器开 exit-node"的路径随库化删除，由直连语义替代）；
- UDP ASSOCIATE/BIND（维持现状返回失败）。

## 3. 架构总览

```
tailcat-socks（单进程，单二进制）
 ├─ SOCKS5 服务 :1080（现状保留）
 │    serveConn → route(host) ─┬→ Dialer.Tunnel(token)  = tailcat.Client.DialTCPPort（进程内，库）
 │                             └→ Dialer.Direct(host)   = net.Dialer 普通 TCP
 │    relay(idleConn + 原子计数) → ConnRegistry
 └─ admin HTTP :8080（新增，可关）
      GET  /                    go:embed 单页（两个 Tab）
      GET  /api/connections     registry 快照 JSON
      GET  /api/dns             解析 data/dns.txt → entries
      PUT  /api/dns             校验 → 原子重写文件 → 立即 reload 进 DNSMap → 200
```

新文件：`src/dialer.go`（路由+拨号）、`src/admin.go`（HTTP API+registry+embed）、`src/static/index.html`（页面）。`main.go` 删 autolaunch 整块；`proxy.go` 换拨号路径；`dnsmap.go` 增 entries 读写。

## 4. tailcat 库化

- 用法对齐官方 CLI socks 模式（`tailcat@v0.7.0` 实测）：`map[tailcat.Addr]*tailcat.Client` + mutex 惰性创建；拨号 `clientForAddr(token).DialTCPPort(ctx, port)`；拨号 ctx 独立挂 **15s** 超时（DERP bootstrap + WireGuard 握手重传需要，官方同款理由）。
- 客户端身份：持久 private key，默认 `data/tailcat-client-key`（flag `--client-key-file`，env `TAILCAT_CLIENT_KEY_FILE`；首启动生成并落盘 0600，存在则加载）。读/写失败 → 进程内临时 key + WARN（隧道仍可用，身份不持久）。
- `tailcat.Logf` 桥接 slog（Debug 级）。进程退出遍历 clients `Close()`。
- 依赖代价：tailscale.com / gvisor netstack / wireguard 进入 go.mod，二进制约 4.4MB → 30MB+；仍纯 Go、CGO=0 静态，alpine 运行镜像不受影响。
- 删除：`--upstream`、`--tailcat-bin`、`--no-autolaunch`、autolaunch/端口探测/子进程回收、`dialUpstream`/`socksClientConnect`/`socksConnectRequest`、`testdata/fake-tailcat`。

## 5. 路由语义（serveConn）

握手解析出 `host:port` 后：

| 目标 | via | 拨号 |
|---|---|---|
| 命中 dns.txt（大小写不敏感） | `tunnel` | token 原样（保留大小写）交 tailcat Client |
| 未命中域名 | `direct` | `net.Dialer` 直连 host:port |
| IP 字面量（v4/v6） | `direct` | 同上，零解析开销 |

拨号成功 → 直接写 `socksOKReply`，进入 relay；失败 → `socksFailReply`，Session 不进 totals，Debug 日志含 via/原 host。不重试（SOCKS 语义交客户端）。

## 6. 连接统计采集

- `Session{ID, Client, Host, Target, Port, Via, Opened, Started, BytesUp, BytesDown atomic.Uint64}`；`ConnRegistry{mu, map[uint64]*Session, nextID, sessionsTotal}`。锁仅出现在注册/注销（每连接一次）与 API 快照（每秒一次）；数据路径只有原子计数。
- 计数挂在现有 `idleConn`：新增可选 `count *atomic.Uint64`，`Read n>0` 时 `Add(n)`。方向天然正确：读客户端腿=上行、读隧道/直连腿=下行。不新增包装层（保持"每字节必经 idleConn.Read"约定）。
- 生命周期：accept 不注册（防半开/扫描污染）→ CONNECT 解析+路由确定后注册（Opened=false）→ 回执成功进入 relay 时 Opened=true、sessionsTotal++ → `serveConn` defer 统一 Drop。
- 速率：服务端只报累计字节；前端记住上次 (t, bytes) 求差，每 1s 轮询。连接消失即停采样，无历史。

## 7. dns.txt 条目化

```
# 生产 web 服务器            ← 备注 = token 行紧邻上方的 # 行组（多行合并为一行）
tcXYZabc123 www.example.com api.example.com
tcDEF456ghi foo.com
```

- `type Entry{Token string; Domains []string; Note string}`；`LoadDNSEntries(path)`：连续 `#` 行作 pending note，遇 token 行成条；token 行仍用 `strings.Fields`（行尾 `#` **无**特殊语义——UI 校验拒绝含 `#` 的 domain）。旧文件（无注释）兼容；`LoadDNSFile`/`DNSMap`/`RewriteHost`/`WatchDNSFile` 不动，映射仍由 entries 派生（后行覆盖前行）。
- `SaveDNSEntries(path, entries)`：temp+rename 原子写；note 非空先写 `# note`（压掉换行）。**文件由 UI 托管**：不属于任何 entry 的散注释（示例文件头部说明等）保存时丢弃。
- `PUT /api/dns`：校验（token 非空、不以 `#` 开头；domains 非空、不含 `#`、不含空白；note 单行可空；token 无 `tc` 前缀仅告警）→ `SaveDNSEntries` → 立即 `reload()` 进 DNSMap（不等 fsnotify 50ms 防抖，"保存即生效"）→ 200 `{domains, tokens}`。写失败 500、映射不动。
- 与外部编辑器并发：last-writer-wins；UI 打开/保存前重新 GET 最新内容。

## 8. HTTP API 契约

`GET /api/connections`：

```json
{ "active": [ { "id": 12, "client": "127.0.0.1:53211",
    "host": "www.example.com", "target": "tcXYZabc",
    "port": 8888, "via": "tunnel", "state": "open",
    "startedAt": 1699876543210, "bytesUp": 10240, "bytesDown": 999234 } ],
  "totals": { "active": 3, "sessionsTotal": 57,
    "bytesUp": 0, "bytesDown": 0 } }
```

`state`: `handshake`（已注册未回执）| `open`。`GET /api/dns`：`{ "path": "data/dns.txt", "entries": [...] }`（path 为当前实际读取路径）。`PUT /api/dns` 体同 entries 数组；错误 `400 {"error": "行2: domain 含 #"}`。

`--admin-addr`（env `ADMIN_ADDR`，flag > env > 默认 `127.0.0.1:8080`；空串禁用全部 admin 功能，页面与 API 都不监听）。`http.Server` 设 `ReadHeaderTimeout`；绑定失败记 ERROR 后**退出**（fail-fast）。

## 9. 页面（src/static/index.html，go:embed）

- 两个 Tab，纯 vanilla JS + fetch，1s `setInterval` 轮询 connections。
- Tab1 当前连接：汇总条（活跃数、累计会话、上/下行总量与聚合速率）+ 表格（原域名、目标、端口、tunnel/direct 徽标、客户端、时长、上/下行与速率）。
- Tab2 DNS 配置：可编辑表（token | domains 逗号分隔 | 备注 | 删除）+ 新增行 + 保存；成功显示计数与"已生效"；失败显示服务端 error。

## 10. 错误处理与退出顺序

- SOCKS listener 绑定失败 → 退出（现状不变）。
- client key 读写失败 → 临时 key + WARN。
- admin 编码异常 → 500 + ERROR（无远端依赖，理论路径）。
- tailcat client 拨号失败后保留复用（magicsock 端点缓存不丢）。
- 退出（ctx 驱动，复用现有 signal 框架与"二次 Ctrl-C 被吞"防护）：SOCKS listener 关 → 在途 relay 自然收尾 → admin `Shutdown(1s)` → clients `Close()`。

## 11. 部署面变化

- `docker/Dockerfile`：删 build 阶段 `go install tailcat` 层、删 `COPY --from=build /go/bin/tailcat`、删 `TAILCAT_BIN` env；CMD 去掉 `--tailcat-bin` 并追加 `--admin-addr`（默认容器内 `127.0.0.1:8080`，不对宿主机暴露）。
- `docker/docker-compose.yml`：删 `TAILCAT_BIN`；注释写明——宿主机要访问页面需设 `ADMIN_ADDR=0.0.0.0:8080` 并加 `8080:8080` 映射（默认只绑容器内 localhost）。
- `bin/start.sh`：删 `TAILCAT_BIN` 透传与 autolaunch 相关 env；`--dns-file` 逻辑不变。
- README：功能、参数表、页面说明、镜像说明、目录结构、测试命令全部按新行为改写。
- `.gitignore` 增 `data/tailcat-client-key`。

## 12. 测试计划（离线，无真实 DERP）

1. `dialer_test`：route 决策纯函数单测（命中/未命中/IP 字面量/大小写不敏感），断言 `Target` 各字段取值；原 `socksConnectRequest` 的 FQDN>255 拒发逻辑随库化删除，direct 路径交 `net.Dialer` 处理，不再单独设限。
2. `proxy_test`：注入 `fakeDialer`（tunnel/direct 各接本地 echo TCP）——断言 tunnel 目标==token、direct 目标==原 host、失败→socksFailReply、Session 字节计数、半关不 RST（沿用现有断言）。
3. `dnsmap_test`：entries 往返（多行 # 合并、散注释丢弃、旧格式兼容、含 # 的 domain 校验拒绝）+ PUT 后 `DNSMap.Load()` 立变。
4. `admin_test`（httptest）：connections 结构与计数、totals 累计、PUT 非法→400、`admin-addr=""` 不监听。
5. 冒烟：`bin/start.sh` 后 curl `GET /api/connections`；真实隧道 e2e 手工清单进 README（依赖外网 DERP）。
