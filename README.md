# go-firmware-campaign

用于编排**分批（波次）设备固件升级活动**的 Go 服务：固件登记、活动创建、
指令领取、回执处理、暂停/恢复/中止与进度查询，内置持久化与 outbox。

开发环境：Go 1.23.0。

## 核心概念

### 活动（Campaign）

创建活动时一次性**冻结**：

- 目标设备集合（须通过资格校验：型号匹配固件、当前版本不是目标版本、电量不低于固件要求）；
- 固件摘要（digest）；
- 波次划分（`[][]deviceID`，深拷贝冻结，调用方后续修改不影响活动）。

互斥约束：同一设备不能同时加入两个未终结（running/paused）的活动，
冲突时创建失败并返回 `ConflictError`（`errors.Is(err, ErrDeviceConflict)`）。

### 指令领取（ClaimCommand）

- 仅 **当前波次** 的设备可领取；其他波次返回 `ErrNotCurrentWave`；
- 指令携带 **活动版本号** 与 **稳定幂等键**（`campaignID:version:deviceID`），
  同一设备重复领取返回相同幂等键（领取幂等）；
- 活动暂停/中止/完成后停止发放新指令（`ErrCampaignNotRunning`）。

### 回执（ReportReceipt）

回执允许**重复**与**乱序**到达，处理规则：

| 情况 | 结果 |
|---|---|
| 相同回执 ID 再次到达 | `ReceiptDuplicate`，不重复计数 |
| 回执活动版本号 < 当前版本 | `ReceiptStale`，忽略，不覆盖新状态 |
| 回执活动版本号 > 当前版本 | 报错 `ErrFutureVersion` |
| 幂等键与已发指令不符 | 报错 `ErrKeyMismatch` |
| 设备已处于更高秩状态 | `ReceiptSuperseded`，不覆盖 |

设备状态按秩单调推进：`pending < dispatched < failed < succeeded`。
`succeeded` 是吸收态——**已安装成功的设备不会被迟到的失败回执回退**；
`failed -> succeeded` 合法（乱序下成功可覆盖失败）。

### 波次推进与自动暂停

每条回执后按**冻结的波次设备分母**评估当前波次：

- 成功率 `succeeded / len(wave)` ≥ 成功门槛 → 开放下一波；最后一波达标则活动完成；
- 失败率 `failed / len(wave)` > 失败阈值 → **自动暂停**（记录原因并发出 outbox 事件）。

### 暂停 / 恢复 / 中止

- `PauseCampaign`：running → paused；
- `ResumeCampaign`：paused → running，**活动版本号 +1**（恢复前发出的旧版本回执随之失效），
  并重新评估当前波次；
- `AbortCampaign`：进入终态，停止发新指令；为仍处于 `dispatched`（已领取未完成）
  的设备生成**补偿通知**（outbox `compensation_required` 事件）——状态迁移只发生一次，
  因此补偿**只会生成一次**，重复中止返回 `ErrCampaignTerminal`。

所有变更在单把互斥锁下完成，暂停/恢复/中止与回执并发时状态保持单调
（`go test -race` 验证）。

### 持久化与 outbox

`Service` 的全部状态（固件、设备、活动、已处理回执 ID、outbox）在每次变更后
通过 `Persister` 接口写回。内置 `FilePersister`：JSON 快照 + 临时文件原子改名。
`NewService(pers)` 启动时自动加载既有状态；传 `nil` 则仅内存运行。

outbox 事件类型：`campaign_created` / `campaign_paused` / `campaign_resumed` /
`campaign_completed` / `campaign_aborted` / `wave_advanced` / `compensation_required`。

## API 一览

```go
s, _ := firmwarecampaign.NewService(firmwarecampaign.NewFilePersister("state.json"))

s.RegisterFirmware(firmwarecampaign.Firmware{ID: "fw1", Model: "m1", Version: "2.0.0", Digest: "sha256:...", MinBattery: 20})
s.RegisterDevice(firmwarecampaign.Device{ID: "d1", Model: "m1", CurrentVersion: "1.0.0", Battery: 80})

c, _ := s.CreateCampaign(firmwarecampaign.CreateCampaignRequest{
    FirmwareID: "fw1",
    Waves:      [][]string{{"d1", "d2"}, {"d3", "d4"}},
    SuccessThreshold: 0.75, // (0, 1]
    FailureThreshold: 0.25, // [0, 1]，1 表示禁用自动暂停
})

cmd, _ := s.ClaimCommand(c.ID, "d1")            // 领取指令（含版本号与幂等键）
res, _ := s.ReportReceipt(firmwarecampaign.Receipt{
    ID: "rcpt-1", CampaignID: c.ID, DeviceID: "d1",
    CampaignVersion: cmd.CampaignVersion, IdempotencyKey: cmd.IdempotencyKey,
    Success: true,
})

s.PauseCampaign(c.ID)
s.ResumeCampaign(c.ID)
s.AbortCampaign(c.ID)
p, _ := s.GetProgress(c.ID)                     // 各波次进度（冻结分母）
events := s.Outbox()                            // 读取 outbox 事件
```

## 错误处理

哨兵错误可用 `errors.Is` 判定：`ErrFirmwareNotFound`、`ErrDeviceConflict`、
`ErrNotCurrentWave`、`ErrCampaignNotRunning`、`ErrCampaignTerminal`、
`ErrFutureVersion`、`ErrKeyMismatch` 等；资格校验失败返回
`*IneligibilityError`（逐设备列明原因），互斥冲突返回 `*ConflictError`
（列明占用设备的活动）。

## 代码结构

| 文件 | 职责 |
|---|---|
| `types.go` | 领域类型、状态枚举与状态秩 |
| `errors.go` | 哨兵错误与结构化错误 |
| `store.go` | 状态快照、`Persister` 接口与 `FilePersister` |
| `service.go` | 全部业务逻辑（并发安全） |
| `service_test.go` | 功能与状态机测试 |
| `concurrency_test.go` | 并发单调性测试（`-race`） |
| `persistence_test.go` | 持久化往返测试 |

## 运行测试

```sh
go test -race ./...
```
