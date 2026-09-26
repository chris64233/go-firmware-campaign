package firmwarecampaign

import (
	"errors"
	"fmt"
	"testing"
)

func newService(t *testing.T) *Service {
	t.Helper()
	s, err := NewService(nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s
}

func mustFirmware(t *testing.T, s *Service, id, model, version string, minBattery int) {
	t.Helper()
	_, err := s.RegisterFirmware(Firmware{
		ID: id, Model: model, Version: version,
		Digest: "sha256:" + id, MinBattery: minBattery,
	})
	if err != nil {
		t.Fatalf("RegisterFirmware: %v", err)
	}
}

func mustDevice(t *testing.T, s *Service, id, model, version string, battery int) {
	t.Helper()
	_, err := s.RegisterDevice(Device{ID: id, Model: model, CurrentVersion: version, Battery: battery})
	if err != nil {
		t.Fatalf("RegisterDevice(%s): %v", id, err)
	}
}

func deviceIDs(prefix string, n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s-%d", prefix, i)
	}
	return ids
}

func mustCampaign(t *testing.T, s *Service, fwID string, waves [][]string, succ, fail float64) *Campaign {
	t.Helper()
	c, err := s.CreateCampaign(CreateCampaignRequest{
		FirmwareID: fwID, Waves: waves,
		SuccessThreshold: succ, FailureThreshold: fail,
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	return c
}

func mustReceipt(t *testing.T, s *Service, r Receipt) ReceiptResult {
	t.Helper()
	res, err := s.ReportReceipt(r)
	if err != nil {
		t.Fatalf("ReportReceipt(%s): %v", r.ID, err)
	}
	return res
}

func TestRegisterFirmwareValidation(t *testing.T) {
	s := newService(t)
	if _, err := s.RegisterFirmware(Firmware{ID: "fw1"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest, got %v", err)
	}
	mustFirmware(t, s, "fw1", "m1", "2.0.0", 20)
	if _, err := s.RegisterFirmware(Firmware{
		ID: "fw1", Model: "m1", Version: "2.0.0", Digest: "sha256:x",
	}); !errors.Is(err, ErrFirmwareExists) {
		t.Fatalf("want ErrFirmwareExists, got %v", err)
	}
}

func TestCreateCampaignFreezesPlan(t *testing.T) {
	s := newService(t)
	mustFirmware(t, s, "fw1", "m1", "2.0.0", 20)
	for _, id := range deviceIDs("d", 4) {
		mustDevice(t, s, id, "m1", "1.0.0", 80)
	}
	waves := [][]string{{"d-0", "d-1"}, {"d-2", "d-3"}}
	c := mustCampaign(t, s, "fw1", waves, 0.5, 0.5)

	if c.FirmwareDigest != "sha256:fw1" {
		t.Fatalf("digest not frozen: %q", c.FirmwareDigest)
	}
	if c.Version != 1 || c.Status != StatusRunning || c.CurrentWave != 0 {
		t.Fatalf("unexpected initial campaign: %+v", c)
	}
	// 调用方修改入参切片不得影响已冻结的波次划分。
	waves[0][0] = "tampered"
	if c.Waves[0][0] != "d-0" {
		t.Fatalf("waves not frozen: %v", c.Waves[0])
	}
	if len(c.Devices) != 4 {
		t.Fatalf("want 4 frozen devices, got %d", len(c.Devices))
	}
}

func TestCreateCampaignEligibility(t *testing.T) {
	s := newService(t)
	mustFirmware(t, s, "fw1", "m1", "2.0.0", 50)
	mustDevice(t, s, "ok", "m1", "1.0.0", 80)
	mustDevice(t, s, "wrong-model", "m2", "1.0.0", 80)
	mustDevice(t, s, "already-target", "m1", "2.0.0", 80)
	mustDevice(t, s, "low-battery", "m1", "1.0.0", 30)

	_, err := s.CreateCampaign(CreateCampaignRequest{
		FirmwareID:       "fw1",
		Waves:            [][]string{{"ok", "wrong-model", "already-target", "low-battery", "ghost"}},
		SuccessThreshold: 0.5, FailureThreshold: 0.5,
	})
	var inel *IneligibilityError
	if !errors.As(err, &inel) {
		t.Fatalf("want IneligibilityError, got %v", err)
	}
	for _, id := range []string{"wrong-model", "already-target", "low-battery", "ghost"} {
		if len(inel.Reasons[id]) == 0 {
			t.Fatalf("expected ineligibility reason for %s, got %v", id, inel.Reasons)
		}
	}
	if _, ok := inel.Reasons["ok"]; ok {
		t.Fatalf("eligible device flagged: %v", inel.Reasons["ok"])
	}
}

func TestCreateCampaignValidation(t *testing.T) {
	s := newService(t)
	mustFirmware(t, s, "fw1", "m1", "2.0.0", 0)
	mustDevice(t, s, "d1", "m1", "1.0.0", 50)

	if _, err := s.CreateCampaign(CreateCampaignRequest{FirmwareID: "nope", Waves: [][]string{{"d1"}}, SuccessThreshold: 0.5, FailureThreshold: 0.5}); !errors.Is(err, ErrFirmwareNotFound) {
		t.Fatalf("want ErrFirmwareNotFound, got %v", err)
	}
	if _, err := s.CreateCampaign(CreateCampaignRequest{FirmwareID: "fw1", Waves: [][]string{{"d1"}}, SuccessThreshold: 0, FailureThreshold: 0.5}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest for success threshold, got %v", err)
	}
	if _, err := s.CreateCampaign(CreateCampaignRequest{FirmwareID: "fw1", Waves: [][]string{{"d1"}}, SuccessThreshold: 0.5, FailureThreshold: 1.5}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest for failure threshold, got %v", err)
	}
	if _, err := s.CreateCampaign(CreateCampaignRequest{FirmwareID: "fw1", SuccessThreshold: 0.5, FailureThreshold: 0.5}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest for empty waves, got %v", err)
	}
	if _, err := s.CreateCampaign(CreateCampaignRequest{FirmwareID: "fw1", Waves: [][]string{{"d1"}, {"d1"}}, SuccessThreshold: 0.5, FailureThreshold: 0.5}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest for duplicate device, got %v", err)
	}
}

func TestMutualExclusionAcrossCampaigns(t *testing.T) {
	s := newService(t)
	mustFirmware(t, s, "fw1", "m1", "2.0.0", 0)
	mustFirmware(t, s, "fw2", "m1", "3.0.0", 0)
	mustDevice(t, s, "d1", "m1", "1.0.0", 90)
	mustDevice(t, s, "d2", "m1", "1.0.0", 90)

	c1 := mustCampaign(t, s, "fw1", [][]string{{"d1"}}, 1, 0.5)

	_, err := s.CreateCampaign(CreateCampaignRequest{
		FirmwareID: "fw2", Waves: [][]string{{"d1", "d2"}},
		SuccessThreshold: 1, FailureThreshold: 0.5,
	})
	var conflict *ConflictError
	if !errors.As(err, &conflict) || !errors.Is(err, ErrDeviceConflict) {
		t.Fatalf("want ConflictError, got %v", err)
	}
	if conflict.Devices["d1"] != c1.ID {
		t.Fatalf("conflict should reference campaign %s: %v", c1.ID, conflict.Devices)
	}

	// 第一个活动中止后，设备可加入新活动。
	if err := s.AbortCampaign(c1.ID); err != nil {
		t.Fatalf("AbortCampaign: %v", err)
	}
	if _, err := s.CreateCampaign(CreateCampaignRequest{
		FirmwareID: "fw2", Waves: [][]string{{"d1", "d2"}},
		SuccessThreshold: 1, FailureThreshold: 0.5,
	}); err != nil {
		t.Fatalf("create after abort: %v", err)
	}
}

func TestClaimCommandGatingAndIdempotency(t *testing.T) {
	s := newService(t)
	mustFirmware(t, s, "fw1", "m1", "2.0.0", 0)
	for _, id := range deviceIDs("d", 4) {
		mustDevice(t, s, id, "m1", "1.0.0", 90)
	}
	c := mustCampaign(t, s, "fw1", [][]string{{"d-0", "d-1"}, {"d-2", "d-3"}}, 1, 0.5)

	// 非当前波次设备不能领取。
	if _, err := s.ClaimCommand(c.ID, "d-2"); !errors.Is(err, ErrNotCurrentWave) {
		t.Fatalf("want ErrNotCurrentWave, got %v", err)
	}
	// 非活动内设备不能领取。
	if _, err := s.ClaimCommand(c.ID, "stranger"); !errors.Is(err, ErrDeviceNotInCampaign) {
		t.Fatalf("want ErrDeviceNotInCampaign, got %v", err)
	}

	cmd1, err := s.ClaimCommand(c.ID, "d-0")
	if err != nil {
		t.Fatalf("ClaimCommand: %v", err)
	}
	if cmd1.CampaignVersion != 1 || cmd1.Digest != "sha256:fw1" || cmd1.IdempotencyKey == "" {
		t.Fatalf("unexpected command: %+v", cmd1)
	}
	// 重复领取：相同幂等键。
	cmd2, err := s.ClaimCommand(c.ID, "d-0")
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if cmd1.IdempotencyKey != cmd2.IdempotencyKey {
		t.Fatalf("idempotency key not stable: %q vs %q", cmd1.IdempotencyKey, cmd2.IdempotencyKey)
	}

	// 暂停后不能领取。
	if err := s.PauseCampaign(c.ID); err != nil {
		t.Fatalf("PauseCampaign: %v", err)
	}
	if _, err := s.ClaimCommand(c.ID, "d-1"); !errors.Is(err, ErrCampaignNotRunning) {
		t.Fatalf("want ErrCampaignNotRunning, got %v", err)
	}
}

func TestReceiptOutOfOrderMonotonic(t *testing.T) {
	s := newService(t)
	mustFirmware(t, s, "fw1", "m1", "2.0.0", 0)
	mustDevice(t, s, "d1", "m1", "1.0.0", 90)
	mustDevice(t, s, "d2", "m1", "1.0.0", 90)
	c := mustCampaign(t, s, "fw1", [][]string{{"d1", "d2"}}, 1, 1) // 阈值拉满，避免自动推进

	if _, err := s.ClaimCommand(c.ID, "d1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimCommand(c.ID, "d2"); err != nil {
		t.Fatal(err)
	}

	// d1: 先成功后失败（乱序）——成功不得被回退。
	if got := mustReceipt(t, s, Receipt{ID: "r1", CampaignID: c.ID, DeviceID: "d1", CampaignVersion: 1, Success: true}); got != ReceiptApplied {
		t.Fatalf("want applied, got %s", got)
	}
	if got := mustReceipt(t, s, Receipt{ID: "r2", CampaignID: c.ID, DeviceID: "d1", CampaignVersion: 1, Success: false}); got != ReceiptSuperseded {
		t.Fatalf("want superseded, got %s", got)
	}
	// d2: 先失败后成功（乱序）——成功可覆盖失败。
	if got := mustReceipt(t, s, Receipt{ID: "r3", CampaignID: c.ID, DeviceID: "d2", CampaignVersion: 1, Success: false}); got != ReceiptApplied {
		t.Fatalf("want applied, got %s", got)
	}
	if got := mustReceipt(t, s, Receipt{ID: "r4", CampaignID: c.ID, DeviceID: "d2", CampaignVersion: 1, Success: true}); got != ReceiptApplied {
		t.Fatalf("want applied, got %s", got)
	}

	p, err := s.GetProgress(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Succeeded != 2 || p.Failed != 0 {
		t.Fatalf("want 2 succeeded 0 failed, got %+v", p)
	}
}

func TestReceiptDuplicateAndKeyMismatch(t *testing.T) {
	s := newService(t)
	mustFirmware(t, s, "fw1", "m1", "2.0.0", 0)
	mustDevice(t, s, "d1", "m1", "1.0.0", 90)
	c := mustCampaign(t, s, "fw1", [][]string{{"d1"}}, 1, 1)

	cmd, err := s.ClaimCommand(c.ID, "d1")
	if err != nil {
		t.Fatal(err)
	}
	r := Receipt{ID: "r1", CampaignID: c.ID, DeviceID: "d1", CampaignVersion: 1,
		IdempotencyKey: cmd.IdempotencyKey, Success: true}
	if got := mustReceipt(t, s, r); got != ReceiptApplied {
		t.Fatalf("want applied, got %s", got)
	}
	// 相同回执 ID 判重，不重复计数。
	if got := mustReceipt(t, s, r); got != ReceiptDuplicate {
		t.Fatalf("want duplicate, got %s", got)
	}
	p, _ := s.GetProgress(c.ID)
	if p.Succeeded != 1 {
		t.Fatalf("duplicate receipt double-counted: %+v", p)
	}
	// 幂等键不匹配被拒绝。
	_, err = s.ReportReceipt(Receipt{ID: "r2", CampaignID: c.ID, DeviceID: "d1",
		CampaignVersion: 1, IdempotencyKey: "bogus", Success: false})
	if !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("want ErrKeyMismatch, got %v", err)
	}
}

func TestStaleReceiptAfterResume(t *testing.T) {
	s := newService(t)
	mustFirmware(t, s, "fw1", "m1", "2.0.0", 0)
	mustDevice(t, s, "d1", "m1", "1.0.0", 90)
	c := mustCampaign(t, s, "fw1", [][]string{{"d1"}}, 1, 1)

	if _, err := s.ClaimCommand(c.ID, "d1"); err != nil {
		t.Fatal(err)
	}
	if err := s.PauseCampaign(c.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeCampaign(c.ID); err != nil {
		t.Fatal(err)
	}
	// 恢复后版本号递增，旧版本回执失效。
	p, _ := s.GetProgress(c.ID)
	if p.Version != 2 {
		t.Fatalf("want version 2 after resume, got %d", p.Version)
	}
	if got := mustReceipt(t, s, Receipt{ID: "old", CampaignID: c.ID, DeviceID: "d1", CampaignVersion: 1, Success: false}); got != ReceiptStale {
		t.Fatalf("want stale, got %s", got)
	}
	p, _ = s.GetProgress(c.ID)
	if p.Failed != 0 {
		t.Fatalf("stale receipt mutated state: %+v", p)
	}
	// 未来版本回执报错。
	_, err := s.ReportReceipt(Receipt{ID: "future", CampaignID: c.ID, DeviceID: "d1", CampaignVersion: 3, Success: true})
	if !errors.Is(err, ErrFutureVersion) {
		t.Fatalf("want ErrFutureVersion, got %v", err)
	}
	// 当前版本回执正常生效。
	if got := mustReceipt(t, s, Receipt{ID: "new", CampaignID: c.ID, DeviceID: "d1", CampaignVersion: 2, Success: true}); got != ReceiptApplied {
		t.Fatalf("want applied, got %s", got)
	}
}

func TestWaveAdvancementFrozenDenominator(t *testing.T) {
	s := newService(t)
	mustFirmware(t, s, "fw1", "m1", "2.0.0", 0)
	for _, id := range deviceIDs("d", 6) {
		mustDevice(t, s, id, "m1", "1.0.0", 90)
	}
	c := mustCampaign(t, s, "fw1",
		[][]string{{"d-0", "d-1", "d-2", "d-3"}, {"d-4", "d-5"}}, 0.75, 0.9)

	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("d-%d", i)
		if _, err := s.ClaimCommand(c.ID, id); err != nil {
			t.Fatal(err)
		}
		mustReceipt(t, s, Receipt{ID: "r-" + id, CampaignID: c.ID, DeviceID: id, CampaignVersion: 1, Success: true})
	}
	p, _ := s.GetProgress(c.ID)
	// 3/4 = 0.75 达到门槛，开放下一波；分母为冻结的 4。
	if p.CurrentWave != 1 {
		t.Fatalf("want wave 1, got %d (%+v)", p.CurrentWave, p.Waves[0])
	}
	if p.Waves[0].Total != 4 || p.Waves[0].SuccessRate != 0.75 {
		t.Fatalf("frozen denominator wrong: %+v", p.Waves[0])
	}
	// 旧波次未完成的设备不能再领取。
	if _, err := s.ClaimCommand(c.ID, "d-3"); !errors.Is(err, ErrNotCurrentWave) {
		t.Fatalf("want ErrNotCurrentWave, got %v", err)
	}
	// 完成最后一波后活动完成。
	for _, id := range []string{"d-4", "d-5"} {
		if _, err := s.ClaimCommand(c.ID, id); err != nil {
			t.Fatal(err)
		}
		mustReceipt(t, s, Receipt{ID: "r-" + id, CampaignID: c.ID, DeviceID: id, CampaignVersion: 1, Success: true})
	}
	p, _ = s.GetProgress(c.ID)
	if p.Status != StatusCompleted {
		t.Fatalf("want completed, got %s", p.Status)
	}
	if _, err := s.ClaimCommand(c.ID, "d-4"); !errors.Is(err, ErrCampaignNotRunning) {
		t.Fatalf("completed campaign must not issue commands, got %v", err)
	}
}

func TestAutoPauseOnFailureThreshold(t *testing.T) {
	s := newService(t)
	mustFirmware(t, s, "fw1", "m1", "2.0.0", 0)
	for _, id := range deviceIDs("d", 4) {
		mustDevice(t, s, id, "m1", "1.0.0", 90)
	}
	c := mustCampaign(t, s, "fw1", [][]string{{"d-0", "d-1", "d-2", "d-3"}}, 1, 0.25)

	if _, err := s.ClaimCommand(c.ID, "d-0"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimCommand(c.ID, "d-1"); err != nil {
		t.Fatal(err)
	}
	mustReceipt(t, s, Receipt{ID: "r0", CampaignID: c.ID, DeviceID: "d-0", CampaignVersion: 1, Success: false})
	p, _ := s.GetProgress(c.ID)
	if p.Status != StatusRunning {
		t.Fatalf("1/4 = 0.25 not over threshold, want running, got %s", p.Status)
	}
	// 2/4 = 0.5 > 0.25，自动暂停。
	mustReceipt(t, s, Receipt{ID: "r1", CampaignID: c.ID, DeviceID: "d-1", CampaignVersion: 1, Success: false})
	p, _ = s.GetProgress(c.ID)
	if p.Status != StatusPaused || p.PauseReason == "" {
		t.Fatalf("want auto-paused with reason, got %s (%q)", p.Status, p.PauseReason)
	}
	if _, err := s.ClaimCommand(c.ID, "d-2"); !errors.Is(err, ErrCampaignNotRunning) {
		t.Fatalf("paused campaign must not issue commands, got %v", err)
	}
	// 失败率仍超阈值时恢复会立即再次自动暂停（状态单调，不会被静默放行）。
	if err := s.ResumeCampaign(c.ID); err != nil {
		t.Fatal(err)
	}
	p, _ = s.GetProgress(c.ID)
	if p.Status != StatusPaused || p.Version != 2 {
		t.Fatalf("want re-paused v2, got %s v%d", p.Status, p.Version)
	}
	// 暂停期间失败设备回报成功（failed -> succeeded 单调推进），
	// 失败率降回阈值内后恢复成功。
	for i, id := range []string{"d-0", "d-1"} {
		mustReceipt(t, s, Receipt{ID: fmt.Sprintf("fix-%d", i), CampaignID: c.ID, DeviceID: id, CampaignVersion: 2, Success: true})
	}
	if err := s.ResumeCampaign(c.ID); err != nil {
		t.Fatal(err)
	}
	p, _ = s.GetProgress(c.ID)
	if p.Status != StatusRunning || p.Version != 3 {
		t.Fatalf("want running v3, got %s v%d", p.Status, p.Version)
	}
}

func TestAbortSemantics(t *testing.T) {
	s := newService(t)
	mustFirmware(t, s, "fw1", "m1", "2.0.0", 0)
	for _, id := range deviceIDs("d", 3) {
		mustDevice(t, s, id, "m1", "1.0.0", 90)
	}
	c := mustCampaign(t, s, "fw1", [][]string{{"d-0", "d-1", "d-2"}}, 1, 1)

	// d-0 成功，d-1 已领取在途，d-2 未领取。
	if _, err := s.ClaimCommand(c.ID, "d-0"); err != nil {
		t.Fatal(err)
	}
	mustReceipt(t, s, Receipt{ID: "r0", CampaignID: c.ID, DeviceID: "d-0", CampaignVersion: 1, Success: true})
	if _, err := s.ClaimCommand(c.ID, "d-1"); err != nil {
		t.Fatal(err)
	}

	if err := s.AbortCampaign(c.ID); err != nil {
		t.Fatal(err)
	}
	// 中止后停止发新指令。
	if _, err := s.ClaimCommand(c.ID, "d-2"); !errors.Is(err, ErrCampaignNotRunning) {
		t.Fatalf("aborted campaign must not issue commands, got %v", err)
	}
	// 补偿通知只对在途设备生成一次。
	countComp := func() int {
		n := 0
		for _, ev := range s.Outbox() {
			if ev.Type == EventCompensation {
				n++
				if ev.DeviceID != "d-1" {
					t.Fatalf("compensation for unexpected device %s", ev.DeviceID)
				}
			}
		}
		return n
	}
	if n := countComp(); n != 1 {
		t.Fatalf("want exactly 1 compensation event, got %d", n)
	}
	// 重复中止报错，且不重复生成补偿。
	if err := s.AbortCampaign(c.ID); !errors.Is(err, ErrCampaignTerminal) {
		t.Fatalf("want ErrCampaignTerminal, got %v", err)
	}
	if n := countComp(); n != 1 {
		t.Fatalf("compensation generated more than once: %d", n)
	}
	// 迟到的失败回执不得回退已成功的设备。
	if got := mustReceipt(t, s, Receipt{ID: "late", CampaignID: c.ID, DeviceID: "d-0", CampaignVersion: 1, Success: false}); got != ReceiptSuperseded {
		t.Fatalf("want superseded, got %s", got)
	}
	p, _ := s.GetProgress(c.ID)
	if p.Status != StatusAborted || p.Succeeded != 1 || p.Failed != 0 {
		t.Fatalf("abort state corrupted: %+v", p)
	}
	// 中止后不能暂停/恢复。
	if err := s.PauseCampaign(c.ID); !errors.Is(err, ErrCampaignTerminal) {
		t.Fatalf("want ErrCampaignTerminal, got %v", err)
	}
	if err := s.ResumeCampaign(c.ID); !errors.Is(err, ErrCampaignNotPaused) {
		t.Fatalf("want ErrCampaignNotPaused, got %v", err)
	}
}

func TestPauseResumeStateMachine(t *testing.T) {
	s := newService(t)
	mustFirmware(t, s, "fw1", "m1", "2.0.0", 0)
	mustDevice(t, s, "d1", "m1", "1.0.0", 90)
	c := mustCampaign(t, s, "fw1", [][]string{{"d1"}}, 1, 1)

	if err := s.ResumeCampaign(c.ID); !errors.Is(err, ErrCampaignNotPaused) {
		t.Fatalf("resume running: want ErrCampaignNotPaused, got %v", err)
	}
	if err := s.PauseCampaign(c.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.PauseCampaign(c.ID); !errors.Is(err, ErrCampaignNotRunning) {
		t.Fatalf("pause paused: want ErrCampaignNotRunning, got %v", err)
	}
	if err := s.ResumeCampaign(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimCommand(c.ID, "d1"); err != nil {
		t.Fatalf("claim after resume: %v", err)
	}
}
