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
	if in.MaxWaveFailures < 0 {
		return Campaign{}, CampaignSnapshot{}, wrapError("CreateCampaign", ErrInvalidArgument, "max wave failures must be >= 0")
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
		var busy []string
		selected := map[string]Device{}
		for _, d := range tx.ListDevices() {
			if !qualified(d, sel, fw.Version, allowed) {
				continue
			}
			if owner, locked := tx.DeviceLock(d.ID); locked && owner != in.ID {
				busy = append(busy, d.ID)
				continue
			}
			deviceIDs = append(deviceIDs, d.ID)
			selected[d.ID] = d
		}
		if len(deviceIDs) == 0 {
			return wrapError("CreateCampaign", ErrConflict,
				"no qualified devices (model=%s, qualified-but-busy=%d)", sel.Model, len(busy))
		}

		waves := splitWaves(deviceIDs, in.BatchSize)
		now := s.now()
		c := Campaign{
			ID:              in.ID,
			FirmwareID:      fw.ID,
			Model:           fw.Model,
			TargetVersion:   fw.Version,
			FirmwareDigest:  fw.Checksum(),
			Status:          CampaignStatusActive,
			CurrentWave:     1,
			TotalWaves:      len(waves),
			Version:         1,
			SuccessRate:     in.SuccessRateThreshold,
			MaxFailureRate:  in.MaxFailureRate,
			MaxWaveFailures: in.MaxWaveFailures,
			BatchSize:       in.BatchSize,
			CreatedAt:       now,
			UpdatedAt:       now,
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
				d := selected[id]
				u := DeviceUpgrade{
					DeviceID:        id,
					CampaignID:      c.ID,
					Wave:            waveIdx + 1,
					Status:          UpgradeStatusPending,
					TargetVersion:   c.TargetVersion,
					IdempotencyKey:  stableKey(c.ID, id),
					FromVersion:     d.CurrentVersion,
					RollbackAllowed: d.RollbackSupported,
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
//     失败超阈值则自动暂停。
//
// 活动中止后仍接受在途回执（只是不再下发新指令）。
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

		// 仅活动中的当前波次触发结算评估；中止/回退后只记录结果。
		if c.Status == CampaignStatusActive {
			// 失败计数熔断优先：越过本批失败上限的同一事务内原子停止下发
			// 并生成回退指令，此后不再做波次推进/自动暂停评估。
			if s.triggerRollbackIfExceeded(tx, &c) {
				return nil
			}
			s.evaluateWave(tx, &c)
		}
		return nil
	})
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

// ---- 自动回退（失败计数熔断）----

// triggerRollbackIfExceeded 必须在事务内、活动处于 active 时调用。
// 当前波失败台数严格越过 MaxWaveFailures 时原子进入 rolling_back：
// 停止全部后续下发、对本批设备做一次性分类、为已安装成功且允许回退者
// 生成回退指令。返回 true 表示已触发（调用方不得再做波次推进评估）。
func (s *Service) triggerRollbackIfExceeded(tx TxStore, c *Campaign) bool {
	if c.MaxWaveFailures <= 0 {
		return false
	}
	_, _, failed := s.waveCounts(tx, c.ID, c.CurrentWave)
	if failed <= c.MaxWaveFailures {
		return false
	}
	s.enterRollback(tx, c, failed)
	return true
}

// enterRollback 在当前事务内把活动切到 rolling_back，并对当前波设备做
// 一次性、冻结的分类。以前波次不受影响。
func (s *Service) enterRollback(tx TxStore, c *Campaign, failed int) {
	now := s.now()
	c.Status = CampaignStatusRollingBack
	c.Version++
	c.UpdatedAt = now
	c.RollbackWave = c.CurrentWave
	c.RollbackReason = fmt.Sprintf("wave %d failures %d exceed limit %d",
		c.CurrentWave, failed, c.MaxWaveFailures)

	installed, inProgress := 0, 0
	for _, u := range tx.ListUpgrades(c.ID) {
		if u.Wave != c.CurrentWave {
			continue // 以前完成的批次不受影响
		}
		switch u.Status {
		case UpgradeStatusSucceeded:
			installed++
			if u.RollbackAllowed && u.FromVersion != "" {
				u.RollbackState = RollbackStateCommand
			} else {
				u.RollbackState = RollbackStateSkipped
			}
		case UpgradeStatusFailed:
			u.RollbackState = RollbackStateFailed
		default: // pending / issued：触发时仍在处理中
			inProgress++
			u.RollbackState = RollbackStateInProgress
		}
		tx.PutUpgrade(u)
	}

	tx.PutCampaign(*c)
	notified := s.emitRollbackCommands(tx, *c)
	// 活动进入终态：与中止一致释放全部设备锁（在途回执仍会被接受）。
	for _, u := range tx.ListUpgrades(c.ID) {
		tx.UnlockDevice(u.DeviceID, c.ID)
	}
	tx.AddOutbox(mustEvent(EventWaveRollingBack, c.ID, "", map[string]any{
		"wave":        c.RollbackWave,
		"reason":      c.RollbackReason,
		"installed":   installed,
		"in_progress": inProgress,
		"failed":      failed,
		"notified":    notified,
	}))
}

// emitRollbackCommands 为所有已分类为 command 且尚未通知的设备生成带版本
// 的回退指令，设置 RollbackNotified 并写入 outbox。触发与（崩溃恢复/重复
// 扫描的）补偿走同一条幂等路径，因此每台设备至多发令一次。返回新发数。
func (s *Service) emitRollbackCommands(tx TxStore, c Campaign) int {
	notified := 0
	for _, u := range tx.ListUpgrades(c.ID) {
		if u.RollbackState != RollbackStateCommand || u.RollbackNotified {
			continue
		}
		u.RollbackNotified = true
		tx.PutUpgrade(u)
		tx.AddOutbox(mustEvent(EventRollbackCommand, c.ID, u.DeviceID, map[string]any{
			"wave":            u.Wave,
			"from_version":    u.TargetVersion,
			"to_version":      u.FromVersion,
			"campaign_ver":    c.Version,
			"idempotency_key": rollbackKey(c.ID, u.DeviceID),
		}))
		notified++
	}
	return notified
}

// ScanRollbackCommands 是补偿扫描入口（可被定时任务/重启恢复反复调用）：
// 补发任何已分类但尚未通知的回退指令。正常路径上触发与通知在同一事务
// 完成，因此扫描通常返回 0；它保证异常恢复时也不会重复发令。
func (s *Service) ScanRollbackCommands(campaignID string) (int, error) {
	if campaignID == "" {
		return 0, wrapError("ScanRollbackCommands", ErrInvalidArgument, "campaign id is required")
	}
	notified := 0
	err := s.repo.UpdateTx(func(tx TxStore) error {
		c, err := tx.GetCampaign(campaignID)
		if err != nil {
			return wrapError("ScanRollbackCommands", ErrNotFound, "campaign %q", campaignID)
		}
		notified = s.emitRollbackCommands(tx, c)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return notified, nil
}

// rollbackKey 是回退指令的稳定幂等键，与升级指令键区分。
func rollbackKey(campaignID, deviceID string) string {
	return fmt.Sprintf("rb:%s:%s", campaignID, deviceID)
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

		// 放行前先判断（含暂停期间累积的）失败是否已越过失败上限：若是则
		// paused → rolling_back 单一原子结果，不发 resumed、不恢复下发。
		// 判定不依赖波次收敛，未收敛时 pending/issued 设备归为处理中。
		if s.triggerRollbackIfExceeded(tx, &c) {
			return nil
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
// 已进入 rolling_back（自动回退终态）或 completed 的活动不可中止——
// 自动回退与人工中止并发时只保留先提交的那一种结果。
func (s *Service) Abort(campaignID, reason string) error {
	if campaignID == "" {
		return wrapError("Abort", ErrInvalidArgument, "campaign id is required")
	}
	return s.repo.UpdateTx(func(tx TxStore) error {
		c, err := tx.GetCampaign(campaignID)
		if err != nil {
			return wrapError("Abort", ErrNotFound, "campaign %q", campaignID)
		}
		if c.Status.Terminal() {
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
		// 触发回退的波次：附触发瞬间冻结的设备分类。
		if c.RollbackWave == st.Wave && c.RollbackWave > 0 {
			wr := &WaveRollback{Triggered: true}
			for _, id := range ids {
				switch byDevice[id].RollbackState {
				case RollbackStateCommand, RollbackStateSkipped:
					wr.Installed++
				case RollbackStateInProgress:
					wr.InProgress++
				case RollbackStateFailed:
					wr.Failed++
				}
				if byDevice[id].RollbackNotified {
					wr.Notified++
				}
			}
			st.Rollback = wr
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
	if c.Status == CampaignStatusRollingBack {
		p.Rollback = s.buildRollbackReport(c, byDevice)
	}
	return p, nil
}

// buildRollbackReport 依据冻结的设备分类构建回退报告与带版本回退指令清单。
func (s *Service) buildRollbackReport(c Campaign, byDevice map[string]DeviceUpgrade) *ProgressRollback {
	r := &ProgressRollback{Wave: c.RollbackWave, Reason: c.RollbackReason}
	for _, u := range byDevice {
		if u.Wave != c.RollbackWave {
			continue
		}
		switch u.RollbackState {
		case RollbackStateCommand:
			r.Installed++
			if u.RollbackNotified {
				r.Notified++
			}
			r.Commands = append(r.Commands, RollbackCommand{
				CampaignID:     c.ID,
				DeviceID:       u.DeviceID,
				Wave:           u.Wave,
				FromVersion:    u.TargetVersion,
				ToVersion:      u.FromVersion,
				CampaignVer:    c.Version,
				IdempotencyKey: rollbackKey(c.ID, u.DeviceID),
			})
		case RollbackStateSkipped:
			r.Installed++
			r.Skipped = append(r.Skipped, u.DeviceID)
		case RollbackStateInProgress:
			r.InProgress++
		case RollbackStateFailed:
			r.Failed++
		}
	}
	sort.Slice(r.Commands, func(i, j int) bool { return r.Commands[i].DeviceID < r.Commands[j].DeviceID })
	sort.Strings(r.Skipped)
	return r
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
