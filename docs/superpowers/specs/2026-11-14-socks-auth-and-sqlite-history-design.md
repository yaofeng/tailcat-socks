# 设计：SOCKS5 认证授权 + SQLite 历史访问记录

日期：2026-11-14
状态：已与用户逐节确认
前置：`0.9.0` 已实现内嵌管理页、tailcat 库化、未命中直连（见 `2026-11-13-web-admin-and-tailcat-library-design.md`）

## 1. 目标

1. **配置化**：从 `data/config.yaml` 读取配置，当前包含 SOCKS5 认证用户与历史记录开关。
2. **SOCKS5 认证与按用户授权**：两个内置用户，权限不同——
   - `tailcat`：**只能**访问 `dns.txt` 中命中的域名（走隧道）；
   - `all`：可访问任意目标（命中走隧道，未命中直连）。
3. **SQLite 历史访问记录**：按（日期, 客户端 IP, 域名, 端口）聚合当日次数/失败/流量，管理页新增"历史连接"标签查看，支持单日筛选与按累计流量倒序。
4. 版本号更新为 `0.10.0`（只改 `.version`）。

## 2. 非目标

- 用户管理界面（用户只在 `config.yaml` 里配置，改后需重启）；
- 配置热加载（`dns.txt` 以外的东西都不热加载）；
- 历史数据的清理/归档策略；
- 多日区间查询与图表；
- 按用户维度区分的历史统计（历史表不记用户名）；
- 明文密码加密存储（`config.yaml` 视为与 `dns.txt` 同级的敏感文件，600 权限）。

## 3. 配置（`data/config.yaml`）

```yaml
socks:
  users:
    tailcat: "changeme"      # 只能访问 dns.txt 命中的域名（隧道）
    all: "changeme"          # 可访问任意目标

history:
  enabled: true
  path: "data/history.db"
```

- 解析库：`gopkg.in/yaml.v3`（纯 Go，无 CGO）。
- 路径：默认 `data/config.yaml`，可用 `--config` 覆盖。
- **文件不存在**：自动生成带上述默认值的文件（权限 0600）+ WARN「已生成默认配置，请修改密码」，随后继续运行。
- **解析失败**：ERROR + exit 1（配置坏了不能猜）。
- **`users` 为空或缺失**：ERROR + exit 1——认证是强制项，没有用户等于谁都不能连。
- 密码比较用 `crypto/subtle.ConstantTimeCompare`。

## 4. SOCKS5 认证与授权

### 4.1 认证流程（`serveConn`）

现状只接受 `0x00`（无认证）。改为只接受 `0x02`（USERNAME/PASSWORD，RFC1929）：

```
1. greeting：在 methods 中找 0x02
   找到   → 回 [0x05, 0x02]，进入子协商
   没找到 → 回 [0x05, 0xFF]，断开
2. 子协商：读 [ver=0x01, ulen, username, plen, password]
   ver != 0x01 或 ulen/plen 为 0 → 断开
   用户不存在 或 密码不符     → 回 [0x01, 0x01]，断开
   通过                       → 回 [0x01, 0x00]，username 带进后续流程
3. CONNECT 请求（现有逻辑不变）
4. 路由 + 授权（新增，见 4.2）
5. 拨号 + relay（现有逻辑不变）
```

### 4.2 授权

在 `route()` 之后、`ConnRegistry.New` 之前检查：

| 用户名 | 目标命中 dns.txt | 目标未命中 |
|---|---|---|
| `tailcat` | 放行（隧道） | **拒绝**，回 `0x02` |
| `all` | 放行（隧道） | 放行（直连） |

拒绝用 SOCKS5 reply `0x02`（connection not allowed by ruleset），随后断开。

### 4.3 现有 reply 辅助

当前只有 `socksOKReply` / `socksFailReply` 两个固定字节串。新增 `socksReplyDenied = {0x05, 0x02, 0x00, 0x01, 0,0,0,0, 0,0}`，或抽出 `socksReply(client, code)` 函数。

## 5. SQLite 历史记录

### 5.1 依赖

`modernc.org/sqlite`（纯 Go 实现，无 CGO，`CGO_ENABLED=0` 静态构建不受影响）。代价：二进制体积增加约 5–10 MB。

### 5.2 表结构

```sql
CREATE TABLE IF NOT EXISTS history (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  date        DATE    NOT NULL,   -- 'YYYY-MM-DD'，SQLite DATE 亲和性
  client_ip   TEXT    NOT NULL,
  host        TEXT    NOT NULL,
  port        INTEGER NOT NULL,
  count       INTEGER NOT NULL DEFAULT 0,   -- 当日累计次数
  failures    INTEGER NOT NULL DEFAULT 0,   -- 当日累计失败次数
  bytes_up    INTEGER NOT NULL DEFAULT 0,
  bytes_down  INTEGER NOT NULL DEFAULT 0,
  UNIQUE (date, client_ip, host, port)
);

CREATE INDEX IF NOT EXISTS idx_history_date_bytes
  ON history(date, (bytes_up + bytes_down) DESC);
```

- `id` 是稳定行号（页面引用用），聚合靠 `UNIQUE` 约束定位。
- `date` 存 `YYYY-MM-DD` 本地时区字符串。

### 5.3 写入时机与语义

`serveConn` 的 `defer` 里，连接结束时写一次：

```sql
INSERT INTO history (date, client_ip, host, port, count, failures, bytes_up, bytes_down)
VALUES (?, ?, ?, ?, 1, ?, ?, ?)
ON CONFLICT(date, client_ip, host, port) DO UPDATE SET
  count      = count + 1,
  failures   = failures + excluded.failures,
  bytes_up   = bytes_up + excluded.bytes_up,
  bytes_down = bytes_down + excluded.bytes_down;
```

- **`client_ip` 不含端口**：用 `net.SplitHostPort` 取 IP。否则客户端每次换源端口都会产生新行。
- **失败判定**：拨号失败或未进入 relay（`Session.Opened == false`）→ `failures += 1`、字节记 0，`count` 仍 +1（确实尝试过）。
- **不记历史**：认证失败、权限拒绝（0x02）、SOCKS 握手异常——都发生在 `Session` 创建之前。
- **一次连接一次写库**：能拿到完整字节数，开销最小；崩溃时最多丢正在进行中的会话。

### 5.4 连接参数

- WAL 模式：`_pragma=journal_mode(WAL)`
- `_pragma=busy_timeout(5000)`
- 写入失败只打 WARN，绝不影响代理主路径。

### 5.5 打开失败的处理

历史库打不开（路径不可写、文件损坏等）→ **WARN 后继续运行**，历史功能降级为不可用，页面显示"历史记录未启用"。理由：历史是旁路功能，不该因为它让代理拒绝服务。

## 6. 管理页

### 6.1 标签顺序

当前连接 → **历史连接** → DNS 配置。

### 6.2 历史连接页

```
日期 [2026-11-14]     ← input[type=date]，默认今天

汇总卡：条目数 / 总次数 / 失败次数 / 上行 / 下行 / 总流量

表格：客户端 IP | 域名 | 端口 | 次数 | 失败 | ↑字节 | ↓字节 | 总流量
      按总流量倒序（服务端已排序）
```

- 切到该标签或改日期时拉一次 `GET /api/history?date=...`；**不轮询**（历史只在连接结束时变化）。
- 空态：`该日暂无记录`。
- 失败次数 > 0 时标红。
- `history.enabled: false` → `历史记录未启用（config.yaml: history.enabled）`。

### 6.3 API

```
GET /api/history?date=YYYY-MM-DD
```

- `date` 缺失 → 默认今天；格式非法 → `400 {"error":"date 需为 YYYY-MM-DD"}`。
- 响应：

```json
{"enabled":true,"date":"2026-11-14",
 "totals":{"entries":3,"count":42,"failures":1,
           "bytesUp":1744,"bytesDown":11461957,"totalBytes":11463701},
 "entries":[{"id":7,"clientIp":"172.16.2.233","host":"a100-2-01.yidong.tail",
             "port":8188,"count":42,"failures":1,
             "bytesUp":1744,"bytesDown":11461957,"totalBytes":11463701}]}
```

排序：`ORDER BY (bytes_up + bytes_down) DESC`。

## 7. 退出顺序

`run()` 收尾增加 `history.Close()`，放在 `dialer.Close()` 之后（它不依赖网络，最后关无妨）。

## 8. 测试计划（全离线）

1. **`config_test.go`**：正常解析；`users` 缺失报错；文件不存在时创建默认并返回默认值；解析失败报错。
2. **`proxy_test.go` 扩展**：认证成功/失败；无 `0x02` method 被拒（`0xFF`）；`tailcat` 用户访问未命中 → `0x02`；`all` 用户访问未命中 → 放行；认证失败不产生 `Session`。
3. **`history_test.go`**：用 `t.TempDir()` 建库；upsert 累加；UNIQUE 冲突更新同一行；按 date 过滤；按总流量倒序；失败计入 count 且 failures 递增；`enabled=false` 时不写。
4. **`admin_test.go` 扩展**：`/api/history` 默认今天；非法 date → 400；`enabled=false` 的响应形状。

## 9. 部署面变化

- **`.version`**：`0.9.0` → `0.10.0`（版本号只存在于该文件）。
- **`docker/Dockerfile`**：无需改（`modernc.org/sqlite` 纯 Go，`CGO_ENABLED=0` 保持）。
- **`docker/docker-compose.yml`**：`/data` 卷已存在，历史库落在其中；新增注释说明 `config.yaml` 也走 `/app/config` 只读挂载。
- **`data/config.yaml`**：加入 `.gitignore`（含明文密码）；提供 `data/config.yaml.example`。
- **`.dockerignore`**：加入 `data/config.yaml`（避免把真实密码打进镜像）。
- **`README.md`**：新增配置、认证、历史三节；参数表补 `--config`。
- **`bin/start.sh`**：无需改（配置由二进制自己读默认路径）。
