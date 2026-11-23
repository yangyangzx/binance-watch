# binance-watch

树莓派上的极简币价哨兵：订阅 Binance 1m K 线，按规则发邮件提醒（QQ SMTP → Outlook），
**并在收不到行情时主动告警**（这条比任何提醒都值钱）。

单二进制 daemon + 一个 CLI + 一个仅限本机的 Web 面板。唯一第三方依赖：`github.com/gorilla/websocket`。

## 构建 / 运行

```bash
export PATH=$PATH:/usr/local/go/bin
export GOPROXY=https://proxy.golang.org,direct     # 国内可换镜像
export HTTPS_PROXY=http://127.0.0.1:7890           # go mod download 经 clash

cd ~/go/binance-watch
make            # 产出 bin/watchd 和 bin/watch
make test       # 全部单测（含假 Binance 服务端到端）
make race       # 带 race detector

./bin/watchd -test-mail     # 只测 SMTP 通不通
./bin/watchd -dry-run       # 前台运行，触发时只打印邮件不真发（调阈值）
./bin/watchd                # 正式运行
make install-service        # 安装并启用 systemd 服务
```

在 x86 电脑上给树莓派编译：`make build-arm64`（32 位系统：`GOOS=linux GOARCH=arm GOARM=7 go build ./cmd/...`）。

### watchd 参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `-env` | `~/script/.env` | 复用现有 Python bot 的邮件配置（`$BW_ENV`） |
| `-proxy` | `http://127.0.0.1:7890` | 空串 = 直连，且**不会**读取环境变量里的代理（`$BW_PROXY`） |
| `-data` | `~/go/binance-watch/data` | `rules.json` 所在目录（`$BW_DATA`） |
| `-web` | `127.0.0.1:8090` | Web 面板；只允许回环地址；空串 = 关闭（`$BW_WEB`） |
| `-dry-run` | 关 | 只打印邮件，不发送 |
| `-test-mail` | 关 | 发一封测试邮件后退出 |

`.env` 额外可选项：`WATCH_SYMBOLS`（基础订阅）、`HEALTH_STALE_SEC`（默认 90）、
`EMAIL_RATE_LIMIT_PER_MIN`（默认 30）、`EMAIL_BATCH_WINDOW_SEC`（合并窗口，默认 5）。
支持 `export KEY=v`、行内 ` # 注释`、`KEY = "v"`、多个收件人（`,`/`;` 分隔）。

## 规则（CLI）

**daemon 每 2 秒检测 `rules.json`，自动热加载，并自动订阅新规则里的交易对 —— 改规则不需要重启。**

```bash
watch add BTCUSDT above 70000 --once            # 到价（涨到 70000 提醒一次）
watch add ETHUSDT --pct 3% --window 1h --dir down   # 1 小时跌 3%
watch add DOTUSDT --pct 5% --window 4h          # 4 小时涨跌 5% 都报（默认双向）
watch add SOLUSDT --near 0.5% --target 150      # 距 150 不到 0.5% 时提前提醒

watch list            # 规则列表            （ls）
watch status          # 实时行情 + 每条规则离触发还有多远（需要 watchd 在跑）
watch off <id>        # 停用  /  watch on <id> 启用（一次性规则会重新上膛）
watch rm  <id>        # 删除；ID 写唯一前缀即可
```

- `--pct` / `--near` 可写 `3%`，也可写纯整数基点（`300` = 3.00%，兼容旧用法）。
- `--window` 支持 `5`、`15m`、`1h`、`4h`，**最大 4h**（窗口设计上限；内存保留 ~15h 的 1m K 线，供 ATR(1h) 聚合）；不写 = 整个 4h。
- `--cooldown` 默认 30 分钟，可写 `2h`；任何规则最小冷却 1 分钟（兜底）。
- 终端是 TTY 时自动彩色；`--no-color` 或 `NO_COLOR=1` 关闭。

### 触发语义（重要）

规则是**边沿触发 + 滞回复位**，而不是"条件满足就一直发"：

1. 条件从"不满足"变为"满足"时触发一次，然后进入"已触发·待复位"；
2. 必须价格离开足够远才复位：到价类回到目标另一侧 0.30% 之外；涨跌幅类回落到阈值一半以下；接近类离开 2 倍距离；
3. 复位后若冷却也已过，下次满足条件才会再提醒；
4. daemon 重启时，若某规则以前触发过、此刻条件仍满足，视为"已提醒过"，不会重复轰炸。

多条规则在 5 秒内同时命中 → **合并成一封摘要邮件**；发送失败按 5s/20s/60s 重试；
每分钟最多 30 封；退出时把队列里剩余的发完。

## Web 面板

`http://127.0.0.1:8090`：实时行情卡片（涨跌 5m/15m/1h/4h + **ATR 5m/15m/1h**）、规则列表（启停/删除/距触发）、新建规则表单，
以及**邮件收件人设置**（查看/修改收件人、发测试邮件）。收件人保存后下一封邮件即生效，**无需重启 daemon**。
**没有鉴权，所以只允许绑定回环地址**（绑 `0.0.0.0` 会直接拒绝启动）。另外校验了 Host 头（防 DNS 重绑定）、
写操作必须带 `X-BW` 头（防跨站请求）、带 body 的 POST 还要求 JSON Content-Type（DELETE 无 body 不卡）、严格 CSP。

在别的机器上看：`ssh -L 8090:127.0.0.1:8090 pi@树莓派`，然后浏览器开 `http://127.0.0.1:8090`。

## 邮件

直连 SMTP，**不经代理**（代理恰好是整机最容易挂的一环）。`SMTP_PORT==465` 或 `SSL_ENABLE=true` → SMTPS；
否则 587 + STARTTLS（服务器不支持 STARTTLS 时拒绝发送，绝不明文传凭据）。
邮件为 `text/plain` + `text/html` 双版本，中文主题 RFC 2047 编码，带 Date/Message-ID/MIME-Version，
整个会话有 60s 总超时。涨绿跌红的暗色卡片，含 5m/15m/1h/4h 涨跌幅 + **ATR 5m/15m/1h**（平均真实波幅，基于 K 线 High/Low 聚合计算）。

**收件人可运行时修改**：面板上改的是 `dataDir/mail.json`（权限 0600），**不写回 `.env`**，两者可独立维护。
首次启动该文件不存在时，以 `.env` 的 `EMAIL_TO` 为初始值（不立即落盘）；一旦在面板保存过，就以文件为准。
SMTP 服务器/端口/账号/密码仍来自 `.env`，改这些需要重启。地址用 `net/mail` 严格校验，拒绝显示名写法与换行，防止邮件头注入。

**发送历史落盘**：每次发信尝试（含失败与重试）追加一条 JSONL 到 `dataDir/send-history.jsonl`（权限 0600），
字段含 `time / subject / to / ok / error`。写入失败只记日志、绝不影响发信。可用 `tail -f` 看实时发送情况，
或 `jq 'select(.ok==false)'` 筛失败记录。
**自动轮转**：单文件满 5 MiB 时，在下一次写入前滚动为 `send-history.1.jsonl`，最多保留 5 份
（数字越大越老，超出自动删最老的）；归档同为 0600。轮转失败只记日志，不影响发信。

## 健康监控

| 情形 | 行为 |
|---|---|
| 所有交易对 >90s 无数据（含**启动后一直连不上**） | 发"行情数据断流"邮件，含最后数据时间 / WS 状态 / 代理 |
| 连续 2 次检查（30s）恢复 | 发"行情已恢复"邮件，含断流时长 |
| 仅某个交易对停更 >5 分钟（可能下架） | 单独告警 / 恢复 |
| Binance 不存在的交易对 | 自动停止订阅 + 邮件提醒，面板上标红 |

数据过期时**不会**拿旧价格去判定规则。日志只在状态变化时打印，并每 10 分钟一行汇总（SD 卡友好）。

## 数据流

| 窗口 | 来源 |
|---|---|
| 5m/15m/1h/4h 涨跌幅 | 内存 910 根 1m K 线（按**时间戳**定位基准，不是按下标） |
| ATR(5m/15m/1h) | 同一 1m 缓冲聚合：5m=5 根、15m/1h=14 根 TR 均值；末桶未收盘丢弃；数据停滞 >75s 不展示 |
| 启动 / 每次 WS 重连 | REST `klines?interval=1m&limit=910`（有缺口才补） |
| WS 断 >45s | REST 拉最近 5 根 1m K 线并合并（价格和窗口数据都不断档） |

WS：combined stream，`wss://stream.binance.com:9443/stream?streams=btcusdt@kline_1m/...`；
读超时 75s + 每 20s 主动 ping（半开连接不再卡死）；指数退避重连 1s→60s（含抖动，秒断时继续退避）；
订阅集合变化（增删规则）时自动重连。

## 本次重构修复的 bug

**行情 / 数据**
1. **WS 帧解析会丢掉所有帧**：Go 的 `encoding/json` 键名大小写不敏感，帧里的 `e/E`、`t/T`、`l/L` 互相串位（数字写进字符串字段 → 解码报错 → 每帧被 `continue`）。现在所有大小写相近的键都显式声明。有真实帧的单测锁死。
2. **K 线时间用错**：用了 `k.T`（收盘时间），REST 回填用的是开盘时间 → 同一根 K 线重复入库、窗口错位。现在统一用开盘时间 `k.t`。
3. **REST 兜底永远解析失败**：`ticker/price` 返回的是数组，代码按 `map[string]string` 解 → 兜底从没真正工作过。兜底改为拉最近几根 1m K 线并合并。
4. **回填的 `Closed` 恒为 false**：读的是 `row[9]`（主动买入量，字符串）。现在用 `closeTime` 判断。
5. **回填快照会覆盖 WS 实时数据**（价格短暂回退）。现在已收盘不改、回填不覆盖实时那根。
6. **涨跌幅窗口按下标算**：WS 断线留下空洞后窗口会算歪；数据不足时悄悄用更短的窗口冒充。现在按时间戳找基准，找不到就**跳过判定**。
7. 价格解析经 `float64`（`0.29×1e8` 得到 28999999）→ 改为精确十进制解析；`PctBP` 在大价格上有 int64 溢出风险 → 改浮点除法。
8. 每次 HTTP 请求新建 `Transport`（连接泄漏）→ 复用同一个 client；`-proxy ""` 实际会走环境变量代理 → 现在严格直连；`symbols=["A","B"]` 未编码。
9. 无读超时 / 无 ping：半开连接要等很久；重连后不补回填；退避在"连上即断"时被重置成 1s 死循环。
10. **窗口基准锚在最后一根 K 线**：WS 断线让数据停滞时，`Change` 仍按"最后一根 K 线往前 N 分钟"算，得到的是陈旧历史区间的涨跌幅却标记 `ok=true`，面板/Web 的"距触发"与邮件里的 5m/15m/1h/4h 数字都是过期数据。现在加了 75s 数据停滞闸门：最后一根 K 线明显旧于当前分钟时所有窗口返回 `ok=false`，绝不拿陈旧 K 线冒充实时窗口。

**规则 / 提醒**
11. **默认冷却是 0 → 到价后每 3 秒一封邮件**（README 写的"默认 30m"与实际不符）。现在默认 30 分钟、最小 1 分钟，并改为边沿触发 + 滞回。
12. CLI 与 daemon 各持一份内存规则，daemon 的 `MarkHit` 会**整份覆盖** CLI 刚写的规则（lost update）。现在读-改-写在 `flock` 内完成。
13. daemon 不热加载 → 已实现；新规则的交易对不在 `WATCH_SYMBOLS` 里会**静默永不触发** → 现在自动订阅。
14. `near` 规则目标为 0 时除零 panic；`target` 写成 `abc` 也能通过校验 → 校验补全（窗口 ≤240、幅度、冷却范围等）。
15. CLI 把基点除以 10000 显示（`pct 300` 显示成 0.0300%）→ 统一格式化。
16. `upsert` 裁剪 250 根后 `copy(arr, arr[over:])` 让旧元素残留在底层数组尾部，被后续 `append` 复用的风险 → 改为重新分配 `make([]Kline, 0, BufferBars)`。
17. 启动 / 每次 WS 重连 / REST 兜底三处各自 `go backfillOne`，同一交易对可能叠出多个并发重试循环、重复拉 250 根 K 线浪费带宽 → 加 `backfilling` in-flight 集合去重。
18. **退出时"正在收集"的那批邮件会被静默丢弃**：`Run` 在收集窗口内被 `ctx.Done()` 打断后，拿已取消的 ctx 去 `deliver` → `waitRate` 直接丢弃，且这批已被弹出通道、`flush()` 看不到 → 改为退出路径把整批塞回通道，交由 `flush()` 用 15s 收尾 ctx 统一发完（有回归单测锁死）。

**邮件 / 运维**
18. 中文主题没做 RFC 2047 编码、缺 Date/Message-ID/MIME-Version（乱码 + 易进垃圾箱）；主题可注入换行（头注入）；SMTP 会话无总超时；不支持多收件人。
19. 邮件发送失败直接丢（`go Send` 忽略错误）→ 队列 + 重试；`EmailRateLimitPerMin` 配了没用 → 已生效。
20. **启动后一直连不上网络不会告警**（`HasData()` 为假就不报）→ 超过启动宽限即告警。断流邮件的"自 xx 起"用的是检测时刻而不是最后数据时刻。
21. systemd 的 `StartLimitIntervalSec/Burst` 写在 `[Service]` 段会被忽略 → 移到 `[Unit]`；加 `TimeoutStopSec`、`GOMEMLIMIT`、`ProtectSystem`。
22. 每 30 秒每个交易对一行日志（SD 卡磨损）→ 状态变化才打 + 10 分钟汇总。

**行情 / 采集（第二轮复查）**
23. **ATR 桶聚合错位**：`atrLocked` 按"下一根在哪"归桶——每桶末根单独成桶、下桶首根混进上桶、桶 Open 取错根，
    邮件/面板里的 ATR 数值系统性失真（实测 89，正确 97，且偏差随数据形态随机）。现在按当前根的桶 id 聚合，
    与独立参考实现逐值对比的回归单测锁死。
24. **ATR(1h) 永远为空**：`ATR(1h,14)` 需要 15 个完整小时桶（≥900 根 1m），但缓冲只有 250 根 → 恒 `ok=false`。
    `BufferBars` 250→910（涨跌幅窗口上限仍是 4h，不变）。
25. **WS `update()` 无"已收盘根"保护**：乱序/重放帧可把已收盘根打回未收盘快照，迟到的旧帧会回退最新价/时间戳
    （进而欺骗兜底与健康判定）→ 与 `merge` 同款保护：已收盘不覆盖、旧帧不动最新价。
26. **CLI 百分比解析走 float64**：`--pct 2.675%` 经 `f*100+0.5` 得 267.4999… → 截成 267（应为 268）；
    `--pct 0.004%` 静默返回 0，被当成"参数没填"而不是报错 → 改用精确十进制定点解析，<1bp 显式报错。
27. （测试）`TestFireIncludesATR` 用 30 根 K 线断言"丢弃末桶后剩 6 个 5m 桶"——该数量依赖墙钟分钟 mod 5，
    分钟落在 5m 块第 4 格时只有 5 桶 → 随机失败（`-count=10` 抓到）→ 改 35 根，任何对齐都成立。

## TODO 完成情况（原清单）

- [x] 热加载 rules.json　- [x] 去抖/复位（hysteresis）　- [x] 事件合并（digest）
- [x] 重连后补回填　- [x] WS 读超时 / ping　- [x] `internal/server` 本机面板　- [x] `--dry-run`
- [x] 端口避开 8080（默认 8090，绑定回环）
- [ ] 事件 jsonl 落盘（命中历史目前只有 `hit_count / last_hit_at` 和日志）
- [ ] 别让 Go 哨兵和 disabled 的 Python bot 同时发同一份告警（运维层面，自行确认）

## 结构

```
cmd/watchd/        daemon 入口
cmd/watch/         CLI：add / list / status / on / off / rm
internal/config/   .env 解析
internal/recipients/  运行时可改的收件人（mail.json，严格地址校验 + 原子落盘）
internal/price/    定点价格 int64×1e8、精确解析、涨跌幅(bp)、自适应格式化
internal/rules/    Rule/Store：flock + 原子写 + 热加载 + 版本号
internal/source/   Binance WS + REST 回填/兜底，按时间戳取窗口基准 + ATR（1m 缓冲聚合 5m/15m/1h）
internal/engine/   评估循环（边沿触发/滞回）+ 健康监控 + 状态快照
internal/notify/   MIME 邮件、HTML 卡片、发送队列（合并/限流/重试）
internal/server/   本机 Web 面板（embed 静态资源，无外部依赖）
deploy/            systemd unit
```
