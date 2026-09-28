package firmwarecampaign

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

// ---- 回退测试辅助 ----

// registerRB 注册一台显式允许/禁止回退的设备。
func registerRB(t *testing.T, svc *Service, id, version string, battery int, rollbackAllowed bool) Device {
	t.Helper()
	d, err := svc.RegisterDevice(Device{
		ID: id, Model: "model-x", CurrentVersion: version,
		BatteryLevel:    battery,
		RollbackAllowed: rollbackAllowed,
	})
	if err != nil {
		t.Fatalf("RegisterDevice(%s): %v", id, err)
	}
	return d
}

// seedRollback 准备 6 台设备（1.0.0 -> 2.0.0）：
// d1..d4 允许回退，d5、d6 不允许回退。
func seedRollback(t *testing.T, svc *Service) string {
	t.Helper()
	mustRegisterFirmware(t, svc, "fw1", "model-x", "2.0.0")
	for _, id := range []string{"d1", "d2", "d3", "d4"} {
		registerRB(t, svc, id, "1.0.0", 80, true)
	}
	for _, id := range []string{"d5", "d6"} {
		registerRB(t, svc, id, "1.0.0", 80, false)
	}
	return "fw1"
}

func rollbackCampaignInput(id string, batchSize, maxFailures int) CreateCampaignInput {
	return CreateCampaignInput{
		ID: id, FirmwareID: "fw1",
		DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
		BatchSize:            batchSize,
		SuccessRateThreshold: 0.0, // 本测试关注失败数上限，比率门槛放到最宽
		MaxFailureRate:       1.0,
		MaxFailuresPerWave:   maxFailures,
	}
}

func rollbackEvents(svc *Service, deviceID string) []OutboxEvent {
	var out []OutboxEvent
	repo := svc.repo.(*MemoryRepository)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	for _, e := range repo.outbox {
		if e.EventType == EventRollbackCommand && (deviceID == "" || e.DeviceID == deviceID) {
			out = append(out, e)
		}
	}
	return out
}

// ---- 1. 越限原子触发：停止下发 + 本批回退指令 ----

func TestRollback_TriggersWhenFailuresExceedLimit(t *testing.T) {
	svc := newTestService()
	fwID := seedRollback(t, svc)
	// 单波 6 台，失败上限 1：第 2 个失败即触发。
	c := createCampaign(t, svc, rollbackCampaignInput("cp1", 10, 1))
	_ = fwID

	// d1 成功且允许回退 → 应回退；d5 成功但不允许回退 → 不回退。
	succeed(t, svc, c, "d1")
	succeed(t, svc, c, "d5")
	failDevice(t, svc, c, "d2") // 第 1 个失败：未越限（==1），不触发
	got, _ := svc.GetCampaign("cp1")
	if got.Status != CampaignStatusActive {
		t.Fatalf("status = %s, want active at boundary failed==limit", got.Status)
	}
	// d3 在处理中（已领取未回执）；d4、d6 尚未下发。
	claim(t, svc, "cp1", "d3")

	failDevice(t, svc, c, "d6") // 第 2 个失败：越过上限 → 原子回退
	got, _ = svc.GetCampaign("cp1")
	if got.Status != CampaignStatusRolledBack {
		t.Fatalf("status = %s, want rolled_back", got.Status)
	}
	if got.CurrentWave != 1 || got.RolledBackAt.IsZero() || got.RollbackReason == "" {
		t.Fatalf("unexpected campaign after rollback: %+v", got)
	}

	// 停止下发：本批未下发/在处理设备都不能再领取。
	for _, id := range []string{"d3", "d4", "d6"} {
		if _, err := svc.ClaimCommand(ClaimCommand{CampaignID: "cp1", DeviceID: id}); !IsError(err, CodeCampaignNotActive) {
			t.Fatalf("device %s claimed after rollback: %v", id, err)
		}
	}

	// 回退指令只发给本批已成功且允许回退的 d1，恰好一条。
	events := rollbackEvents(svc, "")
	if len(events) != 1 || events[0].DeviceID != "d1" {
		t.Fatalf("rollback events = %+v, want only d1", events)
	}
	var payload map[string]any
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["installed_version"] != "2.0.0" || payload["rollback_version"] != "1.0.0" {
		t.Fatalf("rollback payload versions = %v", payload)
	}
	if payload["idempotency_key"] != rollbackKey("cp1", "d1") {
		t.Fatalf("rollback key = %v", payload["idempotency_key"])
	}

	u1, _ := svc.repo.GetUpgrade("cp1", "d1")
	if !u1.RollbackCommandSent || u1.Status != UpgradeStatusSucceeded {
		t.Fatalf("d1 upgrade = %+v", u1)
	}
	u5, _ := svc.repo.GetUpgrade("cp1", "d5")
	if u5.RollbackCommandSent {
		t.Fatalf("d5 must not receive rollback command")
	}

	// 终态不可逆：暂停/恢复/中止都被拒绝。
	if err := svc.Pause("cp1", "x"); !IsError(err, CodeInvalidTransition) {
		t.Fatalf("pause rolled_back: %v", err)
	}
	if err := svc.Resume("cp1"); !IsError(err, CodeInvalidTransition) {
		t.Fatalf("resume rolled_back: %v", err)
	}
	if err := svc.Abort("cp1", "x"); !IsError(err, CodeInvalidTransition) {
		t.Fatalf("abort rolled_back: %v", err)
	}
}

func TestRollback_BoundaryFailureCount(t *testing.T) {
	svc := newTestService()
	seedRollback(t, svc)
	// 上限 2：failed==2 不触发，failed==3 触发。
	c := createCampaign(t, svc, rollbackCampaignInput("cp1", 10, 2))
	failDevice(t, svc, c, "d2")
	failDevice(t, svc, c, "d3")
	if got, _ := svc.GetCampaign("cp1"); got.Status != CampaignStatusActive {
		t.Fatalf("status = %s at failed==limit, want active", got.Status)
	}
	failDevice(t, svc, c, "d4")
	if got, _ := svc.GetCampaign("cp1"); got.Status != CampaignStatusRolledBack {
		t.Fatalf("status = %s at failed==limit+1, want rolled_back", got.Status)
	}
}

// ---- 2. 以前完成的批次不受影响 ----

func TestRollback_PreviousWavesNotCommanded(t *testing.T) {
	svc := newTestService()
	seedRollback(t, svc)
	// 每波 2 台：wave1 = d1,d2；wave2 = d3,d4；d5,d6 在 wave3。
	// 上限 1：wave2 中两个失败即触发。
	c := createCampaign(t, svc, rollbackCampaignInput("cp1", 2, 1))
	succeed(t, svc, c, "d1") // wave1
	succeed(t, svc, c, "d2")
	if got, _ := svc.GetCampaign("cp1"); got.CurrentWave != 2 {
		t.Fatalf("expected wave 2, got %d", got.CurrentWave)
	}
	succeed(t, svc, c, "d3") // wave2，成功且允许回退
	failDevice(t, svc, c, "d4")
	// wave2 只有两台且已收敛：failed==1 == 上限，不触发回退，按比率门槛
	// （MaxFailureRate=1.0）达标放行 wave3。
	if got, _ := svc.GetCampaign("cp1"); got.Status != CampaignStatusActive || got.CurrentWave != 3 {
		t.Fatalf("expected advance to wave 3 at boundary, got %+v", got)
	}
	// wave3：d5 不允许回退、d6 不允许回退。制造 2 个失败需要更多设备——
	// 这里 d5 失败（第 1 个），再让 d6 失败即越过上限（当前波计数=2）。
	failDevice(t, svc, c, "d5")
	failDevice(t, svc, c, "d6")
	got, _ := svc.GetCampaign("cp1")
	if got.Status != CampaignStatusRolledBack || got.CurrentWave != 3 {
		t.Fatalf("expected rollback at wave 3, got %+v", got)
	}

	// wave1/wave2 的成功设备都不得收到回退指令（本批 wave3 无成功设备）。
	if evs := rollbackEvents(svc, ""); len(evs) != 0 {
		t.Fatalf("previous waves must not be rolled back, got %+v", evs)
	}
	// 历史结果保持不变。
	for _, id := range []string{"d1", "d2", "d3"} {
		u, _ := svc.repo.GetUpgrade("cp1", id)
		if u.Status != UpgradeStatusSucceeded || u.RollbackCommandSent {
			t.Fatalf("device %s mutated: %+v", id, u)
		}
	}
}

// ---- 3. 触发后在途设备迟到回执：成功补发一次，失败不回退，终态不倒退 ----

func TestRollback_InFlightReceiptsAfterTrigger(t *testing.T) {
	svc := newTestService()
	seedRollback(t, svc)
	c := createCampaign(t, svc, rollbackCampaignInput("cp1", 10, 1))

	r1 := claim(t, svc, "cp1", "d1") // 允许回退，先领取不回执
	r5 := claim(t, svc, "cp1", "d5") // 不允许回退
	r3 := claim(t, svc, "cp1", "d3") // 允许回退
	failDevice(t, svc, c, "d2")
	failDevice(t, svc, c, "d4") // 第 2 个失败触发；d1/d3/d5 仍在处理中
	if got, _ := svc.GetCampaign("cp1"); got.Status != CampaignStatusRolledBack {
		t.Fatalf("expected rolled_back, got %s", got.Status)
	}
	if evs := rollbackEvents(svc, ""); len(evs) != 0 {
		t.Fatalf("no installed devices at trigger, got %+v", evs)
	}

	// d1 迟到成功 → 补发回退指令。
	if err := svc.SubmitReceipt(ReceiptInput{
		CampaignID: c.ID, DeviceID: "d1",
		CampaignVer: r1.CampaignVer, IdempotencyKey: r1.IdempotencyKey,
		Result: ReceiptSucceeded,
	}); err != nil {
		t.Fatalf("late success d1: %v", err)
	}
	if evs := rollbackEvents(svc, "d1"); len(evs) != 1 {
		t.Fatalf("d1 rollback events = %d, want 1", len(evs))
	}

	// d5 迟到成功但不允许回退 → 不发令。
	if err := svc.SubmitReceipt(ReceiptInput{
		CampaignID: c.ID, DeviceID: "d5",
		CampaignVer: r5.CampaignVer, IdempotencyKey: r5.IdempotencyKey,
		Result: ReceiptSucceeded,
	}); err != nil {
		t.Fatalf("late success d5: %v", err)
	}
	if evs := rollbackEvents(svc, "d5"); len(evs) != 0 {
		t.Fatalf("d5 must not be commanded: %+v", evs)
	}

	// d3 迟到失败 → 不发令。
	if err := svc.SubmitReceipt(ReceiptInput{
		CampaignID: c.ID, DeviceID: "d3",
		CampaignVer: r3.CampaignVer, IdempotencyKey: r3.IdempotencyKey,
		Result: ReceiptFailed,
	}); err != nil {
		t.Fatalf("late fail d3: %v", err)
	}
	if evs := rollbackEvents(svc, "d3"); len(evs) != 0 {
		t.Fatalf("failed d3 must not be commanded: %+v", evs)
	}

	// d1 之后迟到的失败不能倒退成功，也不能再产生回退指令。
	if err := svc.SubmitReceipt(ReceiptInput{
		CampaignID: c.ID, DeviceID: "d1",
		CampaignVer: r1.CampaignVer, IdempotencyKey: r1.IdempotencyKey,
		Result: ReceiptFailed, Reason: "late",
	}); err != nil {
		t.Fatalf("late failure d1: %v", err)
	}
	u1, _ := svc.repo.GetUpgrade("cp1", "d1")
	if u1.Status != UpgradeStatusSucceeded {
		t.Fatalf("success regressed: %s", u1.Status)
	}
	if evs := rollbackEvents(svc, "d1"); len(evs) != 1 {
		t.Fatalf("d1 rollback events = %d, still want 1", len(evs))
	}
}

// ---- 4. 重复扫描与重启恢复都不重复发令 ----

func TestRollback_CommandIssuedExactlyOnceAcrossScansAndRestart(t *testing.T) {
	svc := newTestService()
	seedRollback(t, svc)
	c := createCampaign(t, svc, rollbackCampaignInput("cp1", 10, 1))
	succeed(t, svc, c, "d1")
	succeed(t, svc, c, "d3")
	failDevice(t, svc, c, "d2")
	failDevice(t, svc, c, "d4") // 触发，触发时即发出 2 条
	if evs := rollbackEvents(svc, ""); len(evs) != 2 {
		t.Fatalf("initial rollback events = %d, want 2", len(evs))
	}

	// 同一进程内重复扫描：不再发令。
	for i := 0; i < 3; i++ {
		n, err := svc.ScanRollbackCommands("cp1")
		if err != nil || n != 0 {
			t.Fatalf("repeat scan %d: n=%d err=%v", i, n, err)
		}
	}

	// 模拟重启：用同一仓储构造新服务，恢复扫描仍然不重复。
	restarted := NewService(svc.repo)
	n, err := restarted.ScanRollbackCommands("cp1")
	if err != nil || n != 0 {
		t.Fatalf("scan after restart: n=%d err=%v", n, err)
	}
	if evs := rollbackEvents(svc, ""); len(evs) != 2 {
		t.Fatalf("rollback events after restart scan = %d, want 2", len(evs))
	}

	// 非 rolled_back 活动扫描返回 0。
	svc2 := newTestService()
	seedRollback(t, svc2)
	createCampaign(t, svc2, rollbackCampaignInput("cp2", 10, 1))
	if n, err := svc2.ScanRollbackCommands("cp2"); err != nil || n != 0 {
		t.Fatalf("scan active campaign: n=%d err=%v", n, err)
	}
}

func TestRollback_ScanBackfillsInFlightSuccess(t *testing.T) {
	svc := newTestService()
	seedRollback(t, svc)
	c := createCampaign(t, svc, rollbackCampaignInput("cp1", 10, 1))
	r1 := claim(t, svc, "cp1", "d1") // 触发时仍 issued
	failDevice(t, svc, c, "d2")
	failDevice(t, svc, c, "d3") // 触发
	if evs := rollbackEvents(svc, ""); len(evs) != 0 {
		t.Fatalf("want no commands at trigger, got %d", len(evs))
	}

	// 重启恢复后 d1 才迟到成功：回执事务内补发；也验证纯扫描路径幂等。
	if err := svc.SubmitReceipt(ReceiptInput{
		CampaignID: c.ID, DeviceID: "d1",
		CampaignVer: r1.CampaignVer, IdempotencyKey: r1.IdempotencyKey,
		Result: ReceiptSucceeded,
	}); err != nil {
		t.Fatalf("late success: %v", err)
	}
	restarted := NewService(svc.repo)
	if n, _ := restarted.ScanRollbackCommands("cp1"); n != 0 {
		t.Fatalf("scan after backfill issued %d, want 0", n)
	}
	if evs := rollbackEvents(svc, "d1"); len(evs) != 1 {
		t.Fatalf("d1 events = %d, want 1", len(evs))
	}
}

// ---- 5. 自动回退与人工暂停/中止并发：只保留一种结果 ----

func TestRollback_ConcurrentWithPauseAndAbort(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		svc := newTestService()
		mustRegisterFirmware(t, svc, "fw1", "model-x", "2.0.0")
		const n = 12
		for i := 0; i < n; i++ {
			registerRB(t, svc, fmt.Sprintf("c%02d", i), "1.0.0", 80, true)
		}
		c := createCampaign(t, svc, CreateCampaignInput{
			ID: "cp-conc", FirmwareID: "fw1",
			DeviceSelector:       DeviceSelector{Model: "model-x", MinBatteryLevel: 20},
			BatchSize:            100,
			SuccessRateThreshold: 0.0,
			MaxFailureRate:       1.0,
			MaxFailuresPerWave:   1, // 第 2 个失败即回退
		})

		keys := map[string]struct {
			ver int64
			key string
		}{}
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("c%02d", i)
			r := claim(t, svc, c.ID, id)
			keys[id] = struct {
				ver int64
				key string
			}{r.CampaignVer, r.IdempotencyKey}
		}

		// 先让两台确定性地成功：若回退胜出，它们是回退指令的接收者。
		ok0, ok1 := keys["c00"], keys["c01"]
		if err := svc.SubmitReceipt(ReceiptInput{CampaignID: c.ID, DeviceID: "c00", CampaignVer: ok0.ver, IdempotencyKey: ok0.key, Result: ReceiptSucceeded}); err != nil {
			t.Fatalf("success c00: %v", err)
		}
		if err := svc.SubmitReceipt(ReceiptInput{CampaignID: c.ID, DeviceID: "c01", CampaignVer: ok1.ver, IdempotencyKey: ok1.key, Result: ReceiptSucceeded}); err != nil {
			t.Fatalf("success c01: %v", err)
		}

		var wg sync.WaitGroup
		for i := 2; i < n; i++ {
			id := fmt.Sprintf("c%02d", i)
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				k := keys[id]
				// 绝大多数失败，保证若没有人工操作抢先则必然触发回退；
				// 每个回执重复一次检验幂等。
				in := ReceiptInput{CampaignID: c.ID, DeviceID: id, CampaignVer: k.ver, IdempotencyKey: k.key, Result: ReceiptFailed}
				_ = svc.SubmitReceipt(in)
				_ = svc.SubmitReceipt(in)
			}(id)
		}
		wg.Add(2)
		go func() { defer wg.Done(); _ = svc.Pause(c.ID, "manual") }()
		go func() { defer wg.Done(); _ = svc.Abort(c.ID, "stop") }()
		wg.Wait()

		got, _ := svc.GetCampaign(c.ID)
		switch got.Status {
		case CampaignStatusRolledBack, CampaignStatusPaused, CampaignStatusAborted:
		default:
			t.Fatalf("iter %d: unexpected winner status %s", iter, got.Status)
		}

		// 回退指令存在 当且仅当 胜出结果是 rolled_back。
		rbCount := len(rollbackEvents(svc, ""))
		if got.Status == CampaignStatusRolledBack {
			if rbCount != 2 {
				t.Fatalf("iter %d: rolled_back but rollback commands = %d, want 2", iter, rbCount)
			}
		} else if rbCount != 0 {
			t.Fatalf("iter %d: winner %s but %d rollback commands exist", iter, got.Status, rbCount)
		}
		// 每台设备至多一条回退指令。
		perDevice := map[string]int{}
		for _, e := range rollbackEvents(svc, "") {
			perDevice[e.DeviceID]++
			if perDevice[e.DeviceID] > 1 {
				t.Fatalf("iter %d: device %s got %d rollback commands", iter, e.DeviceID, perDevice[e.DeviceID])
			}
		}
	}
}

// ---- 6. 进度查询的设备分类 ----

func TestRollback_ProgressClassification(t *testing.T) {
	svc := newTestService()
	seedRollback(t, svc)
	c := createCampaign(t, svc, rollbackCampaignInput("cp1", 10, 1))

	succeed(t, svc, c, "d1") // 已装 + 允许
	succeed(t, svc, c, "d5") // 已装 + 不允许
	claim(t, svc, "cp1", "d3")
	failDevice(t, svc, c, "d2")
	failDevice(t, svc, c, "d4") // 触发

	p, err := svc.GetProgress("cp1")
	if err != nil {
		t.Fatalf("GetProgress: %v", err)
	}
	if p.Status != CampaignStatusRolledBack {
		t.Fatalf("status = %s", p.Status)
	}
	if eq := eqStrings(p.RollbackCommanded, []string{"d1"}); !eq {
		t.Fatalf("commanded = %v", p.RollbackCommanded)
	}
	if eq := eqStrings(p.InstalledNotRollbackable, []string{"d5"}); !eq {
		t.Fatalf("not rollbackable = %v", p.InstalledNotRollbackable)
	}
	if eq := eqStrings(p.RollbackInFlight, []string{"d3", "d6"}); !eq {
		// d3 已领取、d6 尚未领取，都属于仍在处理中。
		t.Fatalf("in-flight = %v, want [d3 d6]", p.RollbackInFlight)
	}
	if len(p.RollbackWaiting) != 0 {
		t.Fatalf("waiting should be empty right after atomic trigger, got %v", p.RollbackWaiting)
	}
}

func eqStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// ---- 7. 未启用上限（MaxFailuresPerWave=0）时保持旧行为 ----

func TestRollback_DisabledKeepsRateBasedPause(t *testing.T) {
	svc := newTestService()
	seedRollback(t, svc)
	// 上限 0 禁用；失败率上限 0.2：6 台失败 2 台以上即自动暂停而非回退。
	in := rollbackCampaignInput("cp1", 10, 0)
	in.MaxFailureRate = 0.2
	c := createCampaign(t, svc, in)
	for _, id := range []string{"d2", "d3"} {
		failDevice(t, svc, c, id)
	}
	for _, id := range []string{"d1", "d4"} {
		succeed(t, svc, c, id)
	}
	// 波次尚未全部收敛，不结算；再失败一台并让最后一台成功使波次收敛。
	failDevice(t, svc, c, "d5")
	succeed(t, svc, c, "d6")
	got, _ := svc.GetCampaign("cp1")
	if got.Status != CampaignStatusPaused {
		t.Fatalf("status = %s, want rate-based paused", got.Status)
	}
	if evs := rollbackEvents(svc, ""); len(evs) != 0 {
		t.Fatalf("rollback must not fire when disabled, got %+v", evs)
	}
}
