# traffic-limit 模块

## 职责

- 解析面板下发的节点级 `traffic_limit` 配置
- 基于 tracker tick 的上行 / 下行增量累计当前周期用量
- 达到额度后停止内核，并在重置时间到达或面板手动重置后恢复运行
- 将限额状态持久化到本地 JSON，并通过 metrics 上报给 Xboard

## 当前关键行为

- 面板协议在 `panel.NodeConfig` 和 `model.NodeSpec` 中使用 `TrafficLimit` / `TrafficLimitSpec` 承载配置，machine mailbox clone 不会丢失该字段
- `internal/trafficlimit.Manager` 持有启用状态、额度、重置日、重置时间、时区、周期用量、下次重置和 suspended 状态
- 服务层与内核重载判断共用 `kernel.ConfigHash()`，排除整个 `TrafficLimit`；配额启停、额度、用量及重置状态由限额管理器独立处理，不重建协议入站
- REST 拉取、WS 配置推送和 WS 断开后的 REST 补拉均通过 `Service.applyConfigUpdate()` 同步配额；协议配置哈希相同时仍更新缓存和限额状态，但不调用 `kernel.Reload()`
- 相同 `last_reset_at` 的周期用量按本地与面板的较大值合并；自动重置使用计划周期边界，延迟检查与错过周期后仍能对齐面板；较小报告不清空累计量，旧周期报告及旧重置计划不回填或清空已重置用量
- 在途 REST 响应若已被较新的 WS 配置或用户状态取代，会丢弃对应旧快照并安排完整补拉；补拉会清空 ETag，WS 在线时也执行待处理的补拉；请求携带代次，重连/丢包发生前已排队的结果不会清除较新的补拉请求
- ACME 续期通过独立事件通知主循环应用新证书，不依赖 REST 轮询或配额变化；无用户期间缓存配置，首批全量或增量用户到达后启动协议
- 状态文件固定写入 `kernel.config_dir/traffic-limit-state.json`；进程重启后会恢复周期用量和 suspended 状态
- `ResetDay` 支持 `1-31`，短月按当月最后一天计算；非法时区回退 UTC
- `tracker.Process()` 返回本 tick 的节点总上行 / 下行增量，`Service.trackAndEnforce()` 把两者合并交给限额管理器
- 达到额度后 `Service` 调用 `kernel.Stop()`，并通过 `startKernel()` 与 `ensureRunning()` 的 `CanRun()` gate 阻止自动重启
- 处于限额下线期间，用户全量更新和增量更新仍会刷新 `lastUsers` 与限速器状态，但不会调用内核用户 API；重置恢复时使用最新缓存用户启动
- 到达 `next_reset_at` 或面板下发更新的 `last_reset_at` 后，限额状态恢复 normal，`Service` 在已有 `lastConfig` 和 `lastUsers` 时尝试重新启动内核
- metrics 中追加 `traffic_limit`，包含 `enabled / limit / used / suspended / last_reset_at / next_reset_at / suspended_at / status`

## 依赖关系

- 依赖 `internal/tracker` 计算每个 tick 的双向增量
- 依赖 `internal/service` 执行内核停启 gate、配置热更新、用户缓存和 metrics 上报
- 依赖 `internal/panel`、`internal/model` 与 `internal/controlplane` 传递面板配置
- 依赖 Xboard `ServerTrafficLimitService` 下发配置、处理面板侧手动/定时重置和状态回写
