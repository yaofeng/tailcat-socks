# tailcat-dns-proxy

一个带**域名解析**的 SOCKS5 前置代理:把 `curl http://www.example.com/` 里的真实域名,按 `dns.txt` 里的映射改写成 [tailcat](https://github.com/tailscale/tailcat) 的 token(`tc...`),再转交给 `tailcat socks` 通过 WireGuard/DERP 隧道打到对应机器。

你只需对一个固定的 SOCKS5 端口说人话域名,无需记 `tcXYZabc123...` 这种 token,也无需手动起 tailcat。

## 它做什么

```
curl http://www.example.com:8888/health
   │  --socks5-hostname 127.0.0.1:1080
   ▼
本代理 (:1080)                          ← 你要用的端口
   ├─ 查 dns.txt:  www.example.com → tcXYZabc...  (改写主机名,大小写精确保留)
   ├─ 自动拉起 →  tailcat socks --listen=127.0.0.1:<随机高位端口>   ← 内部,自动
   └─ 以 SOCKS5 客户端身份,把改写后的 host 转发给该上游
                                        ▼
                          tailcat socks → WireGuard/DERP 打洞 → token 对应机器:8888
```

关键设计:
- **token 只存在于 `dns.txt` 和代理内部**,从不进 URL。curl 会小写化 URL 主机名,但本设计里 curl 看到的是你给的小写域名,代理换成 `dns.txt` 里大小写混合的真实 token —— 恰好绕开 curl/浏览器小写化 token 的问题。
- **一个上游服务所有 token**。`tailcat socks` 独立运行时按每个 CONNECT 的主机名路由:主机名是合法 `tc` 地址就直接打洞到那台机器,不依赖命令行参数。所以 Python 代理只需拉起一个固定的 `tailcat socks`,把改写后的 token 当主机名丢过去即可,无需为每个 token 各起一个。
- **`tailcat socks` 自动拉起、自动回收**。代理启动时 spawn 子进程,端口就绪后才开始服务;收到 SIGINT/SIGTERM 或退出时回收子进程,不留孤儿。

## dns.txt 格式

一行一个 token,后跟它对应的任意多个域名(空格/Tab 分隔),`#` 开头为注释,空行忽略:

```
# token          domains (可多个)
tcXYZabc123      www.example.com  api.example.com
tcDEF456ghi      foo.com  bar.com
tcoAbCdEf789     www.example.com
```

- 一个 token 可对应多个域名;一个域名也可出现在多行(最后一条生效)。
- 域名匹配大小写不敏感;token 原样保留大小写。
- **未命中 `dns.txt` 的域名原样透传**给上游。注意:standalone 上游没绑 exit-node,普通域名会连不上;要让普通域名走出口节点,需把上游换成 `tailcat socks <addr> ...` 且服务器开 `--serve=exit-node`。
- **改存即热加载**,无需重启(见下)。

## 安装依赖

- Python 3.8+(仅标准库,无第三方依赖)
- `tailcat`:https://github.com/tailscale/tailcat(`brew install tailcat` 或 `go install github.com/tailscale/tailcat/cmd/tailcat@latest`)

## 快速开始(裸机)

```bash
# 首次:从示例生成自己的映射文件,填入真实 token(此文件已被 git 忽略,勿提交)
cp dns.txt.example dns.txt && $EDITOR dns.txt

# 前台运行(默认: 监听 127.0.0.1:1080, dns.txt 在仓库根, tailcat socks 用随机高位端口)
python3 src/tailcat_dns_proxy.py

# 指定各参数
python3 src/tailcat_dns_proxy.py \
    --listen 127.0.0.1:1080 \
    --dns-file dns.txt \
    --upstream 127.0.0.1:1081 \
    --tailcat-bin tailcat

# 用起来
curl --socks5-hostname 127.0.0.1:1080 http://www.example.com:8888/health
# 或给整个进程注入
export all_proxy=socks5h://127.0.0.1:1080
```

### 命令行参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `--listen` | `127.0.0.1:1080` | 面向客户端的 SOCKS5 监听地址 |
| `--dns-file` | `dns.txt` | 域名→token 映射文件 |
| `--upstream` | `127.0.0.1:0` | 自动拉起的 `tailcat socks` 监听地址;端口 `0` = 随机高位端口 |
| `--tailcat-bin` | `tailcat` | tailcat 可执行文件路径 |
| `--no-autolaunch` | 关 | 不自动拉起,连一个已在运行的上游 SOCKS5 |
| `--no-watch` | 关 | 关闭 dns.txt 热加载 |

## 启停脚本(bin/)

后台运行,`run/` 下维护 pid 与 log:

```bash
bin/start.sh      # 后台启动
bin/stop.sh       # 停止(回收 tailcat 子进程)
bin/restart.sh    # 重启
tail -f run/tailcat-dns-proxy.log
```

配置用环境变量(可选):`LISTEN`、`DNS_FILE`、`UPSTREAM`、`TAILCAT_BIN`、`PROXY_ARGS`。
例:`LISTEN=127.0.0.1:1080 UPSTREAM=127.0.0.1:1081 bin/start.sh`

## Docker

```bash
cd docker
docker compose up --build -d       # 构建并后台启动,暴露 1080
docker compose logs -f
docker compose down
```

- compose 把仓库根目录只读挂进容器 `/app/config`,`DNS_FILE=/app/config/dns.txt`。改宿主机 `dns.txt` 即热加载(挂目录而非单文件,规避编辑器 rename 换 inode 导致的热加载失效)。
- 容器内 `LISTEN=0.0.0.0:1080`,tailcat socks 仍用内部随机高位端口。宿主机经 `1080:1080` 访问。
- 容器需能出网(DERP/STUN)。

## dns.txt 热加载

代理默认每秒轮询 `dns.txt` 的 mtime,变化即重新载入(原子替换内存映射)。读取失败时保留旧映射并打警告。改完文件存盘即生效,无需重启;`--no-watch` 关闭。

## tailcat socks 端口

默认 `--upstream` 端口为 `0`,代理从 `20000–60999` 随机挑一个空闲的高位端口给 tailcat socks。也可 `--upstream 127.0.0.1:1081` 固定。

## 目录结构

```
tailproxy/
├── src/tailcat_dns_proxy.py      # 代理主体(纯标准库)
├── tests/test_tailcat_dns_proxy.py
├── bin/{start,stop,restart}.sh   # 裸机后台启停
├── docker/{Dockerfile,docker-compose.yml}
├── dns.txt                       # 域名→token 映射(运行时热加载)
└── run/                          # pid + log(脚本生成)
```

## 实现范围

- **支持**: SOCKS5 `CONNECT`;IPv4/IPv6/域名 ATYP;无认证握手;未命中域名透传;多线程并发;tailcat 子进程自动拉起/回收;dns.txt 热加载。
- **不支持**(YAGNI): `UDP ASSOCIATE` / `BIND`(返回失败);用户名/密码认证;跨进程共享映射。

## 测试

```bash
python3 tests/test_tailcat_dns_proxy.py     # 或 python3 -m pytest tests -v
```
覆盖 dns.txt 解析、大小写改写、未命中透传、热加载,以及经假上游的端到端 host 改写。
