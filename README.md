# mi-node

Mi Node 的专用节点后端，完整兼容 Mi API，支持 sing-box 与 Xray 两套内核。

> **免责声明**: 本项目仅供学习与研究使用，请自行评估实际部署风险。

## 功能特性

- **双内核支持**: `sing-box`（默认）和 `xray`
- **协议清单明确**: 公共协议与各内核专属协议都已在下方列出，便于直接对照面板配置
- **高效同步**: 优先使用 WebSocket 实时推送，异常时自动回退到 REST 轮询
- **低运维成本**: 单个 Go 二进制即可运行，便于原生部署与多节点管理
- **资源可控**: 支持 `runtime.gomemlimit` 与 `runtime.gogc`，适合小内存主机
- **出口可控**: 支持默认 SOCKS5 出站，并内置一组私网 / BT / 域名黑名单拦截规则

### 当前支持的协议

- `sing-box`（默认）: `vmess`、`vless`、`trojan`、`shadowsocks`、`hysteria`（Version=2 时生成 `hysteria2`）、`tuic`、`naive`、`socks`、`http`、`anytls`、`mieru`
- `xray`: `vmess`、`vless`、`trojan`、`shadowsocks`、`socks`、`http`、`hysteria`（仅 v2）
- 两套内核共同支持: `vmess`、`vless`、`trojan`、`shadowsocks`、`socks`、`http`
- `sing-box` 专属: `hysteria` / `hysteria2`、`tuic`、`naive`、`anytls`、`mieru`
- `xray` 专属: 无；`hysteria` 在 xray 下仅支持 v2

## 推荐部署方式：本机直接部署

如果你希望避免 Docker 带来的额外内存占用，推荐直接通过远程安装脚本进行原生部署。当前仓库远程地址为 `https://github.com/Micah123321/mi-node.git`，一键命令统一从 `dev` 分支的 Raw 脚本拉取并执行；推送到 `main`、`master` 或 `dev` 后，GitHub Actions 会在测试和四个 Linux 产物构建成功后创建新版本 Release，包含二进制与 `node-release.json`，完整上传后才设为 latest。`install.sh upgrade` 默认跟随最新 Release 的 `mi-node-linux-*` 和 `xbctl-linux-*` 产物。新版 `xbctl upgrade --version dev` 与安装脚本 `--version dev` 兼容跟随 latest；CI 同步可变 dev 下载入口供旧 xbctl 使用，固定版本发行及清单 URL 保持不可变。

### 自动接入更新管理

新版节点沿用现有配置及 machine/server token，业务启动后自动引导独立的 `mi-node-update-bootstrap.service` / timer，沿用既有 HTTP/HTTPS 面板地址通过 JSON `POST /api/v2/server/update/auto-enroll` 登记；无需登记票据或手工命令。自动登记默认关闭自动更新策略，登记成功后启用既有 `mi-node-update.timer` 并开始上报，是否更新由面板策略决定。

首次登记优先沿用 discovery 持久化安装 ID；ID、agent token 和待登记 scope 在 `/etc/mi-node/update-agent/config.json` 以 0600 保存，网络故障或 ACK 丢失由独立 timer 使用同一身份重试。认证信息只放在请求体或后续 agent Authorization 头中，服务命令只带实际 `-config` 路径；主进程仅将配置实际引用的环境变量保存为受限快照，独立引导服务加载该快照并使用 `config.LoadRoot` 解析同一配置。

已有手工登记的身份、authority 和本地 disabled 状态保持原值；损坏或复制的配置会保留并报错，待完成的手工登记继续沿用原流程。多绑定优先沿用既有 authority，否则优先选择配置中有效 HTTPS 面板，没有 HTTPS 绑定时使用现有 HTTP 面板，旧节点 scope 汇集该面板全部节点并去重排序，要求 token 一致；同一面板的多个机器身份或混合机器/旧节点认证需先统一管理绑定。

自动引导仅适用于 root、原生 Linux amd64/arm64、systemd 和 `/usr/local/bin/mi-node` / `/usr/local/bin/xbctl` 固定布局；非 root、容器和 OpenRC 保持现有业务运行。登记、布局检查和更新事务在独立服务中执行，后台失败不阻塞业务，版本探测 `version --json` / `-v` 不启动服务。发现上报中的 `auto_enroll_supported: true` 表示新版程序能力，与是否登记成功及更新策略开关无关。

自动接入故障修复：EnvironmentFile 使用不带字面引号的绝对路径，兼容实际 systemd 解析；布局检查跳过无实例模板服务、检查实际实例并识别同服务别名，避免 `systemctl show getty@.service` 阻断登记。接入定时器会重试，故障日志保留具体 systemctl 子命令与退出状态。

接入阶段与执行结果保存在 `/etc/mi-node/update-agent/enrollment-status.json`（0600），随既有业务上报动态发送；仅包含状态、阶段、固定错误码和UTC时间，无原始错误、配置或凭据。后台直接显示失败原因，普通周期 poll 成功会清除旧失败诊断；更新任务、锁和事件重放错误不会被误记成接入失败。修复后自动刷新为可用更新操作。HTTP绑定兼容需要同步发布后端接口，制品下载仍要求HTTPS且校验SHA256/大小/架构。

### 适用环境

- Linux 服务器
- 已安装 `bash`
- 使用 systemd 或 OpenRC
- 建议以 `root` 或 `sudo` 执行
- 支持 Ubuntu 20+、Debian 11+、CentOS/Rocky/AlmaLinux 8+ 等 systemd 发行版，以及 Alpine Linux/OpenRC 环境

### 一键部署单节点

直接执行：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Micah123321/mi-node/refs/heads/dev/install.sh) \
  -a https://panel.example.com \
  -t YOUR_TOKEN \
  -n 1
```

### 小内存机器推荐命令

下面这条命令会直接在宿主机部署节点，并额外写入 Go 运行时内存限制，适合希望压低内存占用的场景：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Micah123321/mi-node/refs/heads/dev/install.sh) \
  -a https://panel.example.com \
  -t YOUR_TOKEN \
  -n 1 \
  --gomemlimit 256MiB \
  --gogc 50
```

参数说明：

- `--gomemlimit 256MiB`: 设置 Go 运行时软内存上限，超过后 GC 会更积极
- `--gogc 50`: 将 GC 目标百分比调低，进一步降低峰值内存，但会增加少量 CPU 开销

### 一键申请 ACME 证书

如果你的节点协议需要 TLS，安装脚本现在可以直接写入证书配置。

使用 ACME HTTP-01：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Micah123321/mi-node/refs/heads/dev/install.sh) \
  -a https://panel.example.com \
  -t YOUR_TOKEN \
  -n 1 \
  --cert-domain node.example.com
```

- 仅传 `--cert-domain` 时，安装脚本会默认写入 `cert_mode: "http"`
- 你需要保证域名已经解析到当前服务器，且公网可以访问 `80` 端口
- `--cert-http-port` 只用于修改节点在本机上的 HTTP-01 监听端口，不会让 ACME 改去访问别的公网端口
- 如果你传了 `--cert-http-port 8080`，实际要求仍然是“公网 `80` 能访问，并且已转发到本机 `8080`”

使用 ACME DNS-01：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Micah123321/mi-node/refs/heads/dev/install.sh) \
  -a https://panel.example.com \
  -t YOUR_TOKEN \
  -n 1 \
  --cert-mode dns \
  --cert-domain node.example.com \
  --cert-dns-provider cloudflare \
  --cert-dns-env CF_API_TOKEN=YOUR_TOKEN
```

- 部署完成后，服务首次拉取面板节点配置时会按 `cert` 配置申请或生成证书；默认路径为 `/etc/mi-node/certs/<domain>.crt/.key`。
- DNS provider 与环境变量完整列表见 [docs-dns-providers.md](docs-dns-providers.md)
- DNS-01 适合被 CDN 代理、无法开放 `80` 端口，或需要通配符证书的场景

### 默认 SOCKS5 出站

如果你希望所有默认出站流量都先走一个上游 SOCKS5，可以在安装时直接传入：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Micah123321/mi-node/refs/heads/dev/install.sh) \
  -a https://panel.example.com \
  -t YOUR_TOKEN \
  -n 1 \
  --egress-socks5 127.0.0.1:1080
```

如果 SOCKS5 需要认证：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Micah123321/mi-node/refs/heads/dev/install.sh) \
  -a https://panel.example.com \
  -t YOUR_TOKEN \
  -n 1 \
  --egress-socks5 127.0.0.1:1080 \
  --egress-socks5-user your-user \
  --egress-socks5-pass your-pass
```

如果节点已经部署完成，只想给某个已存在节点添加或切换默认落地，不需要重新传面板 token：

```bash
# 查看所有节点当前默认出站
bash <(curl -fsSL https://raw.githubusercontent.com/Micah123321/mi-node/refs/heads/dev/install.sh) egress list

# 给 node_id=322 设置 SOCKS5 落地
bash <(curl -fsSL https://raw.githubusercontent.com/Micah123321/mi-node/refs/heads/dev/install.sh) egress set \
  --node-id 322 \
  --socks5-url 'socks5://your-user:your-pass@127.0.0.1:1080'

# 切换为传统 SS / SS2022 落地
bash <(curl -fsSL https://raw.githubusercontent.com/Micah123321/mi-node/refs/heads/dev/install.sh) egress set \
  --node-id 322 \
  --shadowsocks-uri 'ss://...'

# 清除该节点默认上游落地，恢复 direct 默认出站
bash <(curl -fsSL https://raw.githubusercontent.com/Micah123321/mi-node/refs/heads/dev/install.sh) egress clear \
  --node-id 322
```

`egress set/clear` 默认会重启 `mi-node.service` 让配置立即生效；如果只想写配置不重启，可以追加 `--no-restart`。SOCKS5 URL 里的特殊字符需要 URL 编码，例如密码里有 `@` 时写成 `%40`。

如果默认出站需要走传统 Shadowsocks：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Micah123321/mi-node/refs/heads/dev/install.sh) \
  -a https://panel.example.com \
  -t YOUR_TOKEN \
  -n 1 \
  --egress-shadowsocks-uri 'ss://YWVzLTEyOC1nY206eW91ci1wYXNzd29yZA==@127.0.0.1:8388'
```

如果默认出站需要走 SS2022：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Micah123321/mi-node/refs/heads/dev/install.sh) \
  -a https://panel.example.com \
  -t YOUR_TOKEN \
  -n 1 \
  --egress-shadowsocks-uri 'ss://MjAyMi1ibGFrZTMtYWVzLTI1Ni1nY206ODhvMasdads9XS3ZrRGUydWhxek4zcDE3Uk5mQzdhSE0wVldJTUtuZz06UnlObkhsZ3lLT3ZKVzRCWVY5TnhWMDlMWkhnWGM1Ui9wamxKSjRPR3QyND0=@12.23.34.56:37605?type=tcp#egress'
```

- 这项能力同时支持 `singbox` 和 `xray`
- 当配置了 `kernel.egress.socks5` 后，普通 TCP/UDP 默认出站会走这个 SOCKS5
- 如果你更需要接入 `ss` / `ss2022` 落地节点，也可以改用 `kernel.egress.shadowsocks`；默认路由行为与 `socks5` 保持一致，但两者只能二选一
- `install.sh` 现在使用单个 `--egress-shadowsocks-uri 'ss://...'` 参数写入默认 SS 出站
- `kernel.egress.shadowsocks` 现在只接受 `uri`；旧的 `address / port / method / password` 写法会在加载配置时直接报错
- `aes-192-gcm` 仅建议在 `singbox` 内核下使用；`xray` 下请使用 `aes-128-gcm`、`aes-256-gcm` 或 `chacha20-ietf-poly1305`
- 当你使用 `singbox + --egress-shadowsocks-uri` 时，服务运行日志会记录默认出站真实 probe 结果；systemd 可通过 `journalctl -u mi-node -f` 排查，OpenRC 可查看 `/var/log/mi-node.log` 和 `/var/log/mi-node.err`，也可以使用调试端口排查
- 如果未配置 SOCKS5，默认出站仍然是直连，但默认拦截规则依然会照常生效
- 传入默认出站参数时，安装脚本会写出 `kernel.egress.enable_default_rules: true` 和 `kernel.egress.prefer_ipv4: true`；未写出时这两个开关仍按代码默认值 `true` 生效

### 常用管理命令

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Micah123321/mi-node/refs/heads/dev/install.sh) status
bash <(curl -fsSL https://raw.githubusercontent.com/Micah123321/mi-node/refs/heads/dev/install.sh) upgrade
bash <(curl -fsSL https://raw.githubusercontent.com/Micah123321/mi-node/refs/heads/dev/install.sh) uninstall

xbctl status
xbctl list
xbctl service logs
```

### 旧 Xboard-Node 批量迁移到 mi-node

如果你手里有一批还在运行旧 `Xboard-Node` 的机器，而且每台机器的域名、节点 ID、证书配置都不同，仓库里已经提供了一个自动迁移脚本：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Micah123321/mi-node/refs/heads/dev/scripts/migrate-to-mi-node.sh)
```

它会在目标机器上自动读取旧配置中的 `panel.url / token / node_id / cert.* / runtime.* / kernel.egress.*`，再调用当前仓库的 `mi-node install.sh` 完成切换。迁移成功后统一由 `mi-node.service` 管理；单机多节点 `--all` 会合并到 `/etc/mi-node/config.yml` 的 `instances:`，不会为每个节点继续创建 `mi-node@<id>`。

常见用法：

```bash
# 单机单节点
ssh root@YOUR_HOST "bash <(curl -fsSL https://raw.githubusercontent.com/Micah123321/mi-node/refs/heads/dev/scripts/migrate-to-mi-node.sh)"

# 单机多节点，迁移指定节点
ssh root@YOUR_HOST "bash <(curl -fsSL https://raw.githubusercontent.com/Micah123321/mi-node/refs/heads/dev/scripts/migrate-to-mi-node.sh) --node-id 330"

# 单机多节点，整机全部迁移
ssh root@YOUR_HOST "bash <(curl -fsSL https://raw.githubusercontent.com/Micah123321/mi-node/refs/heads/dev/scripts/migrate-to-mi-node.sh) --all"
```

完整说明见 [MIGRATE.md](MIGRATE.md)。

原生部署完成后可使用：

```bash
# systemd
systemctl status mi-node
journalctl -u mi-node -f
systemctl restart mi-node

# OpenRC / Alpine
rc-service mi-node status
tail -n 100 -f /var/log/mi-node.log /var/log/mi-node.err
rc-service mi-node restart
```

## 本地 SS 出站调试端口

如果你需要确认 `sing-box + kernel.egress.shadowsocks` 是否真的成为默认出站，可以额外开启一个只监听本机回环地址的调试端口：

```yaml
debug_port: 65531
```

- 监听地址固定为 `127.0.0.1:<debug_port>`，不会暴露到公网
- 调试接口路径为 `http://127.0.0.1:<debug_port>/debug/egress`
- 返回内容包含 `default_outbound_tag`
- 返回内容包含 `shadowsocks_upstream.address / port / method`
- 返回内容包含 `last_probe`（最近一次通过当前默认出站发起的真实 TCP 拨号检查结果）
- 当前只对 `sing-box + kernel.egress.shadowsocks` 启用真实 probe；未启用 SS 出站或非 sing-box 内核时，会在返回 JSON 里明确说明

示例：

```bash
curl http://127.0.0.1:65531/debug/egress
```

## 可选方案：Docker 部署

如果你更关注环境隔离，可以继续使用 Docker。只是对于小内存机器，通常更推荐上面的原生部署方式。

```bash
docker run -d --restart=always --network=host \
  -e apiHost=https://panel.example.com \
  -e apiKey=YOUR_TOKEN \
  -e nodeID=1 \
  ghcr.io/micah123321/mi-node:latest
```

## 源码构建

### 构建当前平台二进制

```bash
make build
make build-xbctl
```

### 构建 Linux 版本

```bash
make build-linux
make build-linux-arm64
```

`make build-linux` 与 `make build-linux-arm64` 会同时生成 `mi-node-linux-*` 和 `xbctl-linux-*`，Release 会发布这两类产物。

### 固定版本与发布清单

CI 对非 tag 构建一次生成 `v<UTC YYYYMMDD>.<GITHUB_RUN_ID>.<GITHUB_RUN_ATTEMPT>`，所有四个二进制与清单复用同一版本。tag 仅接受严格的 `v数字.数字.数字`，每段除 `0` 外均无前导零，不接受预发布后缀或构建元数据；无效 tag 会在 Linux 二进制构建与 Release 发布前失败。`dev` 标签不再移动。

Release 按版本串行发布，已有 Release（包括未完成草稿）会使同版本重跑失败，既有产物不会被覆盖。非 tag 工作流整体重跑使用新的 attempt 版本；仅重跑失败作业会沿用已解析的版本。CI 构建工件按版本隔离，可由重跑替换；Release 上传先进入草稿，四个二进制与清单全部上传成功后才公开并更新 latest，避免暴露缺清单的发布。Docker 的触发条件和镜像标签规则保持原样。

本地构建和准备上传时也可使用独立脚本，需安装与 `go.mod` 匹配的 Go：

```bash
VERSION="$(go run ./scripts/release-manifest.go --version-only)"
# 也可明确指定 VERSION=v1.2.3；务必只解析一次，供全部构建复用。
make build-all VERSION="$VERSION"
go run ./scripts/release-manifest.go --version "$VERSION" \
  --repository Micah123321/mi-node --dir . --output node-release.json
```

无 GitHub Actions 运行标识时，自动版本使用 `v<UTC YYYYMMDD>.<Unix纳秒>.0`。脚本读取四个非空 Linux 文件，生成纯 JSON 对象，顶层仅含 `version`、`os`、`min_agent_protocol`、`artifacts`；每个 artifact 仅含 `arch`、`component`、`https_url`、`sha256`、`size_bytes`。SHA256 与字节数来自真实文件，缺产物会失败；URL 固定为 `https://github.com/<OWNER>/<REPO>/releases/download/<version>/<filename>`，最小 Agent 协议为 `1`。`--output -` 可将 JSON 输出到标准输出。

如手动上传，先创建同版本草稿，上传全部五个文件，再公开；不要复用已有 Release 或使用覆盖参数：

```bash
gh release create "$VERSION" --draft --target "$(git rev-parse HEAD)" --title "$VERSION" --generate-notes
gh release upload "$VERSION" mi-node-linux-amd64 mi-node-linux-arm64 \
  xbctl-linux-amd64 xbctl-linux-arm64 node-release.json
gh release edit "$VERSION" --draft=false --latest
```

新版 poll 候选支持 `manual: true`：管理员手动升级可在面板全局或策略自动开关关闭时领取，仍受本地 `Config.Disabled`、能力、策略修订、租约、产物验证与事务恢复约束，并保持正常面板事件上报。

### 运行测试

```bash
go test ./...
```

## 配置说明

最小配置示例：

```yaml
panel:
  url: "https://panel.example.com"
  token: "your-server-token"
  node_id: 1

kernel:
  type: "singbox"
  config_dir: "/etc/mi-node"
  log_level: "warn"

log:
  level: "info"
  output: "stdout"
debug_port: 65531
```

如果需要进一步压低内存使用，可以增加 `runtime` 配置：

```yaml
runtime:
  gomemlimit: "256MiB"
  gogc: 50
```

说明：

- `gomemlimit` 建议从机器可用内存的 60% 到 80% 开始尝试
- `gogc` 越低，内存越省，但 CPU 消耗会更高
- 多实例部署建议使用 `instances:`，每个实例设置独立 `kernel.config_dir`

多实例与 machine mode 示例：

```yaml
instances:
  - panel:
      url: "https://panel-a.example.com"
      token_env: "PANEL_A_TOKEN"
      node_id: 1
    kernel:
      type: "singbox"
      config_dir: "/etc/mi-node/instances/panel-a-node-1"

  - panel:
      url: "https://panel-b.example.com"
    machine:
      machine_id: 100
      token_env: "PANEL_B_MACHINE_TOKEN"
    kernel:
      type: "xray"
      config_dir: "/etc/mi-node/instances/panel-b-machine-100"
```

如果需要默认经 SOCKS5 出站，可以增加：

```yaml
kernel:
  egress:
    enable_default_rules: true
    prefer_ipv4: true
    socks5:
      address: "127.0.0.1"
      port: 1080
      # username: "your-user"
      # password: "your-pass"
    # shadowsocks:
    #   uri: "ss://MjAyMi1ibGFrZTMtYWVzLTI1Ni1nY206ODhvMasdads9XS3ZrRGUydWhxek4zcDE3Uk5mQzdhSE0wVldJTUtuZz06UnlObkhsZ3lLT3ZKVzRCWVY5TnhWMDlMWkhnWGM1Ui9wamxKSjRPR3QyND0=@12.23.34.56:37605?type=tcp#egress"
    #   # 传统 SS 示例：
    #   # uri: "ss://YWVzLTEyOC1nY206eW91ci1wYXNzd29yZA==@127.0.0.1:8388"
```

完整字段请参考 [config.yml.example](config.yml.example)。

### TLS 证书说明

- 本地配置和安装脚本都支持 `cert_mode: "http"` / `cert_mode: "dns"` 来自动申请 ACME 证书。
- 对于 `tuic`、`hysteria`（Version=2 时对应 `hysteria2`）、`anytls` 以及其他显式开启 `TLS=1` 的协议，服务端必须有可用证书文件才能启动。
- 如果面板和本地配置都没有提供证书，节点会自动在 `{config_dir}/certs` 下生成自签名证书，避免内核因为缺少证书直接启动失败。
- 生产环境仍建议显式配置可信证书，尤其是在客户端不会关闭证书校验的场景。

### 默认拦截规则说明

- 仓库现在默认启用一组内置防滥用规则，会拦截私网访问、BitTorrent 以及一批危险/不希望放行的域名模式。
- 这套规则会在 `singbox` 和 `xray` 两套生成配置中同时生效。
- 即使你没有配置 `kernel.egress.socks5` 或 `kernel.egress.shadowsocks`，这套默认拦截规则也仍然会优先生效。
- 如果你要调整它，可以直接修改 `kernel.egress.enable_default_rules` 和 `kernel.egress.prefer_ipv4`，或者继续补充自己的 `custom_route` / `custom_config` 规则。
- 如果你没有配置任何默认上游出站，默认出站仍然是 `direct`；如果配置了 `kernel.egress.socks5` 或 `kernel.egress.shadowsocks`，默认出站会切到对应的上游。

## 重要说明：Shadowsocks 2022 与 UUID

如果节点使用 `2022-blake3-*` 系列 Shadowsocks cipher，请不要继续把用户密码当普通 `xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx` 这种 UUID 使用。

- `2022-blake3-aes-128-gcm` 要求标准 base64 编码后的 16 字节密钥
- `2022-blake3-aes-256-gcm` 与 `2022-blake3-chacha20-poly1305` 要求标准 base64 编码后的 32 字节密钥
- 如果面板仍下发 UUID，节点可能在启动阶段报 `invalid shadowsocks 2022 user password`，或在客户端连接阶段出现 `message authentication failed`

推荐做法:

- 如果你的用户体系就是 UUID，请将节点 cipher 改回传统 Shadowsocks，例如 `aes-128-gcm`、`aes-256-gcm`、`chacha20-ietf-poly1305`
- 如果你必须使用 `2022-blake3-*`，请确保服务端 `server_key` 和每个用户密码都使用标准 base64 密钥，而不是 UUID
- 如果你把它配置在 `kernel.egress.shadowsocks.uri` 里，URI 解码后的 `method:password` 中仍然使用标准 base64 key；多用户或中转场景依然是 `<server_key>:<user_key>`

## License

MPL-2.0
