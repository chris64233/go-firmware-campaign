package firmwarecampaign

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// Service 编排分批固件升级活动。所有方法均并发安全：
// 状态变更在单把互斥锁下完成，因此暂停/恢复/中止与回执并发时状态保持单调。
type Service struct {
	mu   sync.Mutex
	st   *snapshot
	pers Persister
	now  func() time.Time
}

// NewService 创建服务。pers 为 nil 时状态仅保存在内存；
// 否则从 pers 加载既有状态（含 outbox），并在每次变更后写回。
func NewService(pers Persister) (*Service, error) {
	st := &snapshot{}
	st.ensure()
	if pers != nil {
		loaded, err := pers.Load()
		if err != nil {
			return nil, fmt.Errorf("load state: %w", err)
		}
		if loaded != nil {
			st = loaded
			st.ensure()
		}
	}
	return &Service{st: st, pers: pers, now: time.Now}, nil
}

func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

// commandKey 生成稳定的指令幂等键：同一 (活动, 版本, 设备) 永远相同。
func commandKey(campaignID string, version int, deviceID string) string {
	return fmt.Sprintf("%s:%d:%s", campaignID, version, deviceID)
}

// persist 在持锁状态下写回快照。
func (s *Service) persist() error {
	if s.pers == nil {
		return nil
	}
	if err := s.pers.Save(s.st); err != nil {
		return fmt.Errorf("persist state: %w", err)
	}
	return nil
}

func (s *Service) emit(ev OutboxEvent) {
	ev.ID = newID("evt")
	ev.CreatedAt = s.now()
	s.st.Outbox = append(s.st.Outbox, ev)
}

// RegisterFirmware 登记一个固件版本。
func (s *Service) RegisterFirmware(fw Firmware) (*Firmware, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if fw.ID == "" || fw.Model == "" || fw.Version == "" || fw.Digest == "" {
		return nil, fmt.Errorf("%w: firmware id, model, version and digest are required", ErrInvalidRequest)
	}
	if fw.MinBattery < 0 || fw.MinBattery > 100 {
		return nil, fmt.Errorf("%w: min battery must be in [0, 100]", ErrInvalidRequest)
	}
	if _, ok := s.st.Firmwares[fw.ID]; ok {
		return nil, fmt.Errorf("%w: %s", ErrFirmwareExists, fw.ID)
	}
	cp := fw
	cp.CreatedAt = s.now()
	s.st.Firmwares[cp.ID] = &cp
	if err := s.persist(); err != nil {
		return nil, err
	}
	out := cp
	return &out, nil
}

// RegisterDevice 登记（或更新）一台设备的资格属性。
func (s *Service) RegisterDevice(d Device) (*Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if d.ID == "" || d.Model == "" || d.CurrentVersion == "" {
		return nil, fmt.Errorf("%w: device id, model and current version are required", ErrInvalidRequest)
	}
	if d.Battery < 0 || d.Battery > 100 {
		return nil, fmt.Errorf("%w: battery must be in [0, 100]", ErrInvalidRequest)
	}
	cp := d
	cp.UpdatedAt = s.now()
	s.st.Devices[cp.ID] = &cp
	if err := s.persist(); err != nil {
		return nil, err
	}
	out := cp
	return &out, nil
}

// CreateCampaign 创建活动：校验设备资格与互斥约束后，
// 冻结目标设备、固件摘要与波次划分。
func (s *Service) CreateCampaign(req CreateCampaignRequest) (*Campaign, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fw, ok := s.st.Firmwares[req.FirmwareID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrFirmwareNotFound, req.FirmwareID)
	}
	if req.SuccessThreshold <= 0 || req.SuccessThreshold > 1 {
		return nil, fmt.Errorf("%w: success threshold must be in (0, 1]", ErrInvalidRequest)
	}
	if req.FailureThreshold < 0 || req.FailureThreshold > 1 {
		return nil, fmt.Errorf("%w: failure threshold must be in [0, 1] (1 disables auto-pause)", ErrInvalidRequest)
	}
	if len(req.Waves) == 0 {
		return nil, fmt.Errorf("%w: campaign must contain at least one wave", ErrInvalidRequest)
	}

	// 校验波次划分：非空、无重复、设备存在且满足资格条件。
	seen := map[string]bool{}
	ineligible := map[string][]string{}
	for i, wave := range req.Waves {
		if len(wave) == 0 {
			return nil, fmt.Errorf("%w: wave %d is empty", ErrInvalidRequest, i)
		}
		for _, devID := range wave {
			if seen[devID] {
				return nil, fmt.Errorf("%w: device %s appears in more than one wave", ErrInvalidRequest, devID)
			}
			seen[devID] = true
			dev, ok := s.st.Devices[devID]
			if !ok {
				ineligible[devID] = append(ineligible[devID], "device not registered")
				continue
			}
			if dev.Model != fw.Model {
				ineligible[devID] = append(ineligible[devID],
					fmt.Sprintf("model %q does not match firmware model %q", dev.Model, fw.Model))
			}
			if dev.CurrentVersion == fw.Version {
				ineligible[devID] = append(ineligible[devID],
					fmt.Sprintf("already running target version %q", fw.Version))
			}
			if dev.Battery < fw.MinBattery {
				ineligible[devID] = append(ineligible[devID],
					fmt.Sprintf("battery %d%% below required %d%%", dev.Battery, fw.MinBattery))
			}
		}
	}
	if len(ineligible) > 0 {
		return nil, &IneligibilityError{Reasons: ineligible}
	}

	// 互斥约束：同一设备不能同时加入两个未终结的活动。
	conflicts := map[string]string{}
	for _, c := range s.st.Campaigns {
		if c.Status.Terminal() {
			continue
		}
		for devID := range c.Devices {
			if seen[devID] {
				conflicts[devID] = c.ID
			}
		}
	}
	if len(conflicts) > 0 {
		return nil, &ConflictError{Devices: conflicts}
	}

	now := s.now()
	id := req.ID
	if id == "" {
		id = newID("cmp")
	} else if _, ok := s.st.Campaigns[id]; ok {
		return nil, fmt.Errorf("%w: campaign id %s already exists", ErrInvalidRequest, id)
	}

	c := &Campaign{
		ID:               id,
		FirmwareID:       fw.ID,
		FirmwareVersion:  fw.Version,
		FirmwareDigest:   fw.Digest, // 冻结固件摘要
		Status:           StatusRunning,
		Version:          1,
		CurrentWave:      0,
		SuccessThreshold: req.SuccessThreshold,
		FailureThreshold: req.FailureThreshold,
		Devices:          map[string]*CampaignDevice{},
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	// 冻结波次划分（深拷贝，调用方后续修改不影响活动）。
	c.Waves = make([][]string, len(req.Waves))
	for i, wave := range req.Waves {
		c.Waves[i] = append([]string(nil), wave...)
		for _, devID := range wave {
			c.Devices[devID] = &CampaignDevice{
				DeviceID:  devID,
				Wave:      i,
				State:     DevicePending,
				UpdatedAt: now,
			}
		}
	}
	s.st.Campaigns[c.ID] = c
	s.emit(OutboxEvent{
		Type:       EventCampaignCreated,
		CampaignID: c.ID,
		Payload: map[string]string{
			"firmware_id": fw.ID,
			"digest":      fw.Digest,
			"waves":       fmt.Sprintf("%d", len(c.Waves)),
		},
	})
	if err := s.persist(); err != nil {
		return nil, err
	}
	out := *c
	return &out, nil
}

// ClaimCommand 由设备领取升级指令。只有当前波次的设备可以领取；
// 重复领取返回相同的幂等键（领取本身幂等）。
func (s *Service) ClaimCommand(campaignID, deviceID string) (*Command, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.st.Campaigns[campaignID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrCampaignNotFound, campaignID)
	}
	if c.Status != StatusRunning {
		return nil, fmt.Errorf("%w: campaign %s is %s", ErrCampaignNotRunning, campaignID, c.Status)
	}
	dev, ok := c.Devices[deviceID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrDeviceNotInCampaign, deviceID)
	}
	if dev.Wave != c.CurrentWave {
		return nil, fmt.Errorf("%w: device %s is in wave %d, current wave is %d",
			ErrNotCurrentWave, deviceID, dev.Wave, c.CurrentWave)
	}
	switch dev.State {
	case DeviceSucceeded, DeviceFailed:
		return nil, fmt.Errorf("%w: device %s is %s", ErrDeviceFinished, deviceID, dev.State)
	}

	now := s.now()
	if dev.State == DevicePending {
		dev.State = DeviceDispatched
		dev.UpdatedAt = now
		c.UpdatedAt = now
		if err := s.persist(); err != nil {
			return nil, err
		}
	}
	return &Command{
		CampaignID:      c.ID,
		CampaignVersion: c.Version,
		DeviceID:        deviceID,
		FirmwareID:      c.FirmwareID,
		FirmwareVersion: c.FirmwareVersion,
		Digest:          c.FirmwareDigest,
		IdempotencyKey:  commandKey(c.ID, c.Version, deviceID),
		IssuedAt:        now,
	}, nil
}

// ReportReceipt 处理设备回执。回执可重复、可乱序：
//   - 相同回执 ID 直接判重；
//   - 旧活动版本的回执被忽略，不得覆盖新状态；
//   - 设备状态按秩单调推进，已成功的设备不会被迟到失败回退。
func (s *Service) ReportReceipt(r Receipt) (ReceiptResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if r.ID == "" || r.CampaignID == "" || r.DeviceID == "" {
		return "", fmt.Errorf("%w: receipt id, campaign id and device id are required", ErrInvalidRequest)
	}
	c, ok := s.st.Campaigns[r.CampaignID]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrCampaignNotFound, r.CampaignID)
	}
	dev, ok := c.Devices[r.DeviceID]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrDeviceNotInCampaign, r.DeviceID)
	}
	if s.st.Receipts[r.ID] {
		return ReceiptDuplicate, nil
	}
	if r.CampaignVersion > c.Version {
		return "", fmt.Errorf("%w: receipt version %d > campaign version %d",
			ErrFutureVersion, r.CampaignVersion, c.Version)
	}
	if r.CampaignVersion < c.Version {
		// 旧活动版本的回执：记录为已处理（去重），但不触碰状态。
		s.st.Receipts[r.ID] = true
		if err := s.persist(); err != nil {
			return "", err
		}
		return ReceiptStale, nil
	}
	if r.IdempotencyKey != "" && r.IdempotencyKey != commandKey(c.ID, c.Version, r.DeviceID) {
		return "", fmt.Errorf("%w: device %s", ErrKeyMismatch, r.DeviceID)
	}

	s.st.Receipts[r.ID] = true

	var newState DeviceState
	if r.Success {
		newState = DeviceSucceeded
	} else {
		newState = DeviceFailed
	}
	result := ReceiptApplied
	if deviceStateRank(newState) > deviceStateRank(dev.State) {
		dev.State = newState
		dev.UpdatedAt = s.now()
		c.UpdatedAt = s.now()
	} else {
		result = ReceiptSuperseded
	}

	// 仅运行中的活动根据回执推进波次/自动暂停；终态活动只记录设备终态。
	if c.Status == StatusRunning {
		s.evaluateWave(c)
	}
	if err := s.persist(); err != nil {
		return "", err
	}
	return result, nil
}

// evaluateWave 按冻结的波次分母评估当前波次：
// 失败率超过阈值则自动暂停；成功率达到门槛则开放下一波或完成活动。
// 调用时必须持锁且活动处于 running。
func (s *Service) evaluateWave(c *Campaign) {
	wave := c.Waves[c.CurrentWave]
	var succ, fail int
	for _, devID := range wave {
		switch c.Devices[devID].State {
		case DeviceSucceeded:
			succ++
		case DeviceFailed:
			fail++
		}
	}
	total := float64(len(wave))
	if float64(fail)/total > c.FailureThreshold {
		c.Status = StatusPaused
		c.PauseReason = fmt.Sprintf("failure rate %.2f exceeded threshold %.2f in wave %d",
			float64(fail)/total, c.FailureThreshold, c.CurrentWave)
		s.emit(OutboxEvent{
			Type:       EventCampaignPaused,
			CampaignID: c.ID,
			Payload:    map[string]string{"reason": c.PauseReason, "auto": "true"},
		})
		return
	}
	if float64(succ)/total >= c.SuccessThreshold {
		if c.CurrentWave == len(c.Waves)-1 {
			c.Status = StatusCompleted
			s.emit(OutboxEvent{Type: EventCampaignComplete, CampaignID: c.ID})
			return
		}
		c.CurrentWave++
		s.emit(OutboxEvent{
			Type:       EventWaveAdvanced,
			CampaignID: c.ID,
			Payload:    map[string]string{"current_wave": fmt.Sprintf("%d", c.CurrentWave)},
		})
	}
}

// PauseCampaign 手动暂停活动（运行中 -> 暂停）。
func (s *Service) PauseCampaign(campaignID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.st.Campaigns[campaignID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrCampaignNotFound, campaignID)
	}
	if c.Status.Terminal() {
		return fmt.Errorf("%w: campaign %s is %s", ErrCampaignTerminal, campaignID, c.Status)
	}
	if c.Status != StatusRunning {
		return fmt.Errorf("%w: campaign %s is %s", ErrCampaignNotRunning, campaignID, c.Status)
	}
	c.Status = StatusPaused
	c.PauseReason = "paused by operator"
	c.UpdatedAt = s.now()
	s.emit(OutboxEvent{
		Type:       EventCampaignPaused,
		CampaignID: c.ID,
		Payload:    map[string]string{"reason": c.PauseReason, "auto": "false"},
	})
	return s.persist()
}

// ResumeCampaign 恢复暂停的活动：活动版本号递增，
// 使恢复前发出的旧版本回执失效，并重新评估当前波次。
func (s *Service) ResumeCampaign(campaignID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.st.Campaigns[campaignID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrCampaignNotFound, campaignID)
	}
	if c.Status != StatusPaused {
		return fmt.Errorf("%w: campaign %s is %s", ErrCampaignNotPaused, campaignID, c.Status)
	}
	c.Status = StatusRunning
	c.PauseReason = ""
	c.Version++
	c.UpdatedAt = s.now()
	s.emit(OutboxEvent{
		Type:       EventCampaignResumed,
		CampaignID: c.ID,
		Payload:    map[string]string{"version": fmt.Sprintf("%d", c.Version)},
	})
	s.evaluateWave(c)
	return s.persist()
}

// AbortCampaign 中止活动：进入终态、停止发放新指令，
// 并为仍处于 dispatched（已领取未完成）的设备生成补偿通知——
// 补偿仅在状态迁移到 aborted 时生成一次，重复中止返回错误。
func (s *Service) AbortCampaign(campaignID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.st.Campaigns[campaignID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrCampaignNotFound, campaignID)
	}
	if c.Status.Terminal() {
		return fmt.Errorf("%w: campaign %s is %s", ErrCampaignTerminal, campaignID, c.Status)
	}
	c.Status = StatusAborted
	c.PauseReason = ""
	c.UpdatedAt = s.now()
	s.emit(OutboxEvent{Type: EventCampaignAborted, CampaignID: c.ID})
	for _, dev := range c.Devices {
		if dev.State == DeviceDispatched {
			s.emit(OutboxEvent{
				Type:       EventCompensation,
				CampaignID: c.ID,
				DeviceID:   dev.DeviceID,
				Payload:    map[string]string{"reason": "campaign aborted with command in flight"},
			})
		}
	}
	return s.persist()
}

// GetProgress 查询活动进度（各波次统计基于冻结的设备分母）。
func (s *Service) GetProgress(campaignID string) (*Progress, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.st.Campaigns[campaignID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrCampaignNotFound, campaignID)
	}
	p := &Progress{
		CampaignID:  c.ID,
		Status:      c.Status,
		Version:     c.Version,
		CurrentWave: c.CurrentWave,
		PauseReason: c.PauseReason,
		Waves:       make([]WaveProgress, len(c.Waves)),
	}
	for i, wave := range c.Waves {
		wp := WaveProgress{Index: i, Total: len(wave)}
		for _, devID := range wave {
			switch c.Devices[devID].State {
			case DevicePending:
				wp.Pending++
			case DeviceDispatched:
				wp.Dispatched++
			case DeviceSucceeded:
				wp.Succeeded++
			case DeviceFailed:
				wp.Failed++
			}
		}
		if wp.Total > 0 {
			wp.SuccessRate = float64(wp.Succeeded) / float64(wp.Total)
			wp.FailureRate = float64(wp.Failed) / float64(wp.Total)
		}
		p.Waves[i] = wp
		p.TotalDevices += wp.Total
		p.Succeeded += wp.Succeeded
		p.Failed += wp.Failed
	}
	return p, nil
}

// Outbox 返回持久化 outbox 中的全部事件（副本）。
func (s *Service) Outbox() []OutboxEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]OutboxEvent, len(s.st.Outbox))
	copy(out, s.st.Outbox)
	return out
}
