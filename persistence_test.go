package firmwarecampaign

import (
	"path/filepath"
	"testing"
)

// TestPersistenceRoundtrip 验证状态与 outbox 经 FilePersister 持久化后
// 可被新服务实例完整恢复，且回执去重信息不丢失。
func TestPersistenceRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	pers := NewFilePersister(path)

	s1, err := NewService(pers)
	if err != nil {
		t.Fatal(err)
	}
	mustFirmware(t, s1, "fw1", "m1", "2.0.0", 0)
	mustDevice(t, s1, "d1", "m1", "1.0.0", 90)
	mustDevice(t, s1, "d2", "m1", "1.0.0", 90)
	c := mustCampaign(t, s1, "fw1", [][]string{{"d1", "d2"}}, 1, 1)

	cmd, err := s1.ClaimCommand(c.ID, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if got := mustReceipt(t, s1, Receipt{ID: "r1", CampaignID: c.ID, DeviceID: "d1",
		CampaignVersion: 1, IdempotencyKey: cmd.IdempotencyKey, Success: true}); got != ReceiptApplied {
		t.Fatalf("want applied, got %s", got)
	}
	outboxLen := len(s1.Outbox())

	// 用同一存储文件重建服务，状态应完整恢复。
	s2, err := NewService(pers)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s2.GetProgress(c.ID)
	if err != nil {
		t.Fatalf("progress after reload: %v", err)
	}
	if p.Succeeded != 1 || p.TotalDevices != 2 || p.Status != StatusRunning {
		t.Fatalf("state not restored: %+v", p)
	}
	if len(s2.Outbox()) != outboxLen {
		t.Fatalf("outbox not restored: want %d events, got %d", outboxLen, len(s2.Outbox()))
	}
	// 回执去重信息应保留：重放同一回执判重。
	if got := mustReceipt(t, s2, Receipt{ID: "r1", CampaignID: c.ID, DeviceID: "d1",
		CampaignVersion: 1, Success: true}); got != ReceiptDuplicate {
		t.Fatalf("receipt dedup lost after reload, got %s", got)
	}
	// 幂等键在重启后保持稳定。
	cmd2, err := s2.ClaimCommand(c.ID, "d1")
	if err == nil {
		// d1 已成功，应拒绝领取。
		t.Fatalf("claim for succeeded device should fail, got %+v", cmd2)
	}
	if _, err := s2.ClaimCommand(c.ID, "d2"); err != nil {
		t.Fatalf("claim d2 after reload: %v", err)
	}
}

// TestPersistenceFileAbsent 验证存储文件不存在时服务从空状态启动。
func TestPersistenceFileAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "state.json")
	// 目录不存在时 Load 返回空状态；首次 Save 才会写文件。
	pers := NewFilePersister(path)
	if _, err := NewService(pers); err != nil {
		t.Fatalf("NewService with absent file: %v", err)
	}
}
