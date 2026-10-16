# Snell TCP v4 Agent

独立 Go module：`github.com/micah123321/mi-node/snell-agent`。它使用自己的 TCP 监听和依赖图；父项目的内核、依赖及产物目录保持原状。

OpenSnell alpha 固定在 [55c60d02794aaaafde2e47594f125419dba1654d](https://github.com/missuo/opensnell/tree/55c60d02794aaaafde2e47594f125419dba1654d)，Go 模块版本为 `v1.0.4-0.20260529145304-55c60d02794a`。直接使用该提交的 `UserStore.ListUsers`、`ServerConfig.OnAuthorize` 和 `Server.ServeListener`。OpenSnell 依赖采用 GPL-3.0-or-later，分发时一并遵循其许可证。

## 一键安装（Linux / systemd）

在服务器 root 终端执行，无需手动上传文件或安装 Go：

```sh
bash <(curl -fsSL https://raw.githubusercontent.com/Micah123321/mi-node/refs/heads/dev/snell-agent/install.sh) \
  -a https://panel.example.com \
  -t PANEL_SERVER_TOKEN \
  -n 41
```

-a 为面板根地址（按传入协议支持 HTTP / HTTPS），-t 为节点通信 Token，-n 为 Snell 节点数字 ID。Snell 使用自身加密，无需 TLS 证书，--cert-* 参数会在安装前给出不适用提示，参数值不写日志。Token 保留引号并按 JSON 转义；也可用 --token-file /root/snell-token.txt 代替 -t。

脚本自动识别 amd64 / arm64，查询 Micah123321/mi-node 的完整 snell-v 预发布版本，下载对应程序及 SHA256SUMS，固定同一 tag 并完成校验，再安装独立用户和 systemd 服务。普通 mi-node Release 和缺少对应程序或校验文件的 Snell Release 会跳过。默认最多检查 500 条 Release；可通过 --version 固定 tag、--release-repo OWNER/REPO 更换来源。GitHub /releases/latest 不用于 Snell，避免取到普通节点程序。

首次可交互输入；升级可重新执行同一条 -a/-t/-n 命令，相同参数保留配置原文和计费队列。任何已提供的身份参数与现有配置不一致时，脚本保留原安装并报错，避免把队列切换到另一节点。

自动安装依赖支持 apt-get / dnf / yum，只在缺少 Python 3、curl 或 flock 时安装。目标系统需要 systemd；脚本无需本地仓库或 Go。端口来自面板，放行相应 TCP 端口并使用 1024 及以上端口。

本地包、指定程序和源码构建同样保留：

```sh
sudo bash install.sh -a https://panel.example.com -t PANEL_SERVER_TOKEN -n 41
sudo bash install.sh --config /root/snell-config.json --binary ./bin/snell-agent-linux-amd64
sudo bash install.sh --build
```

本地脚本旁有预编译程序时优先使用；明确 --version 时使用远程版本。--config 仅用于首次导入完整配置，且 state_dir 应为 /var/lib/snell-agent。--no-start / --skip-start 只安装和启用开机自启，当前服务保持停止。自定义下载支持 --binary-url HTTPS_URL --sha256 SHA256。

安装到 /usr/local/bin/snell-agent，配置为 /etc/snell-agent/config.json（专用用户所有、0600），服务为 snell-agent.service。安装器先以 -check-config 验证配置，该模式不创建状态目录、不访问面板。停止旧进程前完成来源及配置校验，再原子替换文件。旧文件保存在 /var/backups/snell-agent/时间戳-进程号/；启动失败会恢复旧文件和之前的服务状态，报告目录保持原样。

```sh
systemctl status snell-agent --no-pager
journalctl -u snell-agent -f
```

GitHub 上的远程入口和程序由 .github/workflows/snell-agent-release.yml 发布；本地修改推送到 dev 并完成该流程后，新版一键命令才会生效。

## 构建与运行

需要 Go 1.24.7 或更高版本。在本目录执行，始终使用本模块的依赖：

```sh
cd snell-agent
GOWORK=off go mod download
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOWORK=off \
  go build -trimpath -o bin/snell-agent-linux-amd64 ./cmd/snell-agent
./bin/snell-agent-linux-amd64 -config /etc/snell-agent/config.json
```

Windows PowerShell 交叉构建：

```powershell
$env:GOWORK = "off"
$env:CGO_ENABLED = "0"
$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -trimpath -o bin/snell-agent-linux-amd64 ./cmd/snell-agent
```

[JSON 示例](config.example.json)中的 token 为占位内容。运行时配置文件建议设为 0600，状态目录设为 0700；进程只需配置文件读取权和状态目录写入权。端口来自面板，普通用户应使用 1024 以上端口。

| 配置 | 含义 |
| --- | --- |
| `panel_url` | 面板根 URL，可包含子目录，支持显式 HTTP / HTTPS；禁止 URL 用户信息、query 和 fragment。 |
| `token` / `node_id` | 每次请求的 query 认证；node_id 为正整数。 |
| `pull_interval` | 字符串时长，例如 `30s`；省略或 `0s` 时使用面板 pull_interval，否则覆盖面板值。 |
| `report_interval` | 字符串时长；省略或 `0s` 时使用面板 push_interval，否则覆盖面板值。 |
| `state_dir` | 默认 `./state`；保存增量报告队列及进程锁，禁止多个 Agent 共享。 |
| `stale_after` | 默认 `5m`，正时长；距离最近一次完整有效 config + users 同步的最长容忍时间。 |
| `listen_ip` | 可选 IP 字面量，覆盖面板 listen_ip；两处均为空则监听 `0.0.0.0`。 |
| `request_timeout` | 默认 `10s`；限制单次 HTTP 请求。 |

面板未给出有效正间隔时，默认拉取 30 秒、上报 60 秒。拉取与上报分别运行，慢上报不会阻塞用户同步。建议 stale_after 大于正常拉取间隔与请求耗时。启动时先处于停接入状态，获得有效快照后开始服务；面板暂时不可用时继续重试。

## 面板契约

三个 HTTP 入口都使用 `?token=TOKEN&node_id=NODE_ID`。GET 返回直接 JSON 对象，无 data 包装；不请求机器发现、WebSocket、更新服务或旧版 API。HTTP 重定向被拒绝，避免认证 query 随重定向发送。

```text
GET  /api/v2/server/config
GET  /api/v2/server/user
POST /api/v2/server/report
```

```json
{
  "protocol": "snell",
  "listen_ip": "0.0.0.0",
  "server_port": 12345,
  "version": 4,
  "obfs": "off",
  "obfs_host": "",
  "udp": false,
  "base_config": {"pull_interval": 30, "push_interval": 60},
  "traffic_limit": {"enabled": false, "suspended": false}
}
```

`obfs` 仅支持 `off`、`http`；`obfs_host` 必须为空。UDP-over-TCP 与 QUIC 均关闭，也不占用 UDP 端口。v4/v5 的 TCP 线上格式相同；这里固定 v4 功能集，不承诺区分使用相同 TCP 格式的客户端版本标签。

用户格式为 `{"users":[{"id":1,"uuid":"raw-UTF8-key","speed_limit":0,"device_limit":0}]}`。四个字段均必需且非 null，ID 为正整数，限制为非负整数；0 表示对应限制关闭。重复 ID、重复 UUID、缺失/畸形字段、非 UTF-8、额外未知字段、非 200 响应均保留旧有效快照且不刷新同步时间。`{"users":[]}` 是有效的撤销全部用户指令。

每用户 PSK 是 UUID 字符串的原始 UTF-8 字节，不做 Base64/十六进制解码、不去空白、不重新生成。内核 Name 使用 `ID:版本号`，在内存中映射到当前 UUID/ID；删除后重新加入及 UUID 重置都会生成新版本，旧认证不会恢复有效。面板 token 与用户 PSK 均不写日志、不进入报告文件；OpenSnell 自身日志关闭，Agent 仅输出不含请求 URL/响应正文的错误类别。

## 授权、计量与停用

每次 CONNECT（包括同一加密 TCP 上的复用 CONNECT）都经 OnAuthorize 再检查用户状态。入站原始连接按远端地址登记，绑定认证用户并跟踪对应上游。用户删除或 UUID 重置时同时关闭其上游与原始 TCP，连空闲复用连接一起关闭。端口、监听 IP 或混淆模式变化会重建监听并关闭旧连接。

计量包装目标 `net.Conn`：`Write` 的实际字节计为 upload，`Read` 的实际字节计为 download；不计 Snell/混淆头、加密 padding 或 TCP/IP 头。复用的每一段目标连接单独包装，最后汇总到用户 ID。关闭时等待正在进行的计量操作结束后再落盘最终增量。

speed_limit 单位为十进制 Mbps（1 Mbps = 125000 B/s），同一用户的所有连接共享预算，上下行各自限速。按最多 16 KiB 的块平滑调度，没有额外初始突发额度；限速更新作用于后续块。流关闭或被撤销会取消限速等待，并释放共享预算。

设备数按认证 TCP 连接的来源 IP 去重，IPv4-mapped IPv6 归一为 IPv4。同 IP 多连接占一个名额，空闲复用 TCP 仍占名额，最后一条连接关闭后释放。限额降低至当前 IP 数以下时关闭该用户所有原连接，使后续连接重新竞争名额。

**此版本的设备限制只在本节点生效。** 已核对面板 DeviceStateService：现有 alivelist 返回的是每用户跨节点设备数量，而非可按节点排除及去重的 IP 快照。本模块不把该数量误当成全局 IP 集合，也未接入跨节点设备同步；多节点同时连接可超过全局 device_limit。alive 上报真实 IP，online 上报本节点去重 IP 数。

`traffic_limit.suspended=true`、`status="suspended"`，或启用配额且 `current_used >= limit > 0` 时立即停接入并关闭现有连接；有效的暂停配置即使随后用户拉取失败也会生效。配额总用量由面板决定，本版本不在两次拉取之间独立推算跨节点/整机账期配额，不自行安排重置。

超过 stale_after 时，新认证立即停用；独立 watchdog 在至多 `min(250ms, stale_after/4)` 后关闭已建立连接。恢复有效同步后自动恢复。磁盘 checkpoint 失败同样停接入并关闭连接，写入恢复且快照仍新鲜时才恢复服务。

只支持直连系统路由与系统 DNS。面板或本地配置中的 routing、routes、outbounds、自定义出站、DNS 等未知配置会明确报错，保留最后有效配置至其过期，不会声称这些配置已应用。未集成旧内核的路由策略。

## 报告与恢复

报告形状：

```json
{
  "report_id": "32-hex-random-id",
  "traffic": {"1": [123, 456]},
  "alive": {"1": ["192.0.2.1"]},
  "online": {"1": 1},
  "metrics": {"kernel_status": true, "users": 1, "connections": 1, "udp": false, "quic": false}
}
```

kernel_status 表示当前接入就绪，启动未同步、过期、暂停、空用户、存储失败或退出时为 false。报告只接受 HTTP 200 且 JSON `data:true` 的确认。每个 report_id 对应不可变的增量正文；先写临时文件、fsync、原子替换队列，再从内存清除该增量。失败、确认丢失或确认后本地删除失败都会按同 ID/正文重试；恢复后依靠面板 report_id 去重。成功确认且队列更新提交后才丢弃批次，不重报累计计数。每轮最多处理 16 个积压批次。

`reports.json` 绑定 panel_url 与 node_id，身份不匹配或损坏时启动报错而不覆盖文件；同目录进程锁防止并发写入。保持 state_dir 持久化，升级时保留队列。队列不含认证材料，但包含来源 IP，应控制访问权限。队列没有自动丢弃策略；面板长时间故障时应监控磁盘容量。

SIGINT/SIGTERM 会取消请求、停止接入、关闭上下游，并把最终内存增量保存，留给下次启动发送。**未到报告 checkpoint 的活跃增量暂存在内存；强制杀进程/断电可能损失最近一个报告间隔的流量，磁盘故障期间尚未提交的增量也存在同样风险。** 已落盘报告支持重启恢复；成功确认但本地确认记录尚未持久化的窗口通过同 ID 重试消除重复计费。Windows 使用文件 flush + 原子替换；Unix 还会同步父目录。

## systemd

[服务示例](deploy/snell-agent.service)使用独立低权限用户和持久化 StateDirectory：

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin snell-agent
sudo install -m 0755 bin/snell-agent-linux-amd64 /usr/local/bin/snell-agent
sudo install -d -m 0750 -o root -g snell-agent /etc/snell-agent
# 将已填妥的运行时配置安装至 /etc/snell-agent/config.json，所有者 snell-agent，权限 0600。
sudo install -m 0644 deploy/snell-agent.service /etc/systemd/system/snell-agent.service
sudo systemctl daemon-reload
sudo systemctl enable --now snell-agent
sudo journalctl -u snell-agent -f
```

服务示例的 state_dir 应为 `/var/lib/snell-agent`。上述命令为手工安装方式，也可使用 install.sh 完成相同步骤。安装脚本的验收只在临时 chroot 中运行，未在真实服务器安装服务或连接线上面板。

## 验证

安装器 50 项回归已通过，覆盖短参数、HTTP 面板、特殊字符 Token、流式脚本入口、Snell Release 筛选与分页、固定 tag、自动 SHA256 校验、交互式隐藏输入、重复升级、文件权限、锁竞争以及服务失败回滚。测试使用真实 Linux Agent 执行配置预检，文件安装在临时 chroot 中，systemctl 和 HTTPS 传输由测试夹具替代。Linux 上以 root 运行；只影响临时目录：

```sh
sudo python3 tests/install_test.py
bash -n install.sh
```

Agent 验证：

```sh
GOWORK=off go test ./...
CGO_ENABLED=1 GOWORK=off go test -race ./...  # 需要适用的本机 C 编译器
GOWORK=off go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOWORK=off \
  go build -trimpath -o bin/snell-agent-linux-amd64 ./cmd/snell-agent
```

测试使用真实 Go Snell client 和回环 TCP/HTTP/面板，无外网目标：off/http 互通、非对称 HTTP 上传下载、两用户独立计费、复用连续计量、删除/换密钥/重新加入切断活动及空闲连接、IP 设备限制与限额下调、聚合 Mbps 与独立下行限速、暂停/过期/空快照、错误用户集保留、上报幂等及重启恢复、存储失败、慢上报与同步隔离，以及优雅退出最终落盘。

本次验收已通过：Windows Go 1.26.4 的普通测试、go vet 与 Linux amd64 交叉构建；WSL Ubuntu + GCC + Linux Go 1.26.4 的完整竞态测试；Linux 产物帮助入口实际运行。宿主共享 x/sys 缓存存在空文件，验收使用本目录 `bin/.modcache` 的独立缓存，并通过 `go mod verify`。当前机器复验时可设置 PowerShell `$env:GOMODCACHE="$PWD/bin/.modcache"`，Linux 则设置 `GOMODCACHE="$PWD/bin/.modcache"`；此缓存已被 Git 忽略。
