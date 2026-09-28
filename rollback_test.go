package firmwarecampaign

import (
	"fmt"
	"sync"
	"testing"
)

// ---- 测试辅助：登记可定制回退能力的设备 ----

func regDevice(t *testing.T, svc *Service, id, version string, battery int, rollback bool) {
	t.Helper()
	if _, err := svc.RegisterDevice(Device{
		ID: id, Model: "model-x", CurrentVersion: version,
		BatteryLevel:      battery,
		RollbackSupported: rollback,
	}); err != nil {
		t.Fatalf("RegisterDevice(%s): %v", id, err)
	}
}

func claimRaw(t *testing.T, svc *Service, campaignID, deviceID string) ClaimResult {
	t.Helper()
	r, err := svc.ClaimCommand(ClaimCommand{CampaignID: campaignID, DeviceID: deviceID})
	if err != nil {
		t.Fatalf("ClaimCommand(%s): %v", deviceID, err)
	}
	return r
}

func receiptOK(t *testing.T, svc *Service, campaignID, deviceID string, r ClaimResult, result ReceiptResult) {
	t.Helper()
	if err := svc.SubmitReceipt(ReceiptInput{
		CampaignID: campaignID, DeviceID: deviceID,
		CampaignVer: r.CampaignVer, IdempotencyKey: r.IdempotencyKey,
		Result: result, Reason: "boom",
	}); err != nil {
		t.Fatalf("SubmitReceipt(%s,%s): %v", deviceID, result, err)
	}
}

// rollbackCampaign 构建单波 6 台设备的活动：失败上限 1。
//
//	dev-s1 成功、支持回退（from 1.0.0）
//	dev-s2 成功、不支持回退（→ skipped）
//	dev-f1 失败、dev-f2 失败（第 2 个失败越过上限触发）
//	dev-i  已领取、处理中
//	dev-p  从未领取（pending）
func setupRollbackCampaign(t *testing.T, svc *Service) Campaign {
	t.Helper()
	mustRegisterFirmware(t, svc, "fw1", "model-x", "2.0.0")
	regDevice(t, svc, "dev-s1", "1.0.0", 80, true)
	regDevice(t, svc, "dev-s2", "1.0.0", 80, false)
	regDevice(t, svc, "dev-f1", "1.0.0", 80, true)
	regDevice(t, svc, "dev-f2", "1.0.0", 80, true)
	regDevice(t, svc, "dev-i", "1.0.0", 80, true)
	regDevice(t, svc, "dev-p", "1.0.0", 80, true)
	return createCampaign(t, svc, CreateCampaignInput{
		ID: "cp-rb", FirmwareID: "fw1",
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            10,
		SuccessRateThreshold: 1.0,
		MaxFailureRate:       1.0, // 关闭失败率熔断，单独验证计数熔断
		MaxWaveFailures:      1,
	})
}

// 触发动作：s1/s2 成功，f1/f2 失败，i 已领取，p 不领取。第二个失败触发。
func driveToRollback(t *testing.T, svc *Service, c Campaign) {
	t.Helper()
	rs1 := claimRaw(t, svc, c.ID, "dev-s1")
	receiptOK(t, svc, c.ID, "dev-s1", rs1, ReceiptSucceeded)
	rs2 := claimRaw(t, svc, c.ID, "dev-s2")
	receiptOK(t, svc, c.ID, "dev-s2", rs2, ReceiptSucceeded)
	rf1 := claimRaw(t, svc, c.ID, "dev-f1")
	receiptOK(t, svc, c.ID, "dev-f1", rf1, ReceiptFailed)
	// 仅 1 个失败时尚未触发。
	if got, _ := svc.GetCampaign(c.ID); got.Status != CampaignStatusActive {
		t.Fatalf("must stay active at failure limit boundary, got %s", got.Status)
	}
	claimRaw(t, svc, c.ID, "dev-i") // 处理中
	rf2 := claimRaw(t, svc, c.ID, "dev-f2")
	receiptOK(t, svc, c.ID, "dev-f2", rf2, ReceiptFailed) // 2 > 1 触发
}

// ---- 1. 触发后原子停止下发 + 设备分类 + 带版本回退指令 ----

func TestRollback_TriggersAtomicStopAndClassifies(t *testing.T) {
	svc := newTestService()
	c := setupRollbackCampaign(t, svc)
	driveToRollback(t, svc, c)

	got, _ := svc.GetCampaign(c.ID)
	if got.Status != CampaignStatusRollingBack {
		t.Fatalf("status = %s, want rolling_back", got.Status)
	}
	if got.RollbackWave != 1 || got.RollbackReason == "" {
		t.Fatalf("rollback meta = wave %d reason %q", got.RollbackWave, got.RollbackReason)
	}

	// 原子停止下发：处理中/未领取设备都不能再领取。
	for _, id := range []string{"dev-i", "dev-p"} {
		if _, err := svc.ClaimCommand(ClaimCommand{CampaignID: c.ID, DeviceID: id}); !IsError(err, CodeCampaignNotActive) {
			t.Fatalf("device %s claimed after rollback trigger: %v", id, err)
		}
	}

	p, err := svc.GetProgress(c.ID)
	if err != nil {
		t.Fatalf("GetProgress: %v", err)
	}
	if p.Rollback == nil {
		t.Fatalf("expected rollback report")
	}
	r := p.Rollback
	if r.Installed != 2 || r.Failed != 2 || r.InProgress != 2 {
		t.Fatalf("classification = installed %d failed %d inprogress %d, want 2/2/2",
			r.Installed, r.Failed, r.InProgress)
	}
	// 仅 s1（成功且支持回退）进入回退；s2 不支持回退被跳过。
	if len(r.Commands) != 1 || r.Commands[0].DeviceID != "dev-s1" {
		t.Fatalf("commands = %+v, want only dev-s1", r.Commands)
	}
	cmd := r.Commands[0]
	if cmd.FromVersion != "2.0.0" || cmd.ToVersion != "1.0.0" {
		t.Fatalf("rollback versions = %s -> %s, want 2.0.0 -> 1.0.0", cmd.FromVersion, cmd.ToVersion)
	}
	if cmd.CampaignVer != got.Version || cmd.IdempotencyKey != rollbackKey(c.ID, "dev-s1") {
		t.Fatalf("command ver/key = %d %q", cmd.CampaignVer, cmd.IdempotencyKey)
	}
	if len(r.Skipped) != 1 || r.Skipped[0] != "dev-s2" {
		t.Fatalf("skipped = %v, want [dev-s2]", r.Skipped)
	}
	if r.Notified != 1 {
		t.Fatalf("notified = %d, want 1", r.Notified)
	}

	// 波次统计上带同样的分类，且只此一波。
	var rbWaves int
	for _, w := range p.Waves {
		if w.Rollback != nil && w.Rollback.Triggered {
			rbWaves++
			if w.Rollback.Installed != 2 || w.Rollback.InProgress != 2 || w.Rollback.Failed != 2 || w.Rollback.Notified != 1 {
				t.Fatalf("wave rollback stat = %+v", w.Rollback)
			}
		}
	}
	if rbWaves != 1 {
		t.Fatalf("rollback waves = %d, want 1", rbWaves)
	}

	// 恰好一条 rollback.command 事件，负载带版本。
	if n := countOutbox(svc, EventRollbackCommand, "dev-s1"); n != 1 {
		t.Fatalf("rollback.command events for dev-s1 = %d, want 1", n)
	}
	if n := countOutbox(svc, EventWaveRollingBack, ""); n != 1 {
		t.Fatalf("wave_rolling_back events = %d, want 1", n)
	}

	// 终态释放设备锁。
	if _, locked := svc.repo.DeviceLock("dev-s1"); locked {
		t.Fatalf("device still locked after rolling_back")
	}
}

// ---- 1b. 以前完成的批次不受影响 ----

func TestRollback_PriorWavesUnaffected(t *testing.T) {
	svc := newTestService()
	mustRegisterFirmware(t, svc, "fw1", "model-x", "2.0.0")
	// 6 台、每波 3 台：wave1 = d1,d2,d3（全部成功），wave2 = d4(成功),d5,d6(失败)。
	regDevice(t, svc, "d1", "1.0.0", 80, true)
	regDevice(t, svc, "d2", "1.0.0", 80, true)
	regDevice(t, svc, "d3", "1.0.0", 80, true)
	regDevice(t, svc, "d4", "1.0.0", 80, true)
	regDevice(t, svc, "d5", "1.0.0", 80, true)
	regDevice(t, svc, "d6", "1.0.0", 80, true)
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp2", FirmwareID: "fw1",
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            3,
		SuccessRateThreshold: 1.0,
		MaxFailureRate:       1.0,
		MaxWaveFailures:      1,
	})

	// 第一波三台成功，达标放行到第二波。
	succeed(t, svc, c, "d1")
	succeed(t, svc, c, "d2")
	succeed(t, svc, c, "d3")
	if cur, _ := svc.GetCampaign(c.ID); cur.CurrentWave != 2 {
		t.Fatalf("expected wave 2, got %d", cur.CurrentWave)
	}
	// 第二波：d4 成功后 d5、d6 连续失败 -> 触发回退（仅针对第二波）。
	succeed(t, svc, c, "d4")
	failDevice(t, svc, c, "d5")
	failDevice(t, svc, c, "d6")

	got, _ := svc.GetCampaign(c.ID)
	if got.Status != CampaignStatusRollingBack || got.RollbackWave != 2 {
		t.Fatalf("status=%s rbwave=%d, want rolling_back @2", got.Status, got.RollbackWave)
	}
	p, _ := svc.GetProgress(c.ID)
	if p.Waves[0].Rollback != nil {
		t.Fatalf("prior wave must not carry rollback classification")
	}
	if p.Waves[0].Succeeded != 3 {
		t.Fatalf("prior wave results changed: %+v", p.Waves[0])
	}
	// 只有第二波的 d4 收到回退指令，第一波设备绝不回退。
	for _, id := range []string{"d1", "d2", "d3"} {
		if n := countOutbox(svc, EventRollbackCommand, id); n != 0 {
			t.Fatalf("prior-wave device %s got rollback command: %d", id, n)
		}
	}
	if n := countOutbox(svc, EventRollbackCommand, "d4"); n != 1 {
		t.Fatalf("d4 rollback command = %d, want 1", n)
	}
}

// ---- 1c. 迟到回执不能倒退/改变触发瞬间冻结的分类 ----

func TestRollback_LateReceiptsDoNotChangeClassification(t *testing.T) {
	svc := newTestService()
	c := setupRollbackCampaign(t, svc)
	driveToRollback(t, svc, c)

	// 触发时 dev-i 为 issued（in_progress）。迟到成功：状态可前进为成功，
	// 但回退分类仍冻结为 in_progress，不补发回退指令。
	u, _ := svc.repo.GetUpgrade(c.ID, "dev-i")
	if u.Status != UpgradeStatusIssued {
		t.Fatalf("setup: dev-i status = %s", u.Status)
	}
	if err := svc.SubmitReceipt(ReceiptInput{
		CampaignID: c.ID, DeviceID: "dev-i",
		CampaignVer: u.CampaignVer, IdempotencyKey: u.IdempotencyKey,
		Result: ReceiptSucceeded,
	}); err != nil {
		t.Fatalf("late success after rollback: %v", err)
	}
	u, _ = svc.repo.GetUpgrade(c.ID, "dev-i")
	if u.Status != UpgradeStatusSucceeded || u.RollbackState != RollbackStateInProgress {
		t.Fatalf("late success altered rollback class: status=%s class=%s", u.Status, u.RollbackState)
	}
	if n := countOutbox(svc, EventRollbackCommand, "dev-i"); n != 0 {
		t.Fatalf("in-progress device got rollback after late success: %d", n)
	}

	// 已成功并收到回退指令的 dev-s1，迟到失败不得回退成功，也不重复发令。
	us1, _ := svc.repo.GetUpgrade(c.ID, "dev-s1")
	before := countOutbox(svc, EventRollbackCommand, "dev-s1")
	if err := svc.SubmitReceipt(ReceiptInput{
		CampaignID: c.ID, DeviceID: "dev-s1",
		CampaignVer: us1.CampaignVer, IdempotencyKey: us1.IdempotencyKey,
		Result: ReceiptFailed, Reason: "late",
	}); err != nil {
		t.Fatalf("late failure: %v", err)
	}
	us1, _ = svc.repo.GetUpgrade(c.ID, "dev-s1")
	if us1.Status != UpgradeStatusSucceeded {
		t.Fatalf("success regressed to %s", us1.Status)
	}
	if after := countOutbox(svc, EventRollbackCommand, "dev-s1"); after != before {
		t.Fatalf("rollback command duplicated on late failure: %d -> %d", before, after)
	}
}

// ---- 2. 自动回退与人工暂停/中止并发，只保留一种结果 ----

func TestRollback_ConcurrentWithPauseAbort(t *testing.T) {
	svc := newTestService()
	mustRegisterFirmware(t, svc, "fw1", "model-x", "2.0.0")
	const n = 40
	for i := 0; i < n; i++ {
		regDevice(t, svc, fmt.Sprintf("cc%02d", i), "1.0.0", 80, true)
	}
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp-conc-rb", FirmwareID: "fw1",
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            100,
		SuccessRateThreshold: 1.0,
		MaxFailureRate:       1.0,
		MaxWaveFailures:      3,
	})

	keys := map[string]ClaimResult{}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("cc%02d", i)
		keys[id] = claimRaw(t, svc, c.ID, id)
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("cc%02d", i)
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			r := keys[id]
			// 多数失败以冲击熔断；穿插少量成功。
			res := ReceiptFailed
			if i%5 == 0 {
				res = ReceiptSucceeded
			}
			_ = svc.SubmitReceipt(ReceiptInput{
				CampaignID: c.ID, DeviceID: id,
				CampaignVer: r.CampaignVer, IdempotencyKey: r.IdempotencyKey,
				Result: res,
			})
			_ = svc.SubmitReceipt(ReceiptInput{ // 重复回执
				CampaignID: c.ID, DeviceID: id,
				CampaignVer: r.CampaignVer, IdempotencyKey: r.IdempotencyKey,
				Result: res,
			})
		}(i, id)
	}
	wg.Add(2)
	go func() { defer wg.Done(); _ = svc.Pause(c.ID, "manual") }()
	go func() { defer wg.Done(); _ = svc.Abort(c.ID, "manual abort") }()
	wg.Wait()

	got, _ := svc.GetCampaign(c.ID)
	// 终态互斥：rolling_back 与 aborted 至多一个；暂停也可能在熔断前抢先。
	switch got.Status {
	case CampaignStatusRollingBack, CampaignStatusAborted, CampaignStatusPaused:
	default:
		t.Fatalf("unexpected final status: %s", got.Status)
	}
	// 进入回退后，中止/暂停必须被拒绝。
	if got.Status == CampaignStatusRollingBack {
		if err := svc.Abort(c.ID, "late"); !IsError(err, CodeInvalidTransition) {
			t.Fatalf("abort after rollback should fail: %v", err)
		}
		if err := svc.Pause(c.ID, "late"); !IsError(err, CodeInvalidTransition) && !IsError(err, CodeCampaignNotActive) {
			t.Fatalf("pause after rollback should fail: %v", err)
		}
	}
	// 每台设备至多一条回退指令（countOutbox 对 pending/已投递各计一次，不重复）。
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("cc%02d", i)
		if cnt := countOutbox(svc, EventRollbackCommand, id); cnt > 1 {
			t.Fatalf("device %s got %d rollback commands", id, cnt)
		}
	}
}

// ---- 3. 补偿只通知一次：重复扫描与重启恢复不重复发令 ----

func TestRollback_ScanAndRestartNoDuplicate(t *testing.T) {
	repo := NewMemoryRepository()
	svc := NewService(repo)
	c := setupRollbackCampaign(t, svc)
	driveToRollback(t, svc, c)

	// 触发时已发令：重复扫描返回 0，事件数不变。
	for i := 0; i < 3; i++ {
		n, err := svc.ScanRollbackCommands(c.ID)
		if err != nil {
			t.Fatalf("ScanRollbackCommands: %v", err)
		}
		if n != 0 {
			t.Fatalf("repeat scan emitted %d commands, want 0", n)
		}
	}
	if n := countOutbox(svc, EventRollbackCommand, "dev-s1"); n != 1 {
		t.Fatalf("rollback command count = %d, want 1", n)
	}

	// 重启恢复：用同一仓储构造新服务，扫描仍不重复发令。
	restarted := NewService(repo)
	n, err := restarted.ScanRollbackCommands(c.ID)
	if err != nil {
		t.Fatalf("scan after restart: %v", err)
	}
	if n != 0 {
		t.Fatalf("restart scan emitted %d, want 0", n)
	}

	// 模拟崩溃发生在“分类后、通知前”：清掉通知标记后，恢复扫描应恰好补一条；
	// 再扫一次为 0。
	u, _ := repo.GetUpgrade(c.ID, "dev-s1")
	u.RollbackNotified = false
	if err := repo.PutUpgrade(u); err != nil {
		t.Fatalf("PutUpgrade: %v", err)
	}
	n, err = restarted.ScanRollbackCommands(c.ID)
	if err != nil {
		t.Fatalf("recovery scan: %v", err)
	}
	if n != 1 {
		t.Fatalf("recovery scan emitted %d, want 1", n)
	}
	n, _ = restarted.ScanRollbackCommands(c.ID)
	if n != 0 {
		t.Fatalf("scan after recovery emitted %d, want 0", n)
	}
}

// ---- 3b. 暂停期间累积失败越过上限，Resume 时触发回退而非放行 ----

func TestRollback_ResumeTriggersWhenLimitExceededWhilePaused(t *testing.T) {
	svc := newTestService()
	mustRegisterFirmware(t, svc, "fw1", "model-x", "2.0.0")
	for _, id := range []string{"g1", "g2", "g3"} {
		regDevice(t, svc, id, "1.0.0", 80, true)
	}
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp-pause-rb", FirmwareID: "fw1",
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            10,
		SuccessRateThreshold: 1.0,
		MaxFailureRate:       1.0,
		MaxWaveFailures:      1,
	})
	r1 := claimRaw(t, svc, c.ID, "g1")
	r2 := claimRaw(t, svc, c.ID, "g2")
	r3 := claimRaw(t, svc, c.ID, "g3")
	if err := svc.Pause(c.ID, "manual hold"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// 暂停期间 g2、g3 失败：回执仍被接受，但暂停中不做熔断评估。
	receiptOK(t, svc, c.ID, "g2", r2, ReceiptFailed)
	receiptOK(t, svc, c.ID, "g3", r3, ReceiptFailed)
	receiptOK(t, svc, c.ID, "g1", r1, ReceiptSucceeded)

	// 恢复时波次已收敛且失败数 2 > 1：转入回退而不是放行。
	if err := svc.Resume(c.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got, _ := svc.GetCampaign(c.ID)
	if got.Status != CampaignStatusRollingBack {
		t.Fatalf("status = %s, want rolling_back on resume", got.Status)
	}
	// paused → rolling_back 是单一结果：不得同时产生 resumed 事件。
	if n := countEventType(svc, EventCampaignResumed); n != 0 {
		t.Fatalf("resumed event emitted despite rollback: %d", n)
	}
	// g1 成功且支持回退 -> 回退指令；g2/g3 失败不回退。
	if n := countOutbox(svc, EventRollbackCommand, "g1"); n != 1 {
		t.Fatalf("g1 rollback = %d, want 1", n)
	}
}

// ---- 3c. 暂停期间越限但波次未全部收敛，Resume 仍直接回退，未回执设备归 in_progress ----

func TestRollback_ResumeTriggersEvenWhenWaveNotSettled(t *testing.T) {
	svc := newTestService()
	mustRegisterFirmware(t, svc, "fw1", "model-x", "2.0.0")
	for _, id := range []string{"k1", "k2", "k3", "k4"} {
		regDevice(t, svc, id, "1.0.0", 80, true)
	}
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp-pause-open", FirmwareID: "fw1",
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            10,
		SuccessRateThreshold: 1.0,
		MaxFailureRate:       1.0,
		MaxWaveFailures:      1, // 失败 >1 触发
	})
	r1 := claimRaw(t, svc, c.ID, "k1")
	r2 := claimRaw(t, svc, c.ID, "k2")
	r3 := claimRaw(t, svc, c.ID, "k3")
	claimRaw(t, svc, c.ID, "k4") // 已领取但永不回执 -> 波次始终不收敛
	if err := svc.Pause(c.ID, "hold"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// 暂停期间：k1 成功，k2/k3 失败（2 > 1），k4 无回执。波次未收敛。
	receiptOK(t, svc, c.ID, "k1", r1, ReceiptSucceeded)
	receiptOK(t, svc, c.ID, "k2", r2, ReceiptFailed)
	receiptOK(t, svc, c.ID, "k3", r3, ReceiptFailed)

	if err := svc.Resume(c.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got, _ := svc.GetCampaign(c.ID)
	if got.Status != CampaignStatusRollingBack {
		t.Fatalf("status = %s, want rolling_back even though wave unsettled", got.Status)
	}
	// k4 触发时仍 issued -> in_progress；分类不依赖波次收敛。
	p, _ := svc.GetProgress(c.ID)
	if p.Rollback == nil {
		t.Fatalf("missing rollback report")
	}
	if p.Rollback.Installed != 1 || p.Rollback.Failed != 2 || p.Rollback.InProgress != 1 {
		t.Fatalf("report = installed %d failed %d inprogress %d, want 1/2/1",
			p.Rollback.Installed, p.Rollback.Failed, p.Rollback.InProgress)
	}
	if len(p.Rollback.Commands) != 1 || p.Rollback.Commands[0].DeviceID != "k1" {
		t.Fatalf("commands = %+v, want only k1", p.Rollback.Commands)
	}
	// 不产生 resumed 事件。
	if n := countEventType(svc, EventCampaignResumed); n != 0 {
		t.Fatalf("resumed events = %d, want 0", n)
	}
}

// ---- 未启用计数熔断（MaxWaveFailures<=0）保持旧的失败率自动暂停行为 ----

func TestRollback_DisabledKeepsRatePause(t *testing.T) {
	svc := newTestService()
	mustRegisterFirmware(t, svc, "fw1", "model-x", "2.0.0")
	regDevice(t, svc, "h1", "1.0.0", 80, true)
	regDevice(t, svc, "h2", "1.0.0", 80, true)
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp-off", FirmwareID: "fw1",
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            10,
		SuccessRateThreshold: 0.9,
		MaxFailureRate:       0.2,
		MaxWaveFailures:      0,
	})
	succeed(t, svc, c, "h1")
	failDevice(t, svc, c, "h2")
	got, _ := svc.GetCampaign(c.ID)
	if got.Status != CampaignStatusPaused {
		t.Fatalf("status = %s, want paused (rate threshold)", got.Status)
	}
	if n := countEventType(svc, EventRollbackCommand); n != 0 {
		t.Fatalf("rollback commands emitted while disabled: %d", n)
	}
	if n := countEventType(svc, EventWaveRollingBack); n != 0 {
		t.Fatalf("rolling_back event emitted while disabled: %d", n)
	}
}
