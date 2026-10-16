# install 模块

## 职责

- 管理原生安装脚本 `install.sh` 与已安装后的服务管理入口
- 维护 `mi-node` 二进制、`xbctl`、`/etc/mi-node/config.yml`、`credentials.env` 和服务定义的一致性

## 当前关键行为

- `install.sh` 支持 `install/status/upgrade/egress/uninstall`，默认安装根目录为 `/etc/mi-node`，二进制目录为 `/usr/local/bin`
- init system 探测优先使用运行中的 systemd；未运行 systemd 时支持 Alpine/OpenRC
- systemd 环境写入 `/etc/systemd/system/mi-node.service`，服务名为 `mi-node.service`
- OpenRC 环境写入 `/etc/init.d/mi-node`，服务名为 `mi-node`，日志写入 `/var/log/mi-node.log` 和 `/var/log/mi-node.err`
- `xbctl service/status/egress/upgrade/uninstall` 会按当前 init system 调用 `systemctl` 或 `rc-service`/`rc-update`
- `xbctl remove <node-id> [--panel URL]` 按面板节点 ID 唯一定位本机普通节点绑定，再按实例标识复用原有绑定删除流程；重复 ID 提示指定面板，机器、standalone 和旧式多节点聚合配置不参与匹配；参数必须是完整正整数。
- `install.sh egress ...` 只负责准备 `xbctl` 并透传参数，配置修改仍由 `xbctl egress` 统一完成
- 安装器和文档不得记录真实面板 token、DNS API token 或其他凭据
