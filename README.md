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
- **批次失败熔断与自动回退**：可用 `MaxWaveFailures` 为每批设置**失败台数上限**。
  当前波失败计数**严格越过**上限的瞬间，在同一事务内原子停止全部后续下发，活动进入
  `rolling_back` 终态，并对**本批**设备做一次性分类、生成带版本的回退指令（详见下节）。
- **生命周期**：`Pause` / `Resume` / `Abort`：
  - 暂停后停止下发新指令，但在途回执仍被接受；
  - `Resume` 时若波次已收敛：达标则正常推进，未达标则视为运维确认兜底放行（事件标注 `operator_override`）；
    若暂停期间累积失败已越过 `MaxWaveFailures`，恢复会直接转入 `rolling_back` 而非放行；
  - 中止（`Abort`）后停止发新指令、释放设备锁；在途成功回执仍会落库，成功不会被迟到失败回退。
  - 自动回退与人工暂停/中止**互斥**：三者并发时只有先提交的一种结果生效。
- **进度查询**：`GetProgress` 返回活动总体与每波的分母/成功/失败/进行中数量及成功率。
- **事务性 outbox**：所有业务事件与状态在同一事务写入 outbox，`DispatchOutbox` /
  `RunDispatcher` 提供至少一次投递；handler 失败时事件保留待重试。

## 活动状态机

```
 active ──pause──▶ paused ──resume──▶ active
   │                  │
   ├─(末波达标)──────────────────────▶ completed
   ├─(本批失败越限)────┴─(resume 时已越限)─▶ rolling_back
   └──abort───────────┴──abort──────▶ aborted
```

活动内部按波次推进；每次暂停/恢复/中止/波次推进/回退触发都会使活动版本号 `Version` +1。
`completed`、`aborted`、`rolling_back` 均为终态，不可再转移。

## 自动回退（批次失败熔断）

为活动设置 `MaxWaveFailures`（每批失败台数上限，`<=0` 表示不启用）后：

- **触发时机**：仅评估**当前波**。失败回执使当前波失败数 `failed` 满足
  `failed > MaxWaveFailures` 的那一刻触发；手动暂停期间不评估，`Resume`
  放行前会补判一次。触发与该回执写入在**同一事务**内完成，因此"停止下发 +
  分类 + 生成回退指令"是原子的，不存在越限后又放出指令的窗口。
- **触发后的活动**：进入终态 `rolling_back`，所有设备（含本批尚未领取者）
  都不能再 `ClaimCommand`；活动不能再被暂停/恢复/中止。设备锁被释放，
  在途回执仍会被接受（状态单调）。

### 本批设备如何分类（哪些设备会进入回退）

触发瞬间对**触发波次**的每台设备做一次性分类并冻结；**以前完成的批次不受影响**：

| 触发瞬间状态 | 分类 | 是否收到回退指令 |
| --- | --- | --- |
| `succeeded`（已安装成功）且设备 `RollbackSupported=true` 且冻结的历史版本非空 | `command` | **是** |
| `succeeded` 但设备不支持回退，或没有可回到的历史版本 | `skipped` | 否（列入 `Progress.Rollback.Skipped`） |
| `issued` / `pending`（仍在处理中，含已领取未回执、从未领取） | `in_progress` | 否 |
| `failed`（本批失败） | `failed` | 否 |

即：**只有"本批已安装成功、且设备本身支持回退、且升级前版本已知"的设备才会进入回退。**
处理中设备不回退（避免对状态未知的设备下发错误版本），失败设备本来就未安装新版本，也无需回退。

- 回退指令（`rollback.command` 事件 / `RollbackCommand`）携带版本：
  `FromVersion`（当前已装、需回退掉的目标版本）→ `ToVersion`（活动创建时冻结的升级前版本），
  并带触发时的活动版本号与稳定幂等键 `rb:<campaign>:<device>`。
- 分类在触发瞬间冻结：之后迟到的成功/失败回执可让升级状态继续单调收敛，
  但**不会改变分类、不会补发或撤回回退指令**；成功也不会被迟到失败倒退。
- **只发一次**：每台需要补偿的设备至多一条指令（`DeviceUpgrade.RollbackNotified` 标记）。
  正常路径触发即通知；`ScanRollbackCommands` 供定时补偿与重启恢复使用，
  重复扫描、进程重启都不会重复发令。
- 查询：`GetProgress` 在触发后返回 `Progress.Rollback`（`Installed`/`InProgress`/
  `Failed` 计数、`Commands` 清单、`Skipped` 清单），触发波次的 `WaveStat.Rollback`
  也带同样的分类计数。

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
    MaxWaveFailures:      5,              // 每批失败 >5 台即原子停止并自动回退本批已安装设备
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
`upgrade.failed`、`upgrade.compensation`、`campaign.wave_rolling_back`、`rollback.command`。

- `campaign.wave_rolling_back`：活动级，本批失败越限、原子进入回退时产生（每次活动至多一条），
  负载含 `wave`、`reason` 与触发瞬间的 `installed`/`in_progress`/`failed`/`notified` 计数。
- `rollback.command`：设备级，带版本的回退指令，每台设备至多一条。

事件在业务事务内写入；`DispatchOutbox` 按 ID 顺序投递，handler 成功后才标记已投递。

## 运行测试

```sh
go test -race ./...
```

测试覆盖：资格筛选与冻结、设备互斥、波次划分、幂等领取/回执、乱序与旧版本回执、
迟到失败的一次性补偿、成功/失败门槛与自动暂停、暂停/恢复/中止状态机、
中止后行为、outbox 至少一次投递，以及高并发回执 × 暂停/中止的竞态场景。

自动回退相关测试（`rollback_test.go`）覆盖：

- 越限原子停止下发，本批设备按 installed / in_progress / failed 正确分类，
  仅已安装成功且支持回退者收到带版本（`FromVersion → ToVersion`）的回退指令；
- 触发波次之外的以前批次不受影响、不生成回退指令；
- 触发后迟到成功/失败回执不改变冻结分类、不补发指令、成功不被倒退；
- 自动回退与人工暂停/中止并发时终态互斥（`-race`），且每台设备至多一条回退指令；
- 重复扫描 `ScanRollbackCommands` 与重启恢复（复用同一仓储构造新服务）不重复发令，
  以及"分类后通知前崩溃"的恰好一次补发；
- 暂停期间累积失败越限、`Resume` 时直接转入回退（不产生 resumed 事件）；
- 未设置 `MaxWaveFailures`（`<=0`）时保持原有失败率自动暂停行为。
