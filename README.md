# tailcat-proxy

一个带**域名解析**的 SOCKS5 前置代理:把 `curl http://www.example.com/` 里的真实域名,按 `dns.txt` 里的映射改写成 [tailcat](https://github.com/tailscale/tailcat) 的 token(`tc...`),并在**同一进程内**用 tailcat Go 库经 WireGuard/DERP 隧道打到对应机器;**未命中映射的域名与 IP 字面量直接 TCP 拨号**。附带一个 Web 管理页(查看连接、在线编辑 dns.txt)。

你只需对一个固定的 SOCKS5 端口说人话域名,无需记 `tcXYZabc123...` 这种 token,也无需再单独运行 tailcat 可执行文件。

本仓库为 **Go 实现**(`src/`,单静态二进制;tailcat 与 fsnotify 是仅有的库依赖)。

## 它做什么

```
curl http://www.example.com:8888/health
   │  --socks5-hostname 127.0.0.1:1080
   ▼
本代理 (:1080)                          ← 你要用的端口
   ├─ 查 dns.txt: www.example.com → tcXYZabc... (改写主机名,大小写精确保留)
   ├─ 命中   → 进程内 tailcat 库 DialTCPPort → WireGuard/DERP → 那台机器:8888
   ├─ 未命中 → 直接 TCP 拨原 host:port(IP 字面量同样直连)
   └─ 每条连接计数(字节/速率),供管理页展示

管理页 (:8080,可改可关)
   ├─ 当前连接:活跃会话列表 + 汇总(速率由页面按 1s 轮询差值计算)
   └─ DNS 配置:编辑 token/domains/备注,保存即生效
```

关键设计:
- **token 只存在于 `dns.txt` 和代理内部**,从不进 URL。curl 会小写化 URL 主机名,但本设计里 curl 看到的是你给的小写域名,代理换成 `dns.txt` 里大小写混合的真实 token —— 恰好绕开 curl/浏览器小写化 token 的问题。
- **未命中直连**:dns.txt 没命中的域名和 IP 字面量由本进程直接拨号出去。旧的"未命中交给上游 tailcat socks(需 exit-node)"透传路径已随库化删除。
- **隧道 client 复用**:每个 token 一个 `tailcat.Client`(惰性创建 + 缓存);拨号失败后 client 保留(magicsock 端点缓存不丢)。隧道与直连拨号均带 15s 超时(DERP bootstrap + WireGuard 握手重传需要)。
- **客户端身份持久化**:首次启动生成 `data/tailcat-client-key`(JSON,0600)并复用;文件损坏时只 WARN 并用临时身份,绝不覆盖你的文件。

## dns.txt 格式

```
# 示例备注:生产 web 服务器
tcXYZabc123 www.example.com api.example.com
tcDEF456ghi foo.com bar.com
```

- 一行一个 token,后跟任意多个域名(空格/Tab 分隔);`#` 开头行为注释,空行忽略。
- **备注** = token 行紧邻上方的 `#` 行(多行合并为一条备注;与条目之间隔空行的注释是散注释,不算备注)。
- 一个 token 可对应多个域名;一个域名也可出现在多行(最后一条生效)。域名匹配大小写不敏感;token 原样保留大小写。
- **改存即热加载**,无需重启;在管理页保存则**立即生效**(不等防抖)。
- 页面保存会按条目重写整个文件:不属于任何条目的散注释(如示例文件头部说明)会被丢弃——**用页面管理 dns.txt 后,文件就归页面托管**。

## 依赖

- 构建只需要 Go 工具链(tailcat@v0.7.0 要求 **Go 1.27+**;Dockerfile 基于 `golang:1.27.1`)。**不再需要单独安装 tailcat 可执行文件**——它是库依赖。
- 本网络访问 proxy.golang.org 受限时用 `GOPROXY=https://goproxy.cn,direct`。

## 快速开始(裸机)

```bash
# 首次:从示例生成自己的映射文件,填入真实 token(此文件已被 git 忽略,勿提交)
cp data/dns.txt.example data/dns.txt && $EDITOR data/dns.txt

# 首次:配置认证用户。也可以跳过——首次启动会自动生成一份带占位密码的
# 模板,但那两个 changeme 必须改掉再用。
cp data/config.yaml.example data/config.yaml && chmod 600 data/config.yaml
$EDITOR data/config.yaml

# 构建一次(版本号来自 .version)
(cd src && go build -ldflags="-X main.version=$(cat ../.version)" -o ../bin/tailcat-proxy .)

# 前台运行:SOCKS5 127.0.0.1:1080,管理页 http://127.0.0.1:8080/
# 默认从 ./data 读取 dns.txt / config.yaml / tailcat-client-key,并把
# history.db 也写在同一个目录。
bin/tailcat-proxy

# 用起来(必须带凭据)
curl --socks5-hostname 127.0.0.1:1080 --proxy-user tailcat:你的密码 http://www.example.com:8888/health
# 或给整个进程注入

export all_proxy=socks5h://tailcat:你的密码@127.0.0.1:1080
```

### 命令行参数

| 参数 | 默认 | 环境变量回退 | 说明 |
|---|---|---|---|
| `--listen` | `127.0.0.1:1080` | – | 面向客户端的 SOCKS5 监听地址 |
| `--data-path` | `data` | – | 数据目录;下面的文件都从它读,见「数据目录」 |
| `--admin-addr` | `127.0.0.1:8080` | `ADMIN_ADDR` | 管理页/API 监听地址;**空串完全禁用** |
| `--log-level` | `info` | – | 日志级别:`debug` / `info` / `warn` / `error`。**注意**:`debug` 会产生大量 tailscale 内部日志,输出到终端时会拖慢数据路径(实测吞吐从 ~2 MB/s 掉到 ~17 KB/s),排查时请重定向到文件 |
| `--version` | – | – | 打印版本号后退出 |

优先级:**命令行 > 环境变量 > 默认值**。env 只在对应 flag 未显式给出时生效;把 env 设为空串也算"已设置"(用于 `ADMIN_ADDR=""` 禁用页面)。

### 数据目录(`--data-path`)

所有需要读取/写入的文件都固定放在同一个目录里,不再为每个文件单开一个参数:

| 文件 | 说明 |
|---|---|
| `dns.txt` | 域名→token 映射,热加载 |
| `config.yaml` | 认证用户与历史开关;不存在时自动生成模板 |
| `tailcat-client-key` | tailcat 客户端身份(JSON),首启自动生成 |
| `history.db` | SQLite 历史聚合库,自动创建 |

路径会被解析成**绝对路径**后再使用,所以进程工作目录怎么变都不影响:早先每个文件一个 flag 时,一旦换个目录启动,就会出现一半文件读旧位置、一半写新位置,历史库还会悄悄建到别处。

`--data-path` 不存在的子目录会被自动创建(历史库的父目录由 `OpenHistory` 负责)。

### 版本号

版本号**只存在于仓库根的 [`.version`](.version) 文件**。构建时通过 `-ldflags "-X main.version=..."` 注入二进制:

```bash
# 本地构建(与镜像一致的版本号)
(cd src && go build -ldflags="-X main.version=$(tr -d '[:space:]' < .version)" -o ../bin/tailcat-proxy .)
bin/tailcat-proxy --version    # 0.9.0
```

直接 `go build` 不带 ldflags 时版本号是 `dev`,不会去猜。

### 隧道路径:默认强制 DERP

代理启动时默认设置 `TS_DEBUG_ALWAYS_USE_DERP=1`,让隧道流量走 DERP 中继而非 NAT 打洞直连。

原因:在多网卡主机上(bond0 + docker0 + br-* + cali* + flannel.1),magicsock 会把这些接口全部当作候选 endpoint。实测(2026-11-14)打洞成功后会把路径从 DERP "升级"成直连,而这条直连只有 ~17 KB/s,同一 token 走 DERP 则是 1.4–2.7 MB/s —— 一旦升级就不再回落,吞吐永久塌陷。表现是"第一次请求快、之后全慢"。

如需启用直连(例如确认打洞路径良好),显式设置该变量即可,程序不会覆盖:

```bash
TS_DEBUG_ALWAYS_USE_DERP=0 bin/tailcat-proxy
```

## 配置(`<data-path>/config.yaml`)

首次启动时若文件不存在会**自动生成**一份模板并打 WARN。模板里的密码是占位的
`changeme`,**暴露到网络前务必修改**。文件含明文密码,权限应为 0600,已被 git
忽略。配置在启动时读取一次,改完需重启。

```yaml
socks:
  users:
    tailcat: "changeme"      # 只能访问 dns.txt 命中的域名(走隧道)
    all: "changeme"          # 可访问任意目标

history:
  enabled: true              # 历史库固定为 <data-path>/history.db
```

- `socks.users` 是**强制项**:为空、用户名为空、密码为空或空白、用户名不是
  `tailcat` / `all` 之一,都会让启动失败并给出具体原因。用户名决定授权范围,
  所以拼错的名字不能静默通过。
- 历史库的位置不在这里配:所有文件统一由 `--data-path` 决定。写了 `history.path`
  会被 `KnownFields` 拒绝,而不是被静默忽略。

## SOCKS5 认证

代理**强制**要求用户名/密码认证(RFC 1929)——不再接受 `0x00` 无认证方式,
客户端必须同时提供用户名和密码。

| 用户名 | 权限 |
|---|---|
| `tailcat` | **只能**访问 `dns.txt` 命中的域名(走隧道);未命中的目标返回 SOCKS5 `0x02`(connection not allowed by ruleset) |
| `all` | 可访问任意目标:命中走隧道,未命中直连 |

```bash
# curl:用 --proxy-user 带上凭据
curl --socks5-hostname 127.0.0.1:1080 --proxy-user tailcat:密码 http://a.example.com/
curl --socks5-hostname 127.0.0.1:1080 --proxy-user all:密码 http://example.com/
```

认证失败(`0x01 0x01`)与权限拒绝(`0x02`)都不会写入历史,也不会出现在
"当前连接"里——它们从未建立连接。

## 历史连接

每次连接结束后,按(日期, 客户端 IP, 域名, 端口)聚合一行写入
`data/history.db`(SQLite,WAL 模式):当日累计次数、失败次数、上下行字节。
"失败"指拨号失败或未进入转发阶段;认证失败与权限拒绝不计入。

管理页的**历史连接**标签可按日期查看,列表按累计流量倒序。该标签不轮询——
历史只在连接结束时变化,切标签或改日期时拉取一次。

历史是**旁路功能**:`history.enabled: false` 或数据库打不开时,功能降级为
不可用(页面显示"历史记录未启用"),代理继续正常服务,启动日志有 WARN。

## Web 管理页

- **当前连接**:汇总条(活跃数、累计会话、上/下行合计、聚合速率)+ 每条连接:原始域名、端口、tunnel/direct 徽标、状态(handshake/open)、客户端地址、时长、上/下行字节与速率。速率由页面对相邻两次 1s 采样求差得到;已关闭的连接不保留历史。
- **历史连接**:按日期筛选当日聚合,按累计流量倒序;汇总卡给出条目数、总次数、失败次数与上下行合计。
- **DNS 配置**:表格编辑 token / domains(逗号或空格分隔)/ 备注;"保存并生效"经服务端校验 → 原子落盘 → 立即载入映射,失败会给出条目级原因且文件与映射都不动。
- 页面**无鉴权**,默认只绑 127.0.0.1,信任本机;要对宿主机/网络开放请自行加反代或防火墙(见 Docker 一节)。

## 启停脚本(bin/)

后台运行,pid 与 log 在 `run/tailcat-proxy-go.{pid,log}`:

```bash
bin/start.sh          # 后台启动(缺二进制时自动 go build)
bin/stop.sh           # 停止
bin/restart.sh        # 重启
tail -f run/tailcat-proxy-go.log
```

脚本只停止确属本代理的进程(校验 /proc pid 归属);start 后若进程秒退(如 dns 文件不存在、端口被占)会直接报错并以非零码退出,并打印日志尾部。

脚本自身认的环境变量:`LISTEN`、`DATA_PATH`、`PROXY_ARGS`(经 shell 分词,适合放多个额外 flag);`ADMIN_ADDR` 由二进制直接读取。例:`LISTEN=127.0.0.1:1080 ADMIN_ADDR=127.0.0.1:8080 bin/start.sh`

## Docker

```bash
cd docker
docker compose up --build -d       # 构建并后台启动,暴露 1080
docker compose logs -f
docker compose down
```

- compose 把仓库的 `data/` 目录挂进容器 `/data`(`DATA_PATH=/data`),dns.txt、config.yaml、client key、history.db 都在那里。改宿主机 `data/dns.txt` 即热加载(挂目录而非单文件,规避编辑器 rename 换 inode 导致的热加载失效)。
- 管理页端口映射到宿主机**回环地址**(`127.0.0.1:8080:8080`)——页面无鉴权,只让本机访问。要对网络开放请自行加反代或防火墙,并改映射为 `8080:8080`。
- 容器内 `LISTEN=0.0.0.0:1080`;管理页默认 `127.0.0.1:8080`(**容器内**,宿主机访问不到)。要开放:同时取消 `ADMIN_ADDR: "0.0.0.0:8080"` 与 `8080:8080` 端口映射的注释——注意这等于把无鉴权页面暴露给宿主机网络。
- tailcat 客户端身份存在命名卷 `tailcat-client-key:/data`,容器重建不丢身份。
- 容器需能出网(DERP/STUN)。
- 镜像 runtime 为 `alpine:3.22`(全静态二进制不需要 glibc),**只含一个可执行文件**(tailcat 是库依赖);已发布为 `yaofeng928/tailcat-proxy:latest`。
- 手动构建(等价于 compose):在仓库根目录 `docker build -f docker/Dockerfile -t tailcat-proxy .`——context 必须是仓库根,不要写成 `-f docker/Dockerfile ..`(那会把父目录当 context)。

## dns.txt 热加载

代理监听 `dns.txt` 变化即重新载入(原子替换内存映射):用 fsnotify 文件事件,并在每次 Create/Write 后重新挂载 watch(规避原子保存换 inode)。读取失败时保留旧映射并打警告。改完文件存盘即生效,无需重启(热加载始终开启)。管理页保存走的是"写完立即重载"路径,不受 50ms 防抖影响;与外部编辑器并发是 last-writer-wins。

## 目录结构

```
tailcat-proxy/
├── src/                              # Go 版(idiomatic Go;单二进制)
│   ├── main.go                       # flag/env/信号/退出顺序
│   ├── dnsmap.go                     # dns.txt 条目解析 + 原子热加载
│   ├── dialer.go                     # 路由决策 + 进程内 tailcat 库 + 直连拨号
│   ├── stats.go                      # ConnRegistry 会话表 + 原子字节计数
│   ├── proxy.go                      # SOCKS5 服务端 + RFC1929 认证 + 授权 + relay
│   ├── auth.go                       # 用户表、摘要比较、按用户授权范围
│   ├── config.go                     # config.yaml 加载与校验
│   ├── history.go                    # SQLite 历史聚合(按日/IP/域名/端口)
│   ├── admin.go                      # 管理页 HTTP API(go:embed + 单页)
│   ├── static/index.html             # 管理页(编译期内嵌)
│   └── *_test.go                     # 单元 + 本地端到端(全离线)
├── bin/                              # 启停脚本 {start,stop,restart}.sh + 构建产物 tailcat-proxy(gitignore)
├── docker/{Dockerfile,docker-compose.yml}
├── docs/superpowers/                 # Go 版设计文档与实现计划
├── data/
│   ├── dns.txt                       # 域名→token 映射(运行时热加载,gitignore)
│   ├── dns.txt.example               # 示例映射
│   ├── config.yaml                   # 认证用户与历史配置(含明文密码,gitignore)
│   ├── config.yaml.example           # 示例配置
│   ├── history.db                    # 历史聚合库(自动创建,gitignore)
│   └── tailcat-client-key            # tailcat 客户端身份(首启自动生成,gitignore)
└── run/                              # pid + log(脚本生成)
```

## 实现范围

- **支持**: SOCKS5 `CONNECT`;IPv4/IPv6/域名 ATYP;**强制用户名/密码认证(RFC 1929)**;按用户授权(`tailcat` 只走命中隧道、`all` 不限);未命中(含 IP)直连;进程内 tailcat 隧道与 client 复用;连接统计与管理页;**SQLite 历史聚合**;dns.txt 页内编辑 + 热加载;多线程并发。
- **不支持**(YAGNI): `UDP ASSOCIATE` / `BIND`(返回失败);配置热加载;Web 鉴权;多日区间查询与图表;历史数据清理/归档;exit-node 透传(由直连语义替代)。

## 测试

```bash
cd src && go test ./...             # 全离线:本地回环 + 注入 fake,无外网依赖
cd src && go test ./... -race
```

覆盖 dns.txt 条目解析/校验/原子保存、路由决策、注册表计数、SOCKS5 改写与 relay(含半关不 RST)、RFC1929 认证与按用户授权、admin API 契约、配置加载与校验、SQLite 历史聚合(含并发写入与 DSN 处理)。真实隧道 e2e 依赖外网 DERP,手工清单:起服务 → 配一个可达 token → `curl --socks5-hostname --proxy-user tailcat:密码` 打过去 → 管理页出现该连接且字节/速率增长 → 页内改映射立即生效 → 历史标签出现当日记录。
