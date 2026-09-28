package firmwarecampaign

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Clock 便于测试控制时间。
type Clock func() time.Time

// Service 是固件升级活动编排服务。所有业务操作都在 Repository 的
// 串行化事务中执行，因此暂停/恢复/中止与设备回执并发时状态依旧单调。
type Service struct {
	repo Repository
	now  Clock
}

// NewService 创建服务。
func NewService(repo Repository) *Service {
	return &Service{repo: repo, now: time.Now}
}

// NewServiceWithClock 使用自定义时钟创建服务（主要用于测试）。
func NewServiceWithClock(repo Repository, clock Clock) *Service {
	if clock == nil {
		clock = time.Now
	}
	return &Service{repo: repo, now: clock}
}

// ---- 固件与设备登记 ----

// RegisterFirmware 登记固件。ID 重复返回 CodeAlreadyExists。
func (s *Service) RegisterFirmware(in RegisterFirmwareInput) (Firmware, error) {
	if in.ID == "" || in.Model == "" || in.Version == "" {
		return Firmware{}, wrapError("RegisterFirmware", ErrInvalidArgument, "id, model and version are required")
	}
	fw := Firmware{
		ID:          in.ID,
		Model:       in.Model,
		Version:     in.Version,
		SHA256:      in.SHA256,
		SizeBytes:   in.SizeBytes,
		Description: in.Description,
		CreatedAt:   s.now(),
	}
	if err := s.repo.UpdateTx(func(tx TxStore) error {
		if _, err := tx.GetFirmware(fw.ID); err == nil {
			return wrapError("RegisterFirmware", ErrAlreadyExists, "firmware %q", fw.ID)
		}
		tx.PutFirmware(fw)
		return nil
	}); err != nil {
		return Firmware{}, err
	}
	return fw, nil
}

// RegisterDevice 注册或更新设备快照（型号、当前版本、电量等）。
func (s *Service) RegisterDevice(d Device) (Device, error) {
	if d.ID == "" || d.Model == "" {
		return Device{}, wrapError("RegisterDevice", ErrInvalidArgument, "id and model are required")
	}
	if d.BatteryLevel < 0 || d.BatteryLevel > 100 {
		return Device{}, wrapError("RegisterDevice", ErrInvalidArgument, "battery level must be in [0,100]")
	}
	if d.Status == "" {
		d.Status = DeviceStatusOnline
	}
	d.UpdatedAt = s.now()
	if err := s.repo.UpdateTx(func(tx TxStore) error {
		tx.PutDevice(d)
		return nil
	}); err != nil {
		return Device{}, err
	}
	return d, nil
}

// ---- 活动创建（冻结） ----

// CreateCampaign 按资格条件筛选设备并冻结：目标设备集合、固件摘要、
// 波次划分在创建时确定，之后不再变化。被其他未结束活动占用的互斥
// 设备会被排除（若全部被占用则返回 CodeConflict）。
func (s *Service) CreateCampaign(in CreateCampaignInput) (Campaign, CampaignSnapshot, error) {
	if in.ID == "" {
		return Campaign{}, CampaignSnapshot{}, wrapError("CreateCampaign", ErrInvalidArgument, "campaign id is required")
	}
	if in.FirmwareID == "" {
		return Campaign{}, CampaignSnapshot{}, wrapError("CreateCampaign", ErrInvalidArgument, "firmware id is required")
	}
	if in.BatchSize <= 0 {
		return Campaign{}, CampaignSnapshot{}, wrapError("CreateCampaign", ErrInvalidArgument, "batch size must be positive")
	}
	if in.SuccessRateThreshold < 0 || in.SuccessRateThreshold > 1 {
		return Campaign{}, CampaignSnapshot{}, wrapError("CreateCampaign", ErrInvalidArgument, "success rate threshold must be in [0,1]")
	}
	if in.MaxFailureRate < 0 || in.MaxFailureRate > 1 {
		return Campaign{}, CampaignSnapshot{}, wrapError("CreateCampaign", ErrInvalidArgument, "max failure rate must be in [0,1]")
	}
	if in.MaxFailuresPerWave < 0 {
		return Campaign{}, CampaignSnapshot{}, wrapError("CreateCampaign", ErrInvalidArgument, "max failures per wave must be >= 0")
	}
	sel := in.DeviceSelector
	if sel.Model == "" {
		return Campaign{}, CampaignSnapshot{}, wrapError("CreateCampaign", ErrInvalidArgument, "device selector model is required")
	}
	if sel.MinBatteryLevel < 0 || sel.MinBatteryLevel > 100 {
		return Campaign{}, CampaignSnapshot{}, wrapError("CreateCampaign", ErrInvalidArgument, "min battery level must be in [0,100]")
	}

	var result Campaign
	var snap CampaignSnapshot
	err := s.repo.UpdateTx(func(tx TxStore) error {
		if _, err := tx.GetCampaign(in.ID); err == nil {
			return wrapError("CreateCampaign", ErrAlreadyExists, "campaign %q", in.ID)
		}
		fw, err := tx.GetFirmware(in.FirmwareID)
		if err != nil {
			return wrapError("CreateCampaign", ErrNotFound, "firmware %q", in.FirmwareID)
		}
		if fw.Model != sel.Model {
			return wrapError("CreateCampaign", ErrInvalidArgument,
				"firmware model %q does not match selector model %q", fw.Model, sel.Model)
		}

		allowed := map[string]bool{}
		for _, v := range sel.AllowedFromVersion {
			allowed[v] = true
		}

		var deviceIDs []string
		deviceByID := map[string]Device{}
		var busy []string
		for _, d := range tx.ListDevices() {
			if !qualified(d, sel, fw.Version, allowed) {
				continue
			}
			if owner, locked := tx.DeviceLock(d.ID); locked && owner != in.ID {
				busy = append(busy, d.ID)
				continue
			}
			deviceIDs = append(deviceIDs, d.ID)
			deviceByID[d.ID] = d
		}
		if len(deviceIDs) == 0 {
			return wrapError("CreateCampaign", ErrConflict,
				"no qualified devices (model=%s, qualified-but-busy=%d)", sel.Model, len(busy))
		}

		waves := splitWaves(deviceIDs, in.BatchSize)
		now := s.now()
		c := Campaign{
			ID:                 in.ID,
			FirmwareID:         fw.ID,
			Model:              fw.Model,
			TargetVersion:      fw.Version,
			FirmwareDigest:     fw.Checksum(),
			Status:             CampaignStatusActive,
			CurrentWave:        1,
			TotalWaves:         len(waves),
			Version:            1,
			SuccessRate:        in.SuccessRateThreshold,
			MaxFailureRate:     in.MaxFailureRate,
			MaxFailuresPerWave: in.MaxFailuresPerWave,
			BatchSize:          in.BatchSize,
			CreatedAt:          now,
			UpdatedAt:          now,
		}
		snap = CampaignSnapshot{
			CampaignID:     c.ID,
			FirmwareID:     fw.ID,
			FirmwareDigest: c.FirmwareDigest,
			TargetVersion:  c.TargetVersion,
			Model:          c.Model,
			DeviceIDs:      deviceIDs,
			Waves:          waves,
			CreatedAt:      now,
		}

		for waveIdx, ids := range waves {
			for _, id := range ids {
				d := deviceByID[id]
				u := DeviceUpgrade{
					DeviceID:        id,
					CampaignID:      c.ID,
					Wave:            waveIdx + 1,
					Status:          UpgradeStatusPending,
					TargetVersion:   c.TargetVersion,
					FromVersion:     d.CurrentVersion,
					RollbackAllowed: d.RollbackAllowed,
					IdempotencyKey:  stableKey(c.ID, id),
				}
				tx.PutUpgrade(u)
				tx.LockDevice(id, c.ID) // 同事务内锁定，互斥活动无法重复纳入
			}
		}
		tx.PutCampaign(c)
		tx.PutSnapshot(snap)
		tx.AddOutbox(mustEvent(EventCampaignCreated, c.ID, "", map[string]any{
			"firmware_id":    c.FirmwareID,
			"target_version": c.TargetVersion,
			"digest":         c.FirmwareDigest,
			"total_waves":    c.TotalWaves,
			"device_count":   len(deviceIDs),
		}))
		result = c
		return nil
	})
	if err != nil {
		return Campaign{}, CampaignSnapshot{}, err
	}
	return result, snap, nil
}

func qualified(d Device, sel DeviceSelector, targetVersion string, allowed map[string]bool) bool {
	if d.Model != sel.Model {
		return false
	}
	if d.BatteryLevel < sel.MinBatteryLevel {
		return false
	}
	if d.CurrentVersion == targetVersion {
		return false // 已是目标版本，无需升级
	}
	if len(allowed) > 0 && !allowed[d.CurrentVersion] {
		return false
	}
	return true
}

// splitWaves 按冻结的设备顺序（ID 排序后）切波，保证可重复。
func splitWaves(ids []string, size int) [][]string {
	var waves [][]string
	for i := 0; i < len(ids); i += size {
		end := i + size
		if end > len(ids) {
			end = len(ids)
		}
		waves = append(waves, append([]string(nil), ids[i:end]...))
	}
	return waves
}

// stableKey 生成指令的稳定幂等键：同一 (活动,设备) 永远得到同一把键，
// 重复领取与重复回执都可据此去重。
func stableKey(campaignID, deviceID string) string {
	return fmt.Sprintf("upg:%s:%s", campaignID, deviceID)
}

// ---- 指令领取 ----

// ClaimCommand 只有当前波次、且活动处于 active 的设备可以领取指令。
// 指令带活动版本号与稳定幂等键；同一键重复领取返回 AlreadyIssued。
func (s *Service) ClaimCommand(in ClaimCommand) (ClaimResult, error) {
	if in.CampaignID == "" || in.DeviceID == "" {
		return ClaimResult{}, wrapError("ClaimCommand", ErrInvalidArgument, "campaign id and device id are required")
	}
	var res ClaimResult
	err := s.repo.UpdateTx(func(tx TxStore) error {
		c, err := tx.GetCampaign(in.CampaignID)
		if err != nil {
			return wrapError("ClaimCommand", ErrNotFound, "campaign %q", in.CampaignID)
		}
		u, err := tx.GetUpgrade(in.CampaignID, in.DeviceID)
		if err != nil {
			return wrapError("ClaimCommand", ErrNotFound, "device %q is not in campaign", in.DeviceID)
		}
		if c.Status != CampaignStatusActive {
			return wrapError("ClaimCommand", ErrCampaignNotActive, "campaign is %s", c.Status)
		}
		if u.Wave != c.CurrentWave {
			return wrapError("ClaimCommand", ErrNotCurrentWave,
				"device wave %d, current wave %d", u.Wave, c.CurrentWave)
		}
		// 首次领取由服务端分配稳定幂等键；之后设备必须原样回传。
		if in.IdempotencyKey != "" && in.IdempotencyKey != u.IdempotencyKey {
			return wrapError("ClaimCommand", ErrInvalidArgument,
				"idempotency key mismatch for device %q", in.DeviceID)
		}

		switch u.Status {
		case UpgradeStatusSucceeded, UpgradeStatusFailed:
			return wrapError("ClaimCommand", ErrUpgradeTerminal, "upgrade is %s", u.Status)
		case UpgradeStatusIssued:
			if u.CampaignVer == c.Version {
				// 幂等重放：返回同一指令，不产生任何副作用。
				res = claimResult(c, u, true)
				return nil
			}
			// 期间经历过暂停/恢复，版本号前进：重新下发刷新版本。
		}

		now := s.now()
		firstIssue := u.Status == UpgradeStatusPending
		u.Status = UpgradeStatusIssued
		u.CampaignVer = c.Version
		u.IssuedAt = now
		u.Attempts++
		tx.PutUpgrade(u)
		if firstIssue {
			c.UpdatedAt = now
			tx.PutCampaign(c)
		}
		res = claimResult(c, u, false)
		return nil
	})
	if err != nil {
		return ClaimResult{}, err
	}
	return res, nil
}

func claimResult(c Campaign, u DeviceUpgrade, already bool) ClaimResult {
	return ClaimResult{
		CampaignID:     c.ID,
		DeviceID:       u.DeviceID,
		FirmwareID:     c.FirmwareID,
		TargetVersion:  c.TargetVersion,
		FirmwareDigest: c.FirmwareDigest,
		Wave:           u.Wave,
		CampaignVer:    u.CampaignVer,
		IdempotencyKey: u.IdempotencyKey,
		AlreadyIssued:  already,
	}
}

// ---- 回执 ----

// SubmitReceipt 处理设备回执。回执可能重复或乱序：
//   - 幂等键不匹配 → CodeInvalidArgument；
//   - 活动版本低于已接受水位 → CodeStaleReceipt，状态不变；
//   - 终态单调：成功不得被迟到的失败回退，此时仅生成一次补偿通知；
//   - 当前波次全部收敛时按冻结分母计算成功率，达标放行下一波，
//     失败超阈值则自动暂停；
//   - 当前波失败数越过 MaxFailuresPerWave 的瞬间，在同一事务原子转入
//     rolled_back 并生成本批回退指令，先于一切暂停/推进结算。
//
// 活动中止或回退后仍接受在途回执（只是不再下发新指令）；rolled_back
// 之后迟到的成功若属于本批且具备回退条件，会补发恰好一次回退指令。
func (s *Service) SubmitReceipt(in ReceiptInput) error {
	if in.CampaignID == "" || in.DeviceID == "" {
		return wrapError("SubmitReceipt", ErrInvalidArgument, "campaign id and device id are required")
	}
	if in.Result != ReceiptSucceeded && in.Result != ReceiptFailed {
		return wrapError("SubmitReceipt", ErrInvalidArgument, "result must be succeeded or failed")
	}
	return s.repo.UpdateTx(func(tx TxStore) error {
		c, err := tx.GetCampaign(in.CampaignID)
		if err != nil {
			return wrapError("SubmitReceipt", ErrNotFound, "campaign %q", in.CampaignID)
		}
		u, err := tx.GetUpgrade(in.CampaignID, in.DeviceID)
		if err != nil {
			return wrapError("SubmitReceipt", ErrNotFound, "device %q is not in campaign", in.DeviceID)
		}
		key := in.IdempotencyKey
		if key == "" {
			key = stableKey(in.CampaignID, in.DeviceID)
		}
		if key != u.IdempotencyKey {
			return wrapError("SubmitReceipt", ErrInvalidArgument,
				"idempotency key mismatch for device %q", in.DeviceID)
		}
		if u.Status == UpgradeStatusPending {
			return wrapError("SubmitReceipt", ErrConflict, "command was never issued to device %q", in.DeviceID)
		}
		// 旧活动版本的回执不能覆盖新状态。
		if in.CampaignVer < u.ReceiptVer {
			return wrapError("SubmitReceipt", ErrStaleReceipt,
				"receipt version %d < accepted version %d", in.CampaignVer, u.ReceiptVer)
		}

		now := s.now()
		desired := UpgradeStatusSucceeded
		if in.Result == ReceiptFailed {
			desired = UpgradeStatusFailed
		}

		switch {
		case u.Status == desired:
			// 完全重复的回执：幂等丢弃，不产生事件。
			return nil
		case u.Status.Terminal() && desired.rank() <= u.Status.rank():
			// 成功之后迟到的失败：不得回退，补偿通知至多一次。
			if u.Status == UpgradeStatusSucceeded && desired == UpgradeStatusFailed && !u.CompensationSent {
				u.CompensationSent = true
				tx.PutUpgrade(u)
				tx.AddOutbox(mustEvent(EventCompensation, c.ID, u.DeviceID, map[string]any{
					"reason": in.Reason,
					"note":   "late failure after success; success retained",
				}))
			}
			return nil
		default:
			// pending→issued 之外的正向收敛（issued→终态，或乱序的 failed→succeeded）。
			u.Status = desired
			u.ResolvedAt = now
			u.ReceiptVer = in.CampaignVer
			if in.CampaignVer > u.CampaignVer {
				u.CampaignVer = in.CampaignVer
			}
			if desired == UpgradeStatusFailed {
				u.FailureReason = in.Reason
			}
			tx.PutUpgrade(u)
			evType := EventUpgradeSucceeded
			if desired == UpgradeStatusFailed {
				evType = EventUpgradeFailed
			}
			tx.AddOutbox(mustEvent(evType, c.ID, u.DeviceID, map[string]any{
				"wave":           u.Wave,
				"campaign_ver":   in.CampaignVer,
				"target_version": u.TargetVersion,
				"reason":         in.Reason,
			}))

			if desired == UpgradeStatusSucceeded {
				if d, err := tx.GetDevice(u.DeviceID); err == nil {
					d.CurrentVersion = u.TargetVersion
					d.UpdatedAt = now
					tx.PutDevice(d)
				}
			}
		}

		switch {
		case c.Status != CampaignStatusActive:
			// 暂停/中止/完成后只记录在途结果。rolled_back 后迟到的成功若
			// 属于本批且具备回退条件，在同一事务补发恰好一次回退指令。
			if c.Status == CampaignStatusRolledBack && desired == UpgradeStatusSucceeded {
				s.sweepRollbackCommands(tx, &c)
			}
			return nil
		case desired == UpgradeStatusFailed && c.MaxFailuresPerWave > 0:
			if failed := s.waveFailedCount(tx, c.ID, c.CurrentWave); failed > c.MaxFailuresPerWave {
				// 失败数越过每批上限：原子回退，优先级高于比率暂停/波次推进。
				s.triggerRollback(tx, &c, fmt.Sprintf(
					"wave %d failures %d exceed limit %d",
					c.CurrentWave, failed, c.MaxFailuresPerWave))
				return nil
			}
		}
		// 活动中的当前波次触发结算评估。
		s.evaluateWave(tx, &c)
		return nil
	})
}

// waveFailedCount 统计指定波次已失败（终态）的设备数。
func (s *Service) waveFailedCount(tx TxStore, campaignID string, wave int) int {
	failed := 0
	for _, u := range tx.ListUpgrades(campaignID) {
		if u.Wave == wave && u.Status == UpgradeStatusFailed {
			failed++
		}
	}
	return failed
}

// triggerRollback 在事务内把活动原子转入 rolled_back（终态）：
// 版本号前进、记录原因、释放设备锁、停止后续下发，并立即为本批
// （触发波次）已安装成功且允许回退的设备生成回退指令。
// 以前完成的波次不受影响。仅 active 活动可触发；并发下若活动已被
// 暂停/中止（同一事务的更早提交不可能，但保留防御），本次不生效。
func (s *Service) triggerRollback(tx TxStore, c *Campaign, reason string) {
	if c.Status != CampaignStatusActive {
		return
	}
	now := s.now()
	c.Status = CampaignStatusRolledBack
	c.Version++
	c.UpdatedAt = now
	c.RolledBackAt = now
	c.RollbackReason = reason
	tx.PutCampaign(*c)
	upgrades := tx.ListUpgrades(c.ID)
	for _, u := range upgrades {
		tx.UnlockDevice(u.DeviceID, c.ID)
	}
	tx.AddOutbox(mustEvent(EventCampaignRolledBack, c.ID, "", map[string]any{
		"wave": c.CurrentWave, "reason": reason,
	}))
	s.issueRollbackCommands(tx, c, upgrades)
}

// sweepRollbackCommands 为已处于 rolled_back 的活动补发回退指令：
// 触发后在途设备才回报成功时，或重启恢复/人工重复扫描时调用。
// 设备级 RollbackCommandSent 标记保证每台设备至多生成一次指令。
func (s *Service) sweepRollbackCommands(tx TxStore, c *Campaign) int {
	if c.Status != CampaignStatusRolledBack {
		return 0
	}
	return s.issueRollbackCommands(tx, c, tx.ListUpgrades(c.ID))
}

// issueRollbackCommands 扫描本批（触发时的当前波次）设备，为其中
// 已安装成功、冻结时允许回退且尚未发令的设备各生成一条带版本的回退
// 指令事件。幂等：重复调用不会产生第二条指令。
func (s *Service) issueRollbackCommands(tx TxStore, c *Campaign, upgrades []DeviceUpgrade) int {
	issued := 0
	for i := range upgrades {
		u := upgrades[i]
		if u.Wave != c.CurrentWave {
			continue // 只回退本批；以前完成的批次不动
		}
		if u.Status != UpgradeStatusSucceeded || !u.RollbackAllowed || u.RollbackCommandSent {
			continue
		}
		// 没有可回退的历史版本（理论上资格筛选已排除），跳过。
		if u.FromVersion == "" || u.FromVersion == u.TargetVersion {
			continue
		}
		u.RollbackCommandSent = true
		tx.PutUpgrade(u)
		tx.AddOutbox(mustEvent(EventRollbackCommand, c.ID, u.DeviceID, map[string]any{
			"wave":              u.Wave,
			"campaign_ver":      c.Version,
			"installed_version": u.TargetVersion, // 已装上的（坏）版本
			"rollback_version":  u.FromVersion,   // 要回到的冻结历史版本
			"idempotency_key":   rollbackKey(c.ID, u.DeviceID),
			"reason":            c.RollbackReason,
		}))
		issued++
	}
	return issued
}

// rollbackKey 是回退指令的稳定幂等键，与升级指令区分；
// 同一 (活动,设备) 的回退指令永远只有同一把键。
func rollbackKey(campaignID, deviceID string) string {
	return fmt.Sprintf("rb:%s:%s", campaignID, deviceID)
}

// ScanRollbackCommands 是补偿扫描入口（可定时调用，也可在进程重启
// 恢复后调用）：为 rolled_back 活动中已具备回退条件但尚未发令的设备
// 补发回退指令，返回本次新发令的设备数。非 rolled_back 活动返回 0。
// 整个扫描在单个串行事务内完成，设备级标记保证重复扫描不重复发令。
func (s *Service) ScanRollbackCommands(campaignID string) (int, error) {
	if campaignID == "" {
		return 0, wrapError("ScanRollbackCommands", ErrInvalidArgument, "campaign id is required")
	}
	issued := 0
	err := s.repo.UpdateTx(func(tx TxStore) error {
		c, err := tx.GetCampaign(campaignID)
		if err != nil {
			return wrapError("ScanRollbackCommands", ErrNotFound, "campaign %q", campaignID)
		}
		issued = s.sweepRollbackCommands(tx, &c)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return issued, nil
}

// evaluateWave 必须在事务内调用：当前波全部终态时，按冻结分母计算
// 成功率并推进波次或自动暂停。
func (s *Service) evaluateWave(tx TxStore, c *Campaign) {
	upgrades := tx.ListUpgrades(c.ID)
	total, succeeded, failed := 0, 0, 0
	for _, u := range upgrades {
		if u.Wave != c.CurrentWave {
			continue
		}
		total++
		switch u.Status {
		case UpgradeStatusSucceeded:
			succeeded++
		case UpgradeStatusFailed:
			failed++
		}
	}
	if total == 0 || succeeded+failed < total {
		return // 波次尚未收敛
	}
	now := s.now()
	successRate := float64(succeeded) / float64(total)
	failureRate := float64(failed) / float64(total)

	if failureRate > c.MaxFailureRate {
		c.Status = CampaignStatusPaused
		c.Version++
		c.UpdatedAt = now
		c.PauseReason = fmt.Sprintf("wave %d failure rate %.2f exceeds threshold %.2f",
			c.CurrentWave, failureRate, c.MaxFailureRate)
		tx.PutCampaign(*c)
		tx.AddOutbox(mustEvent(EventCampaignPaused, c.ID, "", map[string]any{
			"wave": c.CurrentWave, "reason": c.PauseReason, "auto": true,
		}))
		return
	}
	if successRate < c.SuccessRate {
		c.Status = CampaignStatusPaused
		c.Version++
		c.UpdatedAt = now
		c.PauseReason = fmt.Sprintf("wave %d success rate %.2f below threshold %.2f",
			c.CurrentWave, successRate, c.SuccessRate)
		tx.PutCampaign(*c)
		tx.AddOutbox(mustEvent(EventCampaignPaused, c.ID, "", map[string]any{
			"wave": c.CurrentWave, "reason": c.PauseReason, "auto": true,
		}))
		return
	}

	// 达标：开放下一波；最后一波则活动收敛完成。
	if c.CurrentWave < c.TotalWaves {
		c.CurrentWave++
		c.Version++
		c.UpdatedAt = now
		c.PauseReason = ""
		tx.PutCampaign(*c)
		tx.AddOutbox(mustEvent(EventWaveAdvanced, c.ID, "", map[string]any{
			"wave": c.CurrentWave, "success_rate": successRate,
		}))
		return
	}

	// 全部波次达标完成：进入 completed 终态并释放设备锁。
	c.Status = CampaignStatusCompleted
	c.Version++
	c.UpdatedAt = now
	tx.PutCampaign(*c)
	for _, u := range upgrades {
		tx.UnlockDevice(u.DeviceID, c.ID)
	}
	tx.AddOutbox(mustEvent(EventCampaignCompleted, c.ID, "", map[string]any{
		"success_rate": successRate,
	}))
}

// ---- 暂停 / 恢复 / 中止 ----

// Pause 手动暂停（仅 active → paused）。
func (s *Service) Pause(campaignID, reason string) error {
	if campaignID == "" {
		return wrapError("Pause", ErrInvalidArgument, "campaign id is required")
	}
	return s.repo.UpdateTx(func(tx TxStore) error {
		c, err := tx.GetCampaign(campaignID)
		if err != nil {
			return wrapError("Pause", ErrNotFound, "campaign %q", campaignID)
		}
		if c.Status != CampaignStatusActive {
			return wrapError("Pause", ErrInvalidTransition, "campaign is %s", c.Status)
		}
		now := s.now()
		c.Status = CampaignStatusPaused
		c.Version++
		c.UpdatedAt = now
		c.PauseReason = reason
		tx.PutCampaign(c)
		tx.AddOutbox(mustEvent(EventCampaignPaused, c.ID, "", map[string]any{
			"reason": reason, "auto": false,
		}))
		return nil
	})
}

// Resume 恢复活动（仅 paused → active）。暂停期间在途回执仍会被接受，
// 因此恢复时若当前波次已经收敛：
//   - 达标：正常开放下一波（或完成活动）；
//   - 未达标：运维显式恢复即视为确认兜底，放行下一波（或完成活动），
//     并在事件中标注 operator_override。
//
// 波次未收敛时仅恢复 active，等待剩余回执。
func (s *Service) Resume(campaignID string) error {
	if campaignID == "" {
		return wrapError("Resume", ErrInvalidArgument, "campaign id is required")
	}
	return s.repo.UpdateTx(func(tx TxStore) error {
		c, err := tx.GetCampaign(campaignID)
		if err != nil {
			return wrapError("Resume", ErrNotFound, "campaign %q", campaignID)
		}
		if c.Status != CampaignStatusPaused {
			return wrapError("Resume", ErrInvalidTransition, "campaign is %s", c.Status)
		}
		now := s.now()
		c.Status = CampaignStatusActive
		c.Version++
		c.UpdatedAt = now
		c.PauseReason = ""
		tx.PutCampaign(c)
		tx.AddOutbox(mustEvent(EventCampaignResumed, c.ID, "", nil))

		if !s.waveSettled(tx, c.ID, c.CurrentWave) {
			return nil // 等待剩余回执
		}
		// 波次已收敛：达标则正常推进，未达标则运维确认放行。
		total, succeeded, failed := s.waveCounts(tx, c.ID, c.CurrentWave)
		successRate := float64(succeeded) / float64(total)
		failureRate := float64(failed) / float64(total)
		if failureRate <= c.MaxFailureRate && successRate >= c.SuccessRate {
			s.evaluateWave(tx, &c)
		} else {
			s.advanceOverride(tx, &c)
		}
		return nil
	})
}

func (s *Service) waveSettled(tx TxStore, campaignID string, wave int) bool {
	total, terminal := 0, 0
	for _, u := range tx.ListUpgrades(campaignID) {
		if u.Wave != wave {
			continue
		}
		total++
		if u.Status.Terminal() {
			terminal++
		}
	}
	return total > 0 && total == terminal
}

func (s *Service) waveCounts(tx TxStore, campaignID string, wave int) (total, succeeded, failed int) {
	for _, u := range tx.ListUpgrades(campaignID) {
		if u.Wave != wave {
			continue
		}
		total++
		switch u.Status {
		case UpgradeStatusSucceeded:
			succeeded++
		case UpgradeStatusFailed:
			failed++
		}
	}
	return total, succeeded, failed
}

// advanceOverride 运维恢复后跳过门槛检查推进波次。
func (s *Service) advanceOverride(tx TxStore, c *Campaign) {
	now := s.now()
	if c.CurrentWave < c.TotalWaves {
		c.CurrentWave++
		c.Version++
		c.UpdatedAt = now
		tx.PutCampaign(*c)
		tx.AddOutbox(mustEvent(EventWaveAdvanced, c.ID, "", map[string]any{
			"wave": c.CurrentWave, "operator_override": true,
		}))
		return
	}
	c.Status = CampaignStatusCompleted
	c.Version++
	c.UpdatedAt = now
	tx.PutCampaign(*c)
	for _, u := range tx.ListUpgrades(c.ID) {
		tx.UnlockDevice(u.DeviceID, c.ID)
	}
	tx.AddOutbox(mustEvent(EventCampaignCompleted, c.ID, "", map[string]any{
		"operator_override": true,
	}))
}

// Abort 中止活动。active/paused → aborted：版本号前进、释放全部
// 设备锁、停止下发新指令；在途回执仍可到达且状态单调不变。
func (s *Service) Abort(campaignID, reason string) error {
	if campaignID == "" {
		return wrapError("Abort", ErrInvalidArgument, "campaign id is required")
	}
	return s.repo.UpdateTx(func(tx TxStore) error {
		c, err := tx.GetCampaign(campaignID)
		if err != nil {
			return wrapError("Abort", ErrNotFound, "campaign %q", campaignID)
		}
		if c.Status == CampaignStatusAborted ||
			c.Status == CampaignStatusCompleted ||
			c.Status == CampaignStatusRolledBack {
			return wrapError("Abort", ErrInvalidTransition, "campaign is %s", c.Status)
		}
		now := s.now()
		c.Status = CampaignStatusAborted
		c.Version++
		c.UpdatedAt = now
		c.AbortedAt = now
		tx.PutCampaign(c)
		for _, u := range tx.ListUpgrades(campaignID) {
			tx.UnlockDevice(u.DeviceID, campaignID)
		}
		tx.AddOutbox(mustEvent(EventCampaignAborted, c.ID, "", map[string]any{
			"reason": reason,
		}))
		return nil
	})
}

// ---- 查询 ----

// GetCampaign 返回活动当前状态。
func (s *Service) GetCampaign(id string) (Campaign, error) {
	if id == "" {
		return Campaign{}, wrapError("GetCampaign", ErrInvalidArgument, "campaign id is required")
	}
	c, err := s.repo.GetCampaign(id)
	if err != nil {
		return Campaign{}, wrapError("GetCampaign", ErrNotFound, "campaign %q", id)
	}
	return c, nil
}

// GetProgress 按冻结的波次分母计算活动与每个波次的实时进度。
func (s *Service) GetProgress(campaignID string) (Progress, error) {
	if campaignID == "" {
		return Progress{}, wrapError("GetProgress", ErrInvalidArgument, "campaign id is required")
	}
	c, err := s.repo.GetCampaign(campaignID)
	if err != nil {
		return Progress{}, wrapError("GetProgress", ErrNotFound, "campaign %q", campaignID)
	}
	snap, err := s.repo.GetSnapshot(campaignID)
	if err != nil {
		return Progress{}, wrapError("GetProgress", ErrNotFound, "snapshot for campaign %q", campaignID)
	}
	upgrades := s.repo.ListUpgrades(campaignID)
	byDevice := make(map[string]DeviceUpgrade, len(upgrades))
	for _, u := range upgrades {
		byDevice[u.DeviceID] = u
	}

	p := Progress{
		CampaignID:  c.ID,
		Status:      c.Status,
		CurrentWave: c.CurrentWave,
		TotalWaves:  c.TotalWaves,
		Version:     c.Version,
		PauseReason: c.PauseReason,
		Total:       len(snap.DeviceIDs),
	}
	p.Waves = make([]WaveStat, c.TotalWaves)
	for idx, ids := range snap.Waves {
		st := WaveStat{Wave: idx + 1, Total: len(ids), Open: idx+1 == c.CurrentWave && c.Status == CampaignStatusActive}
		for _, id := range ids {
			u := byDevice[id]
			switch u.Status {
			case UpgradeStatusSucceeded:
				st.Succeeded++
			case UpgradeStatusFailed:
				st.Failed++
			default:
				st.InProgress++
			}
		}
		if st.Total > 0 {
			st.SuccessRate = float64(st.Succeeded) / float64(st.Total)
		}
		p.Succeeded += st.Succeeded
		p.Failed += st.Failed
		p.InProgress += st.InProgress
		p.Waves[idx] = st
	}
	if p.Total > 0 {
		p.SuccessRate = float64(p.Succeeded) / float64(p.Total)
	}
	p.Finished = c.Status == CampaignStatusCompleted
	if c.Status == CampaignStatusRolledBack {
		p.RollbackReason = c.RollbackReason
		// 只报告触发波次（本批）的设备分类；以前完成的批次不受影响。
		for _, u := range upgrades {
			if u.Wave != c.CurrentWave {
				continue
			}
			switch {
			case u.Status == UpgradeStatusSucceeded && u.RollbackAllowed && u.RollbackCommandSent:
				p.RollbackCommanded = append(p.RollbackCommanded, u.DeviceID)
			case u.Status == UpgradeStatusSucceeded && u.RollbackAllowed:
				p.RollbackWaiting = append(p.RollbackWaiting, u.DeviceID)
			case u.Status == UpgradeStatusSucceeded:
				p.InstalledNotRollbackable = append(p.InstalledNotRollbackable, u.DeviceID)
			case !u.Status.Terminal():
				p.RollbackInFlight = append(p.RollbackInFlight, u.DeviceID)
			}
		}
		sort.Strings(p.RollbackCommanded)
		sort.Strings(p.RollbackWaiting)
		sort.Strings(p.InstalledNotRollbackable)
		sort.Strings(p.RollbackInFlight)
	}
	return p, nil
}

// ---- Outbox ----

// DispatchOutbox 将所有待投递事件交给 handler 处理；handler 返回 nil
// 后事件才标记为已投递。至少一次语义：handler 失败或进程崩溃后，
// 事件仍保留在 outbox 中等待重试。事件在同一事务内与业务状态写入。
func (s *Service) DispatchOutbox(ctx context.Context, handler func(context.Context, OutboxEvent) error) (int, error) {
	pending := s.repo.ListPendingOutbox()
	dispatched := 0
	for _, e := range pending {
		if ctx.Err() != nil {
			return dispatched, ctx.Err()
		}
		if err := handler(ctx, e); err != nil {
			return dispatched, err
		}
		if err := s.repo.MarkOutboxDispatched(e.ID); err != nil {
			return dispatched, err
		}
		dispatched++
	}
	return dispatched, nil
}

// RunDispatcher 按间隔轮询 outbox 直到 ctx 取消。
func (s *Service) RunDispatcher(ctx context.Context, interval time.Duration, handler func(context.Context, OutboxEvent) error) {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = s.DispatchOutbox(ctx, handler)
		}
	}
}

// ListPendingOutbox 暴露待投递事件，便于测试与自定义投递循环。
func (s *Service) ListPendingOutbox() []OutboxEvent {
	return s.repo.ListPendingOutbox()
}

func mustEvent(eventType, campaignID, deviceID string, payload map[string]any) *OutboxEvent {
	var body []byte
	if payload != nil {
		body, _ = json.Marshal(payload)
	}
	return &OutboxEvent{
		EventType:  eventType,
		CampaignID: campaignID,
		DeviceID:   deviceID,
		Payload:    body,
	}
}
