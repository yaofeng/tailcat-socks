# tailcat-socks

一个带**域名解析**的 SOCKS5 前置代理:把 `curl http://www.example.com/` 里的真实域名,按 `dns.txt` 里的映射改写成 [tailcat](https://github.com/tailscale/tailcat) 的 token(`tc...`),再转交给 `tailcat socks` 通过 WireGuard/DERP 隧道打到对应机器。

你只需对一个固定的 SOCKS5 端口说人话域名,无需记 `tcXYZabc123...` 这种 token,也无需手动起 tailcat。

本仓库包含行为一致的**三个实现**:`python/`(原版,纯标准库)、`go/`(零第三方依赖,单静态二进制)与 `rust/`(tokio 生态)。Docker 镜像基于 Go 版构建。

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
- **一个上游服务所有 token**。`tailcat socks` 独立运行时按每个 CONNECT 的主机名路由:主机名是合法 `tc` 地址就直接打洞到那台机器,不依赖命令行参数。所以代理只需拉起一个固定的 `tailcat socks`,把改写后的 token 当主机名丢过去即可,无需为每个 token 各起一个。
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

- Go 版:Go 1.23+(构建用;或直接用已构建的 `go/bin/tailcat-socks`)
- Python 版:Python 3.8+(仅标准库)
- Rust 版:Rust 1.85+(构建用;或直接用已构建的 `rust/target/release/tailcat-socks`)
- `tailcat`:https://github.com/tailscale/tailcat(`brew install tailcat` 或 `go install github.com/tailscale/tailcat/cmd/tailcat@latest`;注意 tailcat@latest 需要 Go 1.27+ 工具链)

## 快速开始(裸机)

```bash
# 首次:从示例生成自己的映射文件,填入真实 token(此文件已被 git 忽略,勿提交)
cp dns.txt.example dns.txt && $EDITOR dns.txt

# 前台运行(默认: 监听 127.0.0.1:1080, dns.txt 在仓库根, tailcat socks 用随机高位端口)

# Go 版(推荐:先构建一次)
cd go && go build -o bin/tailcat-socks . && cd ..
go/bin/tailcat-socks

# Rust 版(推荐:先构建一次)
cd rust && cargo build --release && cd ..
rust/target/release/tailcat-socks

# Python 版
python3 python/tailcat_socks.py

# 指定各参数(三版本参数同名同默认,下以 Go 版为例)
go/bin/tailcat-socks \
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

后台运行,`run/` 下按版本维护 pid 与 log(`run/tailcat-socks-{go,python,rust}.{pid,log}`):

```bash
bin/start.sh          # 后台启动 Go 版(缺二进制时自动 go build)
bin/start.sh python   # 后台启动 Python 版
bin/start.sh rust     # 后台启动 Rust 版
bin/stop.sh           # 停止(不带参数停全部版本;bin/stop.sh go 只停 Go 版)
bin/stop.sh rust      # 只停 Rust 版
bin/restart.sh        # 无参数 = 停全部 + 启 Go 版;bin/restart.sh python 重启 Python 版
bin/restart.sh rust   # 重启 Rust 版
tail -f run/tailcat-socks-go.log      # 或 ...-python.log / ...-rust.log
```

脚本只停止确属本代理的进程(校验 /proc pid 归属);start 后若进程秒退(如 dns 文件不存在、端口被占)会直接报错并以非零码退出,并打印日志尾部。

配置用环境变量(可选):`LISTEN`、`DNS_FILE`、`UPSTREAM`、`TAILCAT_BIN`、`PROXY_ARGS`(会经 shell 分词,适合放多个额外 flag)。
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
- 镜像 runtime 为 `alpine:3.22`(全静态二进制不需要 glibc),内含 Go 静态编译的代理二进制与 tailcat,不再依赖 Python;已发布为 `yaofeng928/tailcat-socks:latest`。
- 手动构建(等价于 compose):在仓库根目录 `docker build -f docker/Dockerfile -t tailcat-socks .`——context 必须是仓库根,不要写成 `-f docker/Dockerfile ..`(那会把父目录当 context)。

## dns.txt 热加载

代理监听 `dns.txt` 变化即重新载入(原子替换内存映射):Go 版用 fsnotify 文件事件,Python/Rust 版每秒轮询 mtime。读取失败时保留旧映射并打警告。改完文件存盘即生效,无需重启;`--no-watch` 关闭。

## tailcat socks 端口

默认 `--upstream` 端口为 `0`:Go 版由操作系统分配空闲端口,Python/Rust 版从 `20000–60999` 随机探测一个空闲高位端口给 tailcat socks。也可 `--upstream 127.0.0.1:1081` 固定。

## 目录结构

```
tailcat-socks/
├── python/
│   ├── tailcat_socks.py          # Python 版代理主体(纯标准库)
│   └── tests/test_tailcat_socks.py
├── go/                               # Go 版(idiomatic Go 重写;fsnotify 热加载)
│   ├── main.go                       # flag / tailcat 自动拉起 / 信号
│   ├── dnsmap.go                     # dns.txt 解析 + 原子热加载
│   ├── proxy.go                      # SOCKS5 服务端 + 上游握手 + relay
│   ├── *_test.go                     # go test 单元 + 端到端
│   ├── testdata/fake-tailcat/        # 测试用假 tailcat
│   └── bin/                          # 构建产物(gitignore)
├── rust/                             # Rust 复刻版(tokio 生态)
│   ├── Cargo.toml
│   ├── src/{main,lib,config,dnsmap,proxy,tailcat,error,logging}.rs
│   ├── src/bin/fake-tailcat.rs       # 测试用假 tailcat
│   └── tests/{e2e,autolaunch,waitready_deadport}.rs
├── bin/{start,stop,restart}.sh       # 裸机后台启停(支持 go|python|rust)
├── docker/{Dockerfile,docker-compose.yml}
├── docs/superpowers/                 # 设计文档与实现计划
├── dns.txt                           # 域名→token 映射(运行时热加载)
└── run/                              # pid + log(脚本生成)
```

## 实现范围

- **支持**: SOCKS5 `CONNECT`;IPv4/IPv6/域名 ATYP;无认证握手;未命中域名透传;多线程并发;tailcat 子进程自动拉起/回收;dns.txt 热加载。
- **不支持**(YAGNI): `UDP ASSOCIATE` / `BIND`(返回失败);用户名/密码认证;跨进程共享映射。

三个实现行为一致;Docker 镜像仍基于 Go 版构建。

## 测试

```bash
cd go && go test ./...              # Go 版:解析/改写/热加载/端到端
python3 -m pytest python/tests -v   # Python 版
cd rust && cargo test               # Rust 版:解析/改写/热加载/端到端
```
覆盖 dns.txt 解析、大小写改写、未命中透传、热加载、空闲超时,以及经假上游/假 tailcat 的端到端 host 改写。
