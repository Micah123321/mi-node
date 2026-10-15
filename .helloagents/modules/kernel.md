# kernel 模块

## 职责

- 生成 `sing-box` 与 `xray` 的运行时配置
- 将面板配置和本地 `kernel.egress` 配置转换为内核原生 outbound / route

## 当前关键行为

- 默认出站仍保留 `direct` / `block` 基础结构
- sing-box 用户 IP 表按需创建，末连接关闭后释放峰值容量；统计快照按最近实际流量用户/在线用户数预分配，累计字节继续保留。连接索引高峰达到256条后，剩余连接降至最近峰值四分之一以下时重建索引，回收容量并保留存活连接的强制关闭入口。
- Xray 连接以准入租约绑定原始 IP 计数容器，关闭幂等释放；限额切换和 Reset 后迟到关闭不会清理新连接，无限制用户容器随最后连接释放，状态快照在锁内读取。
- Xray HTTP 用户计费按原生认证用户名(UUID)查询计数器，其他协议按user@ID；计数器身份到面板UID的归属保留至实例排空，用户元数据更新前先保存旧身份与累计字节，避免首次采样前更新漏记和跨UID名称碰撞。Linux HTTP CONNECT原生splice下载在传输结束时提交计数，关闭后的精确总量有真实TCP回归覆盖。
- tracker 不再复制无人读取的待上报流量表；成功完整采样后清理缺席用户的基线，大幅缩小时重建容量。service 仅在持久化 ACK 后清零已发送队列槽，明细过期/恢复也释放旧槽，失败计费批次仍保留重试。
- 当配置 `kernel.egress.shadowsocks` 时，会生成 `default-shadowsocks` outbound
- `sing-box` 与 `xray` 对默认 Shadowsocks 出站保持一致的 tag 与路由语义
- `xray.Protocols()` 与已实现 inbound builder 保持一致：`vmess`、`vless`、`trojan`、`shadowsocks`、`socks`、`http`、`hysteria`
- `xray` 未声明未实现协议；当前不声明 `dokodemo-door`
- `xray` 传统 Shadowsocks inbound 使用多用户 `clients` 结构，避免只吃首个用户
- `trojan` inbound 在 `sing-box` / `xray` 下都会在缺省时补 TLS 兜底，减少面板下发 `tls=0` 时的启动兼容问题
- `grpc` 传输同时兼容 `service_name` 与 `serviceName` 两种字段命名
- 路由顺序在默认保护规则启用时为：内置保护规则 → structured custom routes → raw custom routes → panel routes → final outbound
