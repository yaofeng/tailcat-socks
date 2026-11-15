# 设计：监听地址迁入配置 + 新增 HTTP 代理

日期：2026-11-15
状态：已与用户确认要点，待审阅
前置：`0.10.0` 已实现 SOCKS5 认证授权、SQLite 历史、管理页（见 `2026-11-14-socks-auth-and-sqlite-history-design.md`）

## 1. 目标

1. **监听地址从命令行搬进 `config.yaml`**，三个入口：

   ```yaml
   listen:
     http-proxy: "0.0.0.0:7890"    # HTTP/HTTPS CONNECT 代理
     socks-proxy: "0.0.0.0:1080"   # SOCKS5 代理
     web-console: "127.0.0.1:8080" # 管理页
   ```

   没有 `config.yaml` 时，自动生成的文件里写入上述默认值。

2. **新增 HTTP 代理**：`http-proxy` 监听一个 HTTP 代理端口，支持 `CONNECT` 隧道与普通 HTTP 转发，认证与授权规则与 SOCKS5 **完全一致**。

3. **删掉 `--listen` 与 `--admin-addr`**：监听地址只从配置文件来，`ADMIN_ADDR` 环境变量一并移除。

## 2. 非目标

- HTTP 代理的缓存、请求改写、URL 过滤；
- 配置热加载（监听地址改了需重启）；
- 每个入口独立的用户表（共用一份）；
- 历史里区分协议（见 §6 的说明）。

## 3. 配置结构

```yaml
listen:
  http-proxy: "0.0.0.0:7890"
  socks-proxy: "0.0.0.0:1080"
  web-console: "127.0.0.1:8080"

users:
  tailcat: "changeme"
  all: "changeme"

history:
  enabled: true
```

- 三项都是**字符串**，`host:port` 形式；**空串表示不监听该入口**（可以只用 SOCKS5）。
- 默认值如上表，写在 `configTemplate` 里；文件不存在时生成并落盘。
- `web-console` 为空 → 管理页与 `/api/*` 全部不启动（与现在 `--admin-addr ""` 的语义一致）。
- 启动时校验每个非空地址：`net.SplitHostPort` 通得过、端口是 0–65535 的数字。校验失败 ERROR + exit 1，报出是哪个入口。**不做"猜一个默认值"的兜底**。

### 为什么删掉 env 回退

监听地址是部署拓扑，写死在配置文件里才能一眼看全；env 只能覆盖其中一部分（原来只有 `ADMIN_ADDR`），容易造成"我改了 env 但配置文件里的值仍在生效"的困惑。删掉 `--listen`/`--admin-addr`/`ADMIN_ADDR` 后，监听地址只有一个来源。

## 4. HTTP 代理

### 4.1 协议范围

只做两种请求，其余一律 `501 Not Implemented`：

| 请求 | 处理 |
|---|---|
| `CONNECT host:port HTTP/1.1` | 建立隧道，回 `200 Connection Established`，然后原样双向转发字节 |
| 绝对 URI 的普通请求（`GET http://host/path HTTP/1.1`） | 拨号到 host:port，把请求行改写成 origin-form（`GET /path`）后转发，响应原样回传 |

`CONNECT` 是主路径（HTTPS 全靠它）；普通 HTTP 转发只做最小实现，不解析、不缓存、不改写响应。

### 4.2 认证

`Proxy-Authorization: Basic base64(user:pass)`，用现有的 `Users.Authenticate`：

- 缺失或格式错误 → `407 Proxy Authentication Required` + `Proxy-Authenticate: Basic realm="tailcat-proxy"`；
- 凭据错误 → 同样 407（不区分"用户不存在"与"密码错误"）。

### 4.3 授权

与 SOCKS5 完全一致，复用 `route()` 与 `Mode.Allows()`：

- 目标命中 dns.txt → `ViaTunnel`；
- 未命中 → `ViaDirect`；
- `tailcat` 用户请求未命中目标 → **`403 Forbidden`**（HTTP 里 0x02 的等价物），不拨号、不记历史。

HTTP 没有 SOCKS 的 reply 码，用状态码表达同样的语义：407=认证、403=策略拒绝、502=上游拨号失败。

### 4.4 复用

把 SOCKS5 `serveConn` 里「路由 → 授权 → 注册 → 拨号 → relay → 记历史」抽成一个共用函数，两个代理各自只负责协议解析与回包：

```go
// serveTunnel 是两个入口共用的一条已授权连接的完整生命周期。
// 它拥有 up 与 client 两条连接（负责关闭），并在结束时写一条历史。
func (s *Server) serveTunnel(client net.Conn, host string, port uint16, user User) 
```

这样 SOCKS5 与 HTTP 的授权、计数、历史不会有第二套实现，也就不会出现两边规则不一致。

### 4.5 新增文件与类型

`src/httpproxy.go` + `src/httpproxy_test.go`：

```go
// HTTPProxy 是 HTTP/HTTPS CONNECT 入口。它与 SOCKS5 入口共享路由、用户表、
// 历史与 relay，只负责 HTTP 语义的解析与回包。
type HTTPProxy struct {
	srv *Server   // 复用 DNSMap / Dialer / Registry / Users / History
	ln  net.Listener
}
```

## 5. 启动与退出

`main.go`：

1. 加载配置（含三个监听地址）；
2. 分别 `net.Listen`：SOCKS5、HTTP、admin。**任一个绑定失败都是 fail-fast** —— 端口被占是部署错误，静默少起一个入口比报错更糟；
3. 至少要有**一个**代理入口（socks 或 http）非空且绑定成功，否则 ERROR + exit 1（只有管理页的配置没有意义）；
4. 退出顺序：watcher 停 → 两个代理 listener 关 → admin `Shutdown(1s)` → tailcat clients 关。

## 6. 历史记录

HTTP 代理的连接与 SOCKS5 写进**同一张表、同一套规则**：

- 成功/失败的连接各记一次，`client_ip` 剥端口；
- 认证失败（407）与授权拒绝（403）**不记**——它们从未建立连接，与 SOCKS5 的 0x01/0x02 一致；
- 不在表里加 `protocol` 列：用户要的是"哪台客户端访问了哪个目标多少次"，协议不是聚合维度。若将来要区分，加列是可加的改动。

## 7. 测试计划（全离线）

1. **`config_test`**：三个 listen 字段解析；默认模板含三个值；空串合法；某字段格式非法报错；缺 `listen:` 段时用默认值。
2. **`httpproxy_test`**：
   - 无 `Proxy-Authorization` → 407；
   - 错误凭据 → 407；
   - `tailcat` 用户 `CONNECT` 未命中目标 → 403，且**不拨号**（用 fakeDialer 断言）；
   - `all` 用户 `CONNECT` 未命中目标 → 200 + 隧道可用（回显断言）；
   - `tailcat` 用户 `CONNECT` 命中目标 → 200 + 路由为 tunnel；
   - 普通 HTTP 绝对 URI 请求被改写成 origin-form 并转发；
   - 非 CONNECT/非绝对 URI → 501；
   - 连接结束写入历史（用真实 `:memory:` 或临时库断言）。
3. **`proxy_test`**：现有 SOCKS5 用例全部保持通过（重构 `serveTunnel` 后不能有回归）。
4. **`main` 冒烟**：配置文件三个入口，起服务后分别 curl SOCKS5、`curl -x http://...`、管理页，三处都要通。

## 8. 部署面

- **Dockerfile**：`EXPOSE` 增加 7890；CMD 去掉 `--listen`/`--admin-addr`，只留 `--data-path`。
- **compose**：端口增加 `7890:7890`；删除 `LISTEN`/`ADMIN_ADDR` 环境变量（地址在 config.yaml 里）；注意 `web-console: 127.0.0.1:8080` 在容器内只绑回环，宿主机要访问仍需把配置改成 `0.0.0.0:8080`。
- **`bin/start.sh`**：去掉 `LISTEN` 环境变量与 `--listen`，只传 `--data-path`。
- **README**：监听配置、HTTP 代理用法（`curl -x http://user:pass@host:7890`）、参数表删掉 `--listen`/`--admin-addr`。
- **`.version`** → `1.0.0`（新增 HTTP 代理入口；监听地址从 CLI 迁入配置，属破坏性变更，故一并宣告 1.0）。

## 9. 兼容性

这是**破坏性变更**：现有 `--listen` / `--admin-addr` / `ADMIN_ADDR` 全部失效，旧的 `config.yaml`（没有 `listen:` 段）会用默认值启动——默认 socks 在 `0.0.0.0:1080`、web-console 在 `127.0.0.1:8080`，与之前裸机默认一致；但 `--listen 0.0.0.0:1080` 这种显式用法会因未知 flag 直接报错，这正是期望的（让用户去看新配置）。
