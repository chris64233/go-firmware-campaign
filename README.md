# go-firmware-campaign

分批进行的设备固件升级活动编排服务（Go 库，仅依赖标准库）。

开发环境：Go 1.23.0。

## 能力概览

- **固件登记**：`RegisterFirmware` 登记型号、版本、SHA256 摘要等元数据。
- **设备快照**：`RegisterDevice` 维护设备型号、当前版本、电量等属性。
- **活动创建与冻结**：`CreateCampaign` 按资格条件（型号、当前版本白名单、最低电量）筛选设备，
  并在同一事务中冻结：
  - 目标设备集合（之后新增/变更设备不影响活动，分母恒定）；
  - 固件摘要（SHA256）与目标版本；
  - 波次划分（按固定批量切分，顺序确定）。
- **设备互斥**：同一设备不能同时属于两个未结束（active/paused）的活动；创建时被占用的设备自动排除。
- **指令领取**：`ClaimCommand` 仅允许 **当前波次** 且活动 `active` 的设备领取；
  指令携带单调递增的**活动版本号**与**稳定幂等键**（`upg:<campaign>:<device>`），
  重复领取返回同一指令（`AlreadyIssued`），不产生副作用。
- **回执处理**：`SubmitReceipt` 接受可能**重复或乱序**的回执：
  - 幂等键不匹配 → `invalid_argument`；
  - 活动版本低于已接受水位 → `stale_receipt`，状态不变；
  - 状态单调：`pending → issued → succeeded/failed`，`succeeded` 为最高优先级终态；
  - **成功后迟到的失败不会回退成功**，只会生成**至多一次**补偿通知（`upgrade.compensation`）；
  - 重复回执幂等丢弃，不重复产生事件。
- **波次门槛**：当前波全部收敛后，按**冻结的设备分母**计算：
  - 成功率达到 `SuccessRateThreshold` → 开放下一波；末波达标 → 活动 `completed`；
  - 失败率超过 `MaxFailureRate`（或成功率不达标）→ 活动**自动暂停**，等待运维处理。
- **生命周期**：`Pause` / `Resume` / `Abort`：
  - 暂停后停止下发新指令，但在途回执仍被接受；
  - `Resume` 时若波次已收敛：达标则正常推进，未达标则视为运维确认兜底放行（事件标注 `operator_override`）；
  - 中止（`Abort`）后停止发新指令、释放设备锁；在途成功回执仍会落库，成功不会被迟到失败回退。
- **进度查询**：`GetProgress` 返回活动总体与每波的分母/成功/失败/进行中数量及成功率。
- **事务性 outbox**：所有业务事件与状态在同一事务写入 outbox，`DispatchOutbox` /
  `RunDispatcher` 提供至少一次投递；handler 失败时事件保留待重试。

## 活动状态机

```
 active ──pause──▶ paused ──resume──▶ active
   │                  │
   ├─(末波达标)──────────────────────▶ completed
   └──abort───────────┴──abort──────▶ aborted
```

活动内部按波次推进；每次暂停/恢复/中止/波次推进都会使活动版本号 `Version` +1。

## 快速开始

```go
svc := firmwarecampaign.NewService(firmwarecampaign.NewMemoryRepository())

svc.RegisterFirmware(firmwarecampaign.RegisterFirmwareInput{
    ID: "fw-2.0.0", Model: "sensor-x", Version: "2.0.0", SHA256: "abc123",
})
svc.RegisterDevice(firmwarecampaign.Device{
    ID: "dev-1", Model: "sensor-x", CurrentVersion: "1.9.0", BatteryLevel: 80,
})

c, _, err := svc.CreateCampaign(firmwarecampaign.CreateCampaignInput{
    ID:                   "cp-1",
    FirmwareID:           "fw-2.0.0",
    DeviceSelector:       firmwarecampaign.DeviceSelector{Model: "sensor-x", MinBatteryLevel: 50},
    BatchSize:            100,            // 每波设备数
    SuccessRateThreshold: 0.8,            // 成功率门槛
    MaxFailureRate:       0.2,            // 失败率上限
})

cmd, _ := svc.ClaimCommand(firmwarecampaign.ClaimCommand{CampaignID: c.ID, DeviceID: "dev-1"})
svc.SubmitReceipt(firmwarecampaign.ReceiptInput{
    CampaignID: c.ID, DeviceID: "dev-1",
    CampaignVer: cmd.CampaignVer, IdempotencyKey: cmd.IdempotencyKey,
    Result: firmwarecampaign.ReceiptSucceeded,
})

p, _ := svc.GetProgress(c.ID)
```

完整可运行示例见 `example_test.go`。

## 错误处理

所有业务错误均为 `*firmwarecampaign.Error`，携带稳定错误码，可用 `IsError(err, code)` 或 `errors.Is` 判定：

| 错误码 | 触发场景 |
| --- | --- |
| `invalid_argument` | 参数缺失/越界、幂等键不匹配、非法状态转移 |
| `not_found` | 固件/活动/设备条目不存在 |
| `already_exists` | 固件或活动 ID 重复 |
| `conflict` | 没有合格设备；未领取指令却上报回执 |
| `device_busy` | （仓储层）设备已被其他活动锁定 |
| `campaign_not_active` | 活动暂停/中止/完成后领取指令 |
| `not_current_wave` | 非当前波次设备领取（含已过去的波次） |
| `stale_receipt` | 回执活动版本低于已接受水位 |
| `upgrade_terminal` | 已终态的升级条目再次领取 |
| `invalid_transition` | 非法生命周期转移（如重复中止、恢复非暂停活动） |

## 持久化与并发模型

- `Repository` 接口是持久化抽象，`UpdateTx` 要求可串行化事务。
- 内置 `MemoryRepository` 以整库互斥锁 + 快照拷贝实现事务（回滚即丢弃拷贝），
  因此暂停/恢复/中止与回执并发执行时状态依旧单调一致（测试以 `-race` 覆盖）。
- 生产环境实现同一接口接入 SQL 数据库即可：设备锁可用唯一约束
  `(device_id) WHERE campaign_status IN ('active','paused')`，
  outbox 与业务更新放在同一数据库事务中投递。

## Outbox 事件

`campaign.created`、`campaign.paused`、`campaign.resumed`、`campaign.aborted`、
`campaign.wave_advanced`、`campaign.completed`、`upgrade.succeeded`、
`upgrade.failed`、`upgrade.compensation`。

事件在业务事务内写入；`DispatchOutbox` 按 ID 顺序投递，handler 成功后才标记已投递。

## 运行测试

```sh
go test -race ./...
```

测试覆盖：资格筛选与冻结、设备互斥、波次划分、幂等领取/回执、乱序与旧版本回执、
迟到失败的一次性补偿、成功/失败门槛与自动暂停、暂停/恢复/中止状态机、
中止后行为、outbox 至少一次投递，以及高并发回执 × 暂停/中止的竞态场景。
