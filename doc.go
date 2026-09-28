// Package firmwarecampaign 实现分批进行的设备固件升级活动编排。
//
// 核心能力：
//
//   - 固件登记（RegisterFirmware）与设备快照管理（RegisterDevice）；
//   - 活动创建（CreateCampaign）：按型号、当前版本、电量等资格条件筛选
//     设备，并在同一事务中冻结目标设备集合、固件摘要（SHA256）与波次
//     划分；同一设备不能同时属于两个未结束的活动（互斥锁）；
//   - 指令领取（ClaimCommand）：仅当前波次、活动处于 active 时可领取，
//     指令携带单调的活动版本号与稳定幂等键，重复领取幂等重放；
//   - 回执处理（SubmitReceipt）：回执可重复、乱序，旧活动版本的回执
//     不能覆盖新状态；成功为最高优先级终态，迟到失败不回退成功，
//     仅生成一次补偿通知；
//   - 波次门槛：当前波按冻结的设备分母计算成功率，达标才开放下一波，
//     失败率超阈值自动暂停；暂停可由运维 Resume 确认放行；
//   - 批次失败上限与自动回退：CreateCampaign 设置 MaxFailuresPerWave
//     后，当前波失败数一旦越过（>）上限，处理该失败回执的事务原子地将
//     活动转入 rolled_back 终态：立即停止向后续设备下发，并为本批
//     （触发波次）已安装成功（succeeded）且创建时冻结为允许回退
//     （Device.RollbackAllowed）的设备生成带版本的回退指令
//     （rollback.command：installed_version→rollback_version、稳定幂等
//     键）。以前完成的批次不受影响；触发后在途设备迟到成功会补发一次、
//     迟到失败不回退；成功终态不会被任何迟到回执倒退。ScanRollbackCommands
//     支持重复扫描与重启恢复补偿，设备级标记保证每台设备至多一次发令。
//     rolled_back 与人工暂停/中止互斥，并发时只有先提交的一种结果生效；
//   - 生命周期：Pause / Resume / Abort 与回执并发时状态单调，中止后
//     不再下发新指令但仍接受在途回执；
//   - 事务性 outbox：业务状态与事件同事务写入，至少一次投递；
//   - 进度查询（GetProgress）：返回活动总体与每波的冻结分母统计。
//
// 所有业务操作在 Repository 的可串行化事务中执行。内置的
// MemoryRepository 使用整库互斥锁，适用于测试与单机；生产环境可实现
// Repository 接口接入 SQL 数据库。
package firmwarecampaign
