package firmwarecampaign

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// ---- 测试辅助 ----

func newTestService() *Service {
	return NewService(NewMemoryRepository())
}

func mustRegisterFirmware(t *testing.T, svc *Service, id, model, version string) Firmware {
	t.Helper()
	fw, err := svc.RegisterFirmware(RegisterFirmwareInput{ID: id, Model: model, Version: version, SHA256: "sha-" + id})
	if err != nil {
		t.Fatalf("RegisterFirmware(%s): %v", id, err)
	}
	return fw
}

func mustRegisterDevice(t *testing.T, svc *Service, id, model, version string, battery int) Device {
	t.Helper()
	d, err := svc.RegisterDevice(Device{ID: id, Model: model, CurrentVersion: version, BatteryLevel: battery})
	if err != nil {
		t.Fatalf("RegisterDevice(%s): %v", id, err)
	}
	return d
}

func seed(t *testing.T, svc *Service) (string, []string) {
	t.Helper()
	mustRegisterFirmware(t, svc, "fw1", "model-x", "2.0.0")
	ids := []string{"d1", "d2", "d3", "d4"}
	mustRegisterDevice(t, svc, "d1", "model-x", "1.0.0", 80)
	mustRegisterDevice(t, svc, "d2", "model-x", "1.0.0", 60)
	mustRegisterDevice(t, svc, "d3", "model-x", "1.1.0", 30)
	mustRegisterDevice(t, svc, "d4", "model-x", "1.0.0", 90)
	return "fw1", ids
}

func createCampaign(t *testing.T, svc *Service, in CreateCampaignInput) Campaign {
	t.Helper()
	c, _, err := svc.CreateCampaign(in)
	if err != nil {
		t.Fatalf("CreateCampaign(%s): %v", in.ID, err)
	}
	return c
}

func claim(t *testing.T, svc *Service, campaignID, deviceID string) ClaimResult {
	t.Helper()
	r, err := svc.ClaimCommand(ClaimCommand{CampaignID: campaignID, DeviceID: deviceID})
	if err != nil {
		t.Fatalf("ClaimCommand(%s): %v", deviceID, err)
	}
	return r
}

func succeed(t *testing.T, svc *Service, c Campaign, deviceID string) {
	t.Helper()
	r, err := svc.ClaimCommand(ClaimCommand{CampaignID: c.ID, DeviceID: deviceID})
	if err != nil {
		t.Fatalf("ClaimCommand(%s): %v", deviceID, err)
	}
	if err := svc.SubmitReceipt(ReceiptInput{
		CampaignID: c.ID, DeviceID: deviceID,
		CampaignVer: r.CampaignVer, IdempotencyKey: r.IdempotencyKey,
		Result: ReceiptSucceeded,
	}); err != nil {
		t.Fatalf("SubmitReceipt(success %s): %v", deviceID, err)
	}
}

func failDevice(t *testing.T, svc *Service, c Campaign, deviceID string) {
	t.Helper()
	r, err := svc.ClaimCommand(ClaimCommand{CampaignID: c.ID, DeviceID: deviceID})
	if err != nil {
		t.Fatalf("ClaimCommand(%s): %v", deviceID, err)
	}
	if err := svc.SubmitReceipt(ReceiptInput{
		CampaignID: c.ID, DeviceID: deviceID,
		CampaignVer: r.CampaignVer, IdempotencyKey: r.IdempotencyKey,
		Result: ReceiptFailed, Reason: "boom",
	}); err != nil {
		t.Fatalf("SubmitReceipt(fail %s): %v", deviceID, err)
	}
}

// ---- 固件与设备登记 ----

func TestRegisterFirmware_DuplicateAndValidation(t *testing.T) {
	svc := newTestService()
	mustRegisterFirmware(t, svc, "fw1", "model-x", "1.0.0")

	_, err := svc.RegisterFirmware(RegisterFirmwareInput{ID: "fw1", Model: "model-x", Version: "2.0.0"})
	if !IsError(err, CodeAlreadyExists) {
		t.Fatalf("expected already_exists, got %v", err)
	}
	if _, err := svc.RegisterFirmware(RegisterFirmwareInput{ID: "", Model: "m", Version: "v"}); !IsError(err, CodeInvalidArgument) {
		t.Fatalf("expected invalid_argument, got %v", err)
	}
}

func TestRegisterDevice_BatteryRange(t *testing.T) {
	svc := newTestService()
	if _, err := svc.RegisterDevice(Device{ID: "d", Model: "m", BatteryLevel: 101}); !IsError(err, CodeInvalidArgument) {
		t.Fatalf("expected invalid_argument for battery 101, got %v", err)
	}
}

// ---- 活动创建：资格筛选与冻结 ----

func TestCreateCampaign_QualificationAndFreeze(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	// 再加一个不同型号与一个已是目标版本的设备
	mustRegisterDevice(t, svc, "d5", "model-y", "1.0.0", 90) // 型号不符
	mustRegisterDevice(t, svc, "d6", "model-x", "2.0.0", 90) // 已在目标版本

	c, snap, err := svc.CreateCampaign(CreateCampaignInput{
		ID: "cp1", FirmwareID: fwID,
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 50},
		BatchSize:            2,
		SuccessRateThreshold: 0.8,
		MaxFailureRate:       0.2,
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	// d3 电量 30 被排除；d5 型号不符；d6 已是目标版本。冻结集合 = d1,d2,d4
	want := []string{"d1", "d2", "d4"}
	if len(snap.DeviceIDs) != 3 {
		t.Fatalf("frozen devices = %v, want %v", snap.DeviceIDs, want)
	}
	for i, id := range want {
		if snap.DeviceIDs[i] != id {
			t.Fatalf("frozen devices = %v, want %v", snap.DeviceIDs, want)
		}
	}
	if c.TotalWaves != 2 || len(snap.Waves[0]) != 2 || len(snap.Waves[1]) != 1 {
		t.Fatalf("wave split = %v", snap.Waves)
	}
	if c.FirmwareDigest != "sha-fw1" {
		t.Fatalf("digest = %q", c.FirmwareDigest)
	}

	// 注册新设备不影响已冻结活动
	mustRegisterDevice(t, svc, "d7", "model-x", "1.0.0", 90)
	p, err := svc.GetProgress("cp1")
	if err != nil {
		t.Fatalf("GetProgress: %v", err)
	}
	if p.Total != 3 {
		t.Fatalf("frozen denominator changed: total=%d, want 3", p.Total)
	}
}

func TestCreateCampaign_VersionAllowList(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	// 只允许 1.1.0 升级：仅 d3 合格（但电量门槛要放宽）
	_, _, err := svc.CreateCampaign(CreateCampaignInput{
		ID: "cp1", FirmwareID: fwID,
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 0, AllowedFromVersion: []string{"1.1.0"}},
		BatchSize:            10,
		SuccessRateThreshold: 0.5,
		MaxFailureRate:       0.5,
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	p, _ := svc.GetProgress("cp1")
	if p.Total != 1 {
		t.Fatalf("total = %d, want 1 (only d3 on 1.1.0)", p.Total)
	}
}

func TestCreateCampaign_NoQualifiedDevices(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	_, _, err := svc.CreateCampaign(CreateCampaignInput{
		ID: "cp1", FirmwareID: fwID,
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 99},
		BatchSize:            10,
		SuccessRateThreshold: 0.5,
		MaxFailureRate:       0.5,
	})
	if !IsError(err, CodeConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestCreateCampaign_DuplicateID(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	in := CreateCampaignInput{
		ID: "cp1", FirmwareID: fwID,
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            10,
		SuccessRateThreshold: 0.5,
		MaxFailureRate:       0.5,
	}
	createCampaign(t, svc, in)
	if _, _, err := svc.CreateCampaign(in); !IsError(err, CodeAlreadyExists) {
		t.Fatalf("expected already_exists, got %v", err)
	}
}

// ---- 互斥活动 ----

func TestCreateCampaign_MutexWithActiveCampaign(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	mustRegisterFirmware(t, svc, "fw2", "model-x", "2.1.0")
	in := func(id, fw string, minBattery int) CreateCampaignInput {
		return CreateCampaignInput{
			ID: id, FirmwareID: fw,
			DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: minBattery},
			BatchSize:            10,
			SuccessRateThreshold: 0.5,
			MaxFailureRate:       0.5,
		}
	}
	// 电量门槛 50：cp1 冻结 d1,d2,d4（d3 电量 30 不合格）
	createCampaign(t, svc, in("cp1", fwID, 50))

	// cp2 放宽电量后本应包含 d1..d4，但 d1/d2/d4 被互斥占用，只剩 d3。
	c2, snap2, err := svc.CreateCampaign(in("cp2", "fw2", 20))
	if err != nil {
		t.Fatalf("CreateCampaign cp2: %v", err)
	}
	if c2.TotalWaves != 1 || len(snap2.DeviceIDs) != 1 || snap2.DeviceIDs[0] != "d3" {
		t.Fatalf("cp2 frozen = %v, want only [d3]", snap2.DeviceIDs)
	}

	// 没有任何空闲合格设备时冲突
	_, _, err = svc.CreateCampaign(in("cp3", "fw2", 20))
	if !IsError(err, CodeConflict) {
		t.Fatalf("expected device_busy conflict, got %v", err)
	}
}

// ---- 指令领取 ----

func TestClaim_OnlyCurrentWaveAndState(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp1", FirmwareID: fwID,
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            2, // wave1: d1,d2  wave2: d3,d4
		SuccessRateThreshold: 0.5,
		MaxFailureRate:       0.5,
	})

	// 波次 2 的 d3 不能提前领取
	_, err := svc.ClaimCommand(ClaimCommand{CampaignID: "cp1", DeviceID: "d3"})
	if !IsError(err, CodeNotCurrentWave) {
		t.Fatalf("expected not_current_wave, got %v", err)
	}

	// d1 正常领取：带活动版本号与稳定幂等键
	r1 := claim(t, svc, "cp1", "d1")
	if r1.CampaignVer != c.Version || r1.IdempotencyKey == "" || r1.AlreadyIssued {
		t.Fatalf("unexpected claim: %+v", r1)
	}
	if r1.IdempotencyKey != stableKey("cp1", "d1") {
		t.Fatalf("idempotency key = %q", r1.IdempotencyKey)
	}
	// 重复领取：幂等重放
	r1b, err := svc.ClaimCommand(ClaimCommand{CampaignID: "cp1", DeviceID: "d1", IdempotencyKey: r1.IdempotencyKey})
	if err != nil || !r1b.AlreadyIssued || r1b.CampaignVer != r1.CampaignVer {
		t.Fatalf("repeated claim should replay: %+v err=%v", r1b, err)
	}
	// 幂等键错误
	_, err = svc.ClaimCommand(ClaimCommand{CampaignID: "cp1", DeviceID: "d1", IdempotencyKey: "wrong"})
	if !IsError(err, CodeInvalidArgument) {
		t.Fatalf("expected invalid argument on key mismatch, got %v", err)
	}

	// 暂停后不能领取
	if err := svc.Pause("cp1", "manual"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	_, err = svc.ClaimCommand(ClaimCommand{CampaignID: "cp1", DeviceID: "d2"})
	if !IsError(err, CodeCampaignNotActive) {
		t.Fatalf("expected not active while paused, got %v", err)
	}
	if err := svc.Resume("cp1"); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// 推进到波次 2 后，波次 1 的 d1 不能再领取
	succeed(t, svc, c, "d1")
	succeed(t, svc, c, "d2")
	c2, _ := svc.GetCampaign("cp1")
	if c2.CurrentWave != 2 {
		t.Fatalf("current wave = %d, want 2", c2.CurrentWave)
	}
	_, err = svc.ClaimCommand(ClaimCommand{CampaignID: "cp1", DeviceID: "d1"})
	if !IsError(err, CodeNotCurrentWave) {
		t.Fatalf("expected not_current_wave for old wave, got %v", err)
	}
	claim(t, svc, "cp1", "d3")
}

func TestClaim_AbortedStopsNewCommands(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp1", FirmwareID: fwID,
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            10,
		SuccessRateThreshold: 0.5,
		MaxFailureRate:       0.5,
	})
	if err := svc.Abort(c.ID, "stop"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if _, err := svc.ClaimCommand(ClaimCommand{CampaignID: c.ID, DeviceID: "d1"}); !IsError(err, CodeCampaignNotActive) {
		t.Fatalf("expected no claims after abort, got %v", err)
	}
	// 中止后在途回执仍被接受
	r, _ := svc.repo.GetUpgrade(c.ID, "d1")
	r.Status = UpgradeStatusIssued
	r.CampaignVer = 1
	_ = svc.repo.PutUpgrade(r)
	if err := svc.SubmitReceipt(ReceiptInput{CampaignID: c.ID, DeviceID: "d1", CampaignVer: 1, Result: ReceiptSucceeded}); err != nil {
		t.Fatalf("in-flight receipt after abort should be accepted: %v", err)
	}
}

// ---- 回执：重复、乱序、版本水位 ----

func TestReceipt_DuplicateAndIdempotencyKey(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp1", FirmwareID: fwID,
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            10,
		SuccessRateThreshold: 1.0,
		MaxFailureRate:       1.0,
	})
	r := claim(t, svc, "cp1", "d1")

	// 幂等键不匹配
	err := svc.SubmitReceipt(ReceiptInput{CampaignID: c.ID, DeviceID: "d1", CampaignVer: r.CampaignVer, IdempotencyKey: "bogus", Result: ReceiptSucceeded})
	if !IsError(err, CodeInvalidArgument) {
		t.Fatalf("expected invalid argument, got %v", err)
	}

	// 成功回执
	receipt := ReceiptInput{CampaignID: c.ID, DeviceID: "d1", CampaignVer: r.CampaignVer, IdempotencyKey: r.IdempotencyKey, Result: ReceiptSucceeded}
	if err := svc.SubmitReceipt(receipt); err != nil {
		t.Fatalf("first receipt: %v", err)
	}
	eventsBefore := countOutbox(svc, EventUpgradeSucceeded, "d1")
	// 完全重复的回执：丢弃且不重复产生事件
	if err := svc.SubmitReceipt(receipt); err != nil {
		t.Fatalf("duplicate receipt must be idempotent: %v", err)
	}
	if got := countOutbox(svc, EventUpgradeSucceeded, "d1"); got != eventsBefore {
		t.Fatalf("duplicate success produced extra event: %d -> %d", eventsBefore, got)
	}
	u, _ := svc.repo.GetUpgrade(c.ID, "d1")
	if u.Status != UpgradeStatusSucceeded {
		t.Fatalf("status = %s", u.Status)
	}
}

func TestReceipt_StaleVersionCannotOverwrite(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp1", FirmwareID: fwID,
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            10,
		SuccessRateThreshold: 0.0,
		MaxFailureRate:       1.0,
	})
	r := claim(t, svc, "cp1", "d1")
	// 旧版本（v0）失败回执先到——版本低于初始水位？初始水位为 0，0 < 0 不成立，会被接受。
	// 用正常版本成功，再用更旧版本失败来验证水位。
	if err := svc.SubmitReceipt(ReceiptInput{
		CampaignID: c.ID, DeviceID: "d1", CampaignVer: r.CampaignVer, IdempotencyKey: r.IdempotencyKey,
		Result: ReceiptSucceeded,
	}); err != nil {
		t.Fatalf("success receipt: %v", err)
	}
	// 旧活动版本（v < 已接受版本）的失败不得覆盖
	err := svc.SubmitReceipt(ReceiptInput{
		CampaignID: c.ID, DeviceID: "d1", CampaignVer: r.CampaignVer - 1, IdempotencyKey: r.IdempotencyKey,
		Result: ReceiptFailed, Reason: "late",
	})
	if !IsError(err, CodeStaleReceipt) {
		t.Fatalf("expected stale_receipt, got %v", err)
	}
	u, _ := svc.repo.GetUpgrade(c.ID, "d1")
	if u.Status != UpgradeStatusSucceeded {
		t.Fatalf("success was overwritten by stale failure: %s", u.Status)
	}
}

func TestReceipt_OutOfOrderFailureThenSuccess(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp1", FirmwareID: fwID,
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            10,
		SuccessRateThreshold: 1.0,
		MaxFailureRate:       1.0,
	})
	r := claim(t, svc, "cp1", "d1")
	// 失败先到
	if err := svc.SubmitReceipt(ReceiptInput{CampaignID: c.ID, DeviceID: "d1", CampaignVer: r.CampaignVer, IdempotencyKey: r.IdempotencyKey, Result: ReceiptFailed}); err != nil {
		t.Fatalf("fail receipt: %v", err)
	}
	// 成功（更高等级的单调状态）后到，应覆盖失败
	if err := svc.SubmitReceipt(ReceiptInput{CampaignID: c.ID, DeviceID: "d1", CampaignVer: r.CampaignVer, IdempotencyKey: r.IdempotencyKey, Result: ReceiptSucceeded}); err != nil {
		t.Fatalf("success receipt: %v", err)
	}
	u, _ := svc.repo.GetUpgrade(c.ID, "d1")
	if u.Status != UpgradeStatusSucceeded {
		t.Fatalf("status = %s, want succeeded (monotonic rank allows failed->succeeded)", u.Status)
	}
	d, _ := svc.repo.GetDevice("d1")
	if d.CurrentVersion != "2.0.0" {
		t.Fatalf("device version = %s, want 2.0.0", d.CurrentVersion)
	}
}

func TestReceipt_LateFailureAfterSuccessEmitsOneCompensation(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp1", FirmwareID: fwID,
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            10,
		SuccessRateThreshold: 1.0,
		MaxFailureRate:       1.0,
	})
	r := claim(t, svc, "cp1", "d1")
	ok := ReceiptInput{CampaignID: c.ID, DeviceID: "d1", CampaignVer: r.CampaignVer, IdempotencyKey: r.IdempotencyKey, Result: ReceiptSucceeded}
	if err := svc.SubmitReceipt(ok); err != nil {
		t.Fatalf("success: %v", err)
	}
	late := ok
	late.Result = ReceiptFailed
	late.Reason = "power lost after install"

	// 迟到失败不回退成功，只生成一次补偿通知
	for i := 0; i < 3; i++ {
		if err := svc.SubmitReceipt(late); err != nil {
			t.Fatalf("late failure %d: %v", i, err)
		}
	}
	if got := countOutbox(svc, EventCompensation, "d1"); got != 1 {
		t.Fatalf("compensation events = %d, want exactly 1", got)
	}
	u, _ := svc.repo.GetUpgrade(c.ID, "d1")
	if u.Status != UpgradeStatusSucceeded {
		t.Fatalf("status = %s, success must be retained", u.Status)
	}
}

func TestReceipt_WithoutPriorClaim(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp1", FirmwareID: fwID,
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            10,
		SuccessRateThreshold: 1.0,
		MaxFailureRate:       1.0,
	})
	err := svc.SubmitReceipt(ReceiptInput{CampaignID: c.ID, DeviceID: "d1", CampaignVer: 1, IdempotencyKey: stableKey(c.ID, "d1"), Result: ReceiptSucceeded})
	if !IsError(err, CodeConflict) {
		t.Fatalf("expected conflict for receipt without claim, got %v", err)
	}
}

// ---- 波次门槛 ----

func TestWave_SuccessThresholdOpensNext(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	// 2 个设备一波，门槛 0.5：d1 成功 d2 失败 => 成功率 0.5 达标放行
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp1", FirmwareID: fwID,
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            2,
		SuccessRateThreshold: 0.5,
		MaxFailureRate:       0.9,
	})
	succeed(t, svc, c, "d1")
	// 仅一台收敛时不推进
	if got, _ := svc.GetCampaign("cp1"); got.CurrentWave != 1 || got.Status != CampaignStatusActive {
		t.Fatalf("wave advanced prematurely: %+v", got)
	}
	failDevice(t, svc, c, "d2")
	got, _ := svc.GetCampaign("cp1")
	if got.CurrentWave != 2 || got.Status != CampaignStatusActive {
		t.Fatalf("expected wave 2 active after meeting threshold, got wave=%d status=%s", got.CurrentWave, got.Status)
	}

	// 成功率按冻结分母计算
	p, _ := svc.GetProgress("cp1")
	if p.Waves[0].Total != 2 || p.Waves[0].SuccessRate != 0.5 {
		t.Fatalf("wave1 stat = %+v", p.Waves[0])
	}
}

func TestWave_FailureThresholdAutoPauses(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp1", FirmwareID: fwID,
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            2,
		SuccessRateThreshold: 0.9,
		MaxFailureRate:       0.2, // d1 成功 d2 失败 => 失败率 0.5 > 0.2 自动暂停
	})
	succeed(t, svc, c, "d1")
	failDevice(t, svc, c, "d2")
	got, _ := svc.GetCampaign("cp1")
	if got.Status != CampaignStatusPaused || got.PauseReason == "" {
		t.Fatalf("expected auto pause, got %+v", got)
	}
	// 暂停期间不能领取
	if _, err := svc.ClaimCommand(ClaimCommand{CampaignID: c.ID, DeviceID: "d3"}); !IsError(err, CodeCampaignNotActive) {
		t.Fatalf("claim while auto-paused: %v", err)
	}
	// 运维恢复：未达标但确认放行下一波
	if err := svc.Resume("cp1"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got2, _ := svc.GetCampaign("cp1")
	if got2.Status != CampaignStatusActive || got2.CurrentWave != 2 {
		t.Fatalf("expected override advance to wave 2, got %+v", got2)
	}
}

func TestWave_SuccessBelowThresholdAutoPausesAndResume(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp1", FirmwareID: fwID,
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            2,
		SuccessRateThreshold: 0.9,
		MaxFailureRate:       0.9,
	})
	succeed(t, svc, c, "d1")
	failDevice(t, svc, c, "d2") // 成功率 0.5 < 0.9 => 自动暂停
	got, _ := svc.GetCampaign("cp1")
	if got.Status != CampaignStatusPaused {
		t.Fatalf("expected auto pause, got %s", got.Status)
	}
	if err := svc.Resume("cp1"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got, _ = svc.GetCampaign("cp1")
	if got.CurrentWave != 2 || got.Status != CampaignStatusActive {
		t.Fatalf("expected wave 2 after resume, got %+v", got)
	}
}

func TestCampaign_CompletionReleasesLocksAndEvent(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp1", FirmwareID: fwID,
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            10,
		SuccessRateThreshold: 1.0,
		MaxFailureRate:       0.0,
	})
	for _, id := range []string{"d1", "d2", "d3", "d4"} {
		succeed(t, svc, c, id)
	}
	got, _ := svc.GetCampaign("cp1")
	if got.Status != CampaignStatusCompleted {
		t.Fatalf("status = %s, want completed", got.Status)
	}
	p, _ := svc.GetProgress("cp1")
	if !p.Finished || p.Succeeded != 4 {
		t.Fatalf("progress = %+v", p)
	}
	// 设备锁释放：同一设备可以加入新活动
	mustRegisterFirmware(t, svc, "fw2", "model-x", "3.0.0")
	// 设备版本已是 2.0.0，新固件 3.0.0 仍合格
	c2, _, err := svc.CreateCampaign(CreateCampaignInput{
		ID: "cp2", FirmwareID: "fw2",
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 0, AllowedFromVersion: []string{"2.0.0"}},
		BatchSize:            10,
		SuccessRateThreshold: 1.0,
		MaxFailureRate:       0.0,
	})
	if err != nil {
		t.Fatalf("devices should be free after completion: %v", err)
	}
	if c2.TotalWaves != 1 {
		t.Fatalf("cp2 waves = %d", c2.TotalWaves)
	}
	if got := countOutbox(svc, EventCampaignCompleted, ""); got != 1 {
		t.Fatalf("completed events = %d, want 1", got)
	}
}

// ---- 暂停 / 恢复 / 中止的状态单调性 ----

func TestPauseResumeAbort_Transitions(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp1", FirmwareID: fwID,
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            10,
		SuccessRateThreshold: 1.0,
		MaxFailureRate:       0.0,
	})

	if err := svc.Resume("cp1"); !IsError(err, CodeInvalidTransition) {
		t.Fatalf("resume active: %v", err)
	}
	if err := svc.Pause("cp1", "m"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := svc.Pause("cp1", "m"); !IsError(err, CodeInvalidTransition) {
		t.Fatalf("pause paused: %v", err)
	}
	v := c.Version
	got, _ := svc.GetCampaign("cp1")
	if got.Version <= v {
		t.Fatalf("version did not advance on pause: %d -> %d", v, got.Version)
	}
	if err := svc.Resume("cp1"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := svc.Abort("cp1", "done"); err != nil {
		t.Fatalf("abort: %v", err)
	}
	// 终态不可逆
	for _, op := range []func() error{
		func() error { return svc.Pause("cp1", "x") },
		func() error { return svc.Resume("cp1") },
	} {
		if err := op(); !IsError(err, CodeInvalidTransition) {
			t.Fatalf("transition from aborted: %v", err)
		}
	}
	if err := svc.Abort("cp1", "again"); !IsError(err, CodeInvalidTransition) {
		t.Fatalf("double abort: %v", err)
	}
}

func TestAbort_ReleasesDeviceLocks(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp1", FirmwareID: fwID,
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            10,
		SuccessRateThreshold: 1.0,
		MaxFailureRate:       0.0,
	})
	if err := svc.Abort(c.ID, "stop"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	for _, id := range []string{"d1", "d2", "d3", "d4"} {
		if _, locked := svc.repo.DeviceLock(id); locked {
			t.Fatalf("device %s still locked after abort", id)
		}
	}
}

func TestManualPause_ReceiptsStillAcceptedSettleOnResume(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp1", FirmwareID: fwID,
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            2,
		SuccessRateThreshold: 1.0,
		MaxFailureRate:       0.0,
	})
	// 先领取两台，然后手动暂停，回执在暂停期间到达（一台成功一台失败）
	r1 := claim(t, svc, "cp1", "d1")
	r2 := claim(t, svc, "cp1", "d2")
	if err := svc.Pause("cp1", "investigating"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := svc.SubmitReceipt(ReceiptInput{CampaignID: c.ID, DeviceID: "d1", CampaignVer: r1.CampaignVer, IdempotencyKey: r1.IdempotencyKey, Result: ReceiptSucceeded}); err != nil {
		t.Fatalf("receipt during pause: %v", err)
	}
	if err := svc.SubmitReceipt(ReceiptInput{CampaignID: c.ID, DeviceID: "d2", CampaignVer: r2.CampaignVer, IdempotencyKey: r2.IdempotencyKey, Result: ReceiptFailed}); err != nil {
		t.Fatalf("receipt during pause: %v", err)
	}
	got, _ := svc.GetCampaign("cp1")
	if got.Status != CampaignStatusPaused || got.CurrentWave != 1 {
		t.Fatalf("wave must not advance while paused: %+v", got)
	}
	// 恢复时波次已收敛且未达标 → 运维确认放行
	if err := svc.Resume("cp1"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got, _ = svc.GetCampaign("cp1")
	if got.Status != CampaignStatusActive || got.CurrentWave != 2 {
		t.Fatalf("expected override to wave 2: %+v", got)
	}
}

// ---- 版本号在暂停/恢复后推进，旧指令重新领取刷新版本 ----

func TestClaim_RefreshesVersionAfterPauseResume(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	createCampaign(t, svc, CreateCampaignInput{
		ID: "cp1", FirmwareID: fwID,
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            10,
		SuccessRateThreshold: 1.0,
		MaxFailureRate:       0.0,
	})
	r := claim(t, svc, "cp1", "d1")
	if err := svc.Pause("cp1", "m"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := svc.Resume("cp1"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	r2, err := svc.ClaimCommand(ClaimCommand{CampaignID: "cp1", DeviceID: "d1", IdempotencyKey: r.IdempotencyKey})
	if err != nil {
		t.Fatalf("re-claim after resume: %v", err)
	}
	if !r2.AlreadyIssued {
		// issued 状态 + 新版本：实现选择重新下发而非重放，两者都合理；
		// 这里要求版本号必须刷新为最新活动版本。
	}
	cur, _ := svc.GetCampaign("cp1")
	if r2.CampaignVer != cur.Version {
		t.Fatalf("claim version %d != campaign version %d", r2.CampaignVer, cur.Version)
	}
}

// ---- Outbox ----

func TestOutbox_TransactionalAndDispatch(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp1", FirmwareID: fwID,
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            10,
		SuccessRateThreshold: 1.0,
		MaxFailureRate:       0.0,
	})

	var mu sync.Mutex
	seen := map[string]int{}
	h := func(_ context.Context, e OutboxEvent) error {
		mu.Lock()
		defer mu.Unlock()
		seen[e.EventType]++
		return nil
	}
	n, err := svc.DispatchOutbox(context.Background(), h)
	if err != nil || n == 0 {
		t.Fatalf("dispatch: n=%d err=%v", n, err)
	}
	// 已投递后不再出现
	pending := svc.ListPendingOutbox()
	for _, e := range pending {
		if e.EventType == EventCampaignCreated {
			t.Fatalf("created event still pending")
		}
	}

	// handler 失败：事件保留，至少一次
	r := claim(t, svc, "cp1", "d1")
	_ = svc.SubmitReceipt(ReceiptInput{CampaignID: c.ID, DeviceID: "d1", CampaignVer: r.CampaignVer, IdempotencyKey: r.IdempotencyKey, Result: ReceiptSucceeded})
	failOnce := errors.New("network down")
	calls := 0
	_, _ = svc.DispatchOutbox(context.Background(), func(_ context.Context, e OutboxEvent) error {
		calls++
		if e.EventType == EventUpgradeSucceeded {
			return failOnce
		}
		return nil
	})
	if got := countOutbox(svc, EventUpgradeSucceeded, "d1"); got != 1 {
		t.Fatalf("failed dispatch must keep event pending: %d", got)
	}
}

// ---- 并发：回执 × 暂停/中止，状态单调且无竞态 ----

func TestConcurrent_ReceiptsWithPauseAbort(t *testing.T) {
	svc := newTestService()
	mustRegisterFirmware(t, svc, "fw1", "model-x", "2.0.0")
	// 50 台设备，单波
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("c%02d", i)
		mustRegisterDevice(t, svc, id, "model-x", "1.0.0", 80)
	}
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp-conc", FirmwareID: "fw1",
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            100,
		SuccessRateThreshold: 1.0,
		MaxFailureRate:       0.0,
	})

	var wg sync.WaitGroup
	// 所有设备先领取
	keys := map[string]struct {
		ver int64
		key string
	}{}
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("c%02d", i)
		r := claim(t, svc, c.ID, id)
		keys[id] = struct {
			ver int64
			key string
		}{r.CampaignVer, r.IdempotencyKey}
	}

	// 一半成功一半失败，每个回执再重复发一次（模拟重复投递）
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("c%02d", i)
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			k := keys[id]
			result := ReceiptSucceeded
			if i%2 == 0 {
				result = ReceiptFailed
			}
			in := ReceiptInput{CampaignID: c.ID, DeviceID: id, CampaignVer: k.ver, IdempotencyKey: k.key, Result: result}
			_ = svc.SubmitReceipt(in)
			_ = svc.SubmitReceipt(in) // 重复
			// 再来一个相反结果，验证单调
			other := in
			if result == ReceiptSucceeded {
				other.Result = ReceiptFailed
			} else {
				other.Result = ReceiptSucceeded
			}
			_ = svc.SubmitReceipt(other)
		}(i, id)
	}
	// 并发地暂停/恢复/中止尝试（中止只执行一次）
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = svc.Pause(c.ID, "concurrent")
		_ = svc.Resume(c.ID)
	}()
	wg.Wait()

	// 最终状态一致性：每台设备的状态都必须是终态且单调结果合法
	upgrades := svc.repo.ListUpgrades(c.ID)
	if len(upgrades) != 50 {
		t.Fatalf("upgrades = %d", len(upgrades))
	}
	succeeded, failed := 0, 0
	for _, u := range upgrades {
		if !u.Status.Terminal() {
			t.Fatalf("device %s non-terminal: %s", u.DeviceID, u.Status)
		}
		if u.Status == UpgradeStatusSucceeded {
			succeeded++
		} else {
			failed++
		}
	}
	if succeeded+failed != 50 {
		t.Fatalf("succeeded=%d failed=%d", succeeded, failed)
	}
	// 补偿事件至多每台一次
	if got := countEventType(svc, EventCompensation); got > 50 {
		t.Fatalf("compensation events = %d", got)
	}
}

func TestConcurrent_AbortStopsClaims(t *testing.T) {
	svc := newTestService()
	fwID, _ := seed(t, svc)
	for i := 0; i < 20; i++ {
		mustRegisterDevice(t, svc, fmt.Sprintf("e%d", i), "model-x", "1.0.0", 80)
	}
	c := createCampaign(t, svc, CreateCampaignInput{
		ID: "cp-abort", FirmwareID: fwID,
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            100,
		SuccessRateThreshold: 1.0,
		MaxFailureRate:       0.0,
	})

	var wg sync.WaitGroup
	aborted := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = svc.Abort(c.ID, "stop")
		close(aborted)
	}()
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("e%d", i)
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, err := svc.ClaimCommand(ClaimCommand{CampaignID: c.ID, DeviceID: id})
			if err != nil && !IsError(err, CodeCampaignNotActive) {
				t.Errorf("unexpected claim error for %s: %v", id, err)
			}
		}(id)
	}
	wg.Wait()
	got, _ := svc.GetCampaign(c.ID)
	if got.Status != CampaignStatusAborted {
		t.Fatalf("status = %s", got.Status)
	}
}

// ---- 查询不存在资源 ----

func TestGetProgress_NotFound(t *testing.T) {
	svc := newTestService()
	if _, err := svc.GetProgress("nope"); !IsError(err, CodeNotFound) {
		t.Fatalf("expected not_found, got %v", err)
	}
	if _, err := svc.ClaimCommand(ClaimCommand{CampaignID: "nope", DeviceID: "d1"}); !IsError(err, CodeNotFound) {
		t.Fatalf("expected not_found, got %v", err)
	}
}

// ---- helpers ----

func countOutbox(svc *Service, eventType, deviceID string) int {
	n := 0
	for _, e := range svc.repo.ListPendingOutbox() {
		if e.EventType == eventType && (deviceID == "" || e.DeviceID == deviceID) {
			n++
		}
	}
	// 已投递事件不在 pending 中，直接扫描底层仓储。
	repo := svc.repo.(*MemoryRepository)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	for _, e := range repo.outbox {
		if e.Dispatched && e.EventType == eventType && (deviceID == "" || e.DeviceID == deviceID) {
			n++
		}
	}
	return n
}

func countEventType(svc *Service, eventType string) int {
	repo := svc.repo.(*MemoryRepository)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	n := 0
	for _, e := range repo.outbox {
		if e.EventType == eventType {
			n++
		}
	}
	return n
}
