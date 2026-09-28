# go-firmware-campaign

分批进行的设备固件升级活动编排服务（Go 库，仅依赖标准库）。

开发环境：Go 1.23.0。

## 能力概览

- **固件登记**：`RegisterFirmware` 登记型号、版本、SHA256 摘要等元数据。
- **设备快照**：`RegisterDevice` 维护设备型号、当前版本、电量等属性；
  `RollbackAllowed` 显式声明该设备是否允许固件回退（默认不允许）。
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
- **批次失败上限与自动回退**：`CreateCampaign` 设置 `MaxFailuresPerWave`（每批失败数
  硬上限，`0` 表示不启用）后，**当前波失败数一旦越过（严格大于）上限**，处理该失败
  回执的事务立即**原子**完成（无需等待本批收敛）：
  - 活动进入 `rolled_back` 终态，**停止向后续设备继续下发**（未领取的不再领取，
    已领取在途的回执仍被接受，但不再发新升级指令）；
  - 为**本批**已安装成功且允许回退的设备，各生成一条带版本的回退指令事件
    `rollback.command`（含 `installed_version` → `rollback_version`、波次、活动版本、
    稳定幂等键 `rb:<campaign>:<device>`）；
  - **以前完成的批次不受影响**，不生成回退；
  - 迟到回执不能倒退终态：成功之后迟到的失败仍只产生补偿通知。
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
   ├─(本批失败数越限)─────────────────▶ rolled_back
   └──abort───────────┴──abort──────▶ aborted
```

活动内部按波次推进；每次暂停/恢复/中止/回退/波次推进都会使活动版本号 `Version` +1。

## 哪些设备会进入自动回退

触发保护的瞬间只扫描**触发波次（本批）**的设备，按下列规则分类，可通过
`GetProgress` 的字段查看：

| 本批设备情况 | 处理 | Progress 字段 |
| --- | --- | --- |
| 已安装成功（`succeeded`）且创建时 `RollbackAllowed=true` | **生成回退指令**，回到冻结的升级前版本 `FromVersion` | `RollbackCommanded` |
| 已安装成功、允许回退，但触发时尚未发令（在途成功后到达） | 回执事务内自动补发，或由 `ScanRollbackCommands` 补发 | `RollbackWaiting`（补发后清空） |
| 已安装成功但 `RollbackAllowed=false` | **不回退**，保持新版本 | `InstalledNotRollbackable` |
| 仍在处理中（`pending` 未领取 / `issued` 已领取未回执） | **不发回退指令**、不再下发升级指令；其迟到成功会补发一次回退（允许回退时），迟到失败则不回退 | `RollbackInFlight` |
| 已失败（`failed`） | 不回退（本就停留在旧版本），计入 `Failed` | `Failed` |
| **以前批次**的任何设备 | **完全不受影响**，既不停止也不回退 | 不在上述列表 |

回退的目标版本在活动创建时从设备快照冻结为 `FromVersion`（即该设备的升级前版本），
指令载荷同时携带已安装的（坏）版本 `installed_version` 与 `rollback_version`。

**每台设备至多一条回退指令**：设备级 `RollbackCommandSent` 标记与稳定幂等键
`rb:<campaign>:<device>` 共同保证——重复调用 `ScanRollbackCommands`、进程重启后
恢复扫描、在途成功补发，都不会重复发令。

**与人工操作互斥**：自动回退（`rolled_back`）与人工暂停（`paused`）、中止（`aborted`）
在串行化事务中竞争，只有**先提交的一种结果**生效：若人工暂停/中止抢先，则不会产生
任何回退指令；若回退抢先，则暂停/恢复/中止返回 `invalid_transition`。

## 快速开始

```go
svc := firmwarecampaign.NewService(firmwarecampaign.NewMemoryRepository())

svc.RegisterFirmware(firmwarecampaign.RegisterFirmwareInput{
    ID: "fw-2.0.0", Model: "sensor-x", Version: "2.0.0", SHA256: "abc123",
})
svc.RegisterDevice(firmwarecampaign.Device{
    ID: "dev-1", Model: "sensor-x", CurrentVersion: "1.9.0", BatteryLevel: 80,
    RollbackAllowed: true, // 显式允许，触发批次回退时才会收到回退指令
})

c, _, err := svc.CreateCampaign(firmwarecampaign.CreateCampaignInput{
    ID:                   "cp-1",
    FirmwareID:           "fw-2.0.0",
    DeviceSelector:       firmwarecampaign.DeviceSelector{Model: "sensor-x", MinBatteryLevel: 50},
    BatchSize:            100,            // 每波设备数
    SuccessRateThreshold: 0.8,            // 成功率门槛
    MaxFailureRate:       0.2,            // 失败率上限
    MaxFailuresPerWave:   3,              // 每批失败数硬上限：越过（>3）立即自动回退
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
| `campaign_not_active` | 活动暂停/中止/完成/回退后领取指令 |
| `not_current_wave` | 非当前波次设备领取（含已过去的波次） |
| `stale_receipt` | 回执活动版本低于已接受水位 |
| `upgrade_terminal` | 已终态的升级条目再次领取 |
| `invalid_transition` | 非法生命周期转移（如重复中止、恢复非暂停活动、对 `rolled_back` 活动暂停/恢复/中止） |

## 持久化与并发模型

- `Repository` 接口是持久化抽象，`UpdateTx` 要求可串行化事务。
- 内置 `MemoryRepository` 以整库互斥锁 + 快照拷贝实现事务（回滚即丢弃拷贝），
  因此暂停/恢复/中止与回执并发执行时状态依旧单调一致（测试以 `-race` 覆盖）。
- 生产环境实现同一接口接入 SQL 数据库即可：设备锁可用唯一约束
  `(device_id) WHERE campaign_status IN ('active','paused')`，
  outbox 与业务更新放在同一数据库事务中投递。

## Outbox 事件

`campaign.created`、`campaign.paused`、`campaign.resumed`、`campaign.aborted`、
`campaign.rolled_back`、`campaign.wave_advanced`、`campaign.completed`、
`upgrade.succeeded`、`upgrade.failed`、`upgrade.compensation`、`rollback.command`。

事件在业务事务内写入；`DispatchOutbox` 按 ID 顺序投递，handler 成功后才标记已投递。

## 运行测试

```sh
go test -race ./...
```

测试覆盖：资格筛选与冻结、设备互斥、波次划分、幂等领取/回执、乱序与旧版本回执、
迟到失败的一次性补偿、成功/失败门槛与自动暂停、暂停/恢复/中止状态机、
中止后行为、outbox 至少一次投递，以及高并发回执 × 暂停/中止的竞态场景。

自动回退专项（`rollback_test.go`）覆盖：

- 失败数**严格越过**上限的边界与触发后立即 `rolled_back`、停止领取；
- 回退指令只发给本批已成功且允许回退的设备，载荷带 `installed_version` /
  `rollback_version` 与稳定幂等键；
- 触发后在途设备的迟到成功（允许则补发一次）、迟到失败（不回退）、
  已成功设备的迟到失败（不倒退成功、不再发令）；
- 重复扫描与**重启恢复**（同一仓储新建 `Service`）均不重复发令，
  在途成功也可由扫描补发；
- 自动回退与人工暂停、中止并发 20 轮压力测试（`-race`）：结果唯一、
  有回退指令当且仅当 `rolled_back` 胜出；
- 进度查询的设备分类（`RollbackCommanded` / `RollbackWaiting` /
  `InstalledNotRollbackable` / `RollbackInFlight`）；
- 未启用上限（`MaxFailuresPerWave=0`）时仍按失败率自动暂停，不回退。
