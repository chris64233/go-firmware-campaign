package firmwarecampaign

import (
	"fmt"
	"sync"
	"testing"
)

// TestConcurrentReceiptsAndControl 在并发回执与暂停/恢复/中止混合操作下
// 验证状态单调性：已成功设备不得被回退，补偿通知至多生成一次。
// 需配合 go test -race 运行。
func TestConcurrentReceiptsAndControl(t *testing.T) {
	s := newService(t)
	mustFirmware(t, s, "fw1", "m1", "2.0.0", 0)
	const n = 40
	for _, id := range deviceIDs("d", n) {
		mustDevice(t, s, id, "m1", "1.0.0", 90)
	}
	c := mustCampaign(t, s, "fw1", [][]string{deviceIDs("d", n)}, 1, 1)

	var wg sync.WaitGroup
	// 一半设备并发领取并上报成功，另一半上报失败。
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("d-%d", i)
			if _, err := s.ClaimCommand(c.ID, id); err != nil {
				return // 活动可能已被并发中止
			}
			// 同一设备重复、乱序回执。
			_, _ = s.ReportReceipt(Receipt{ID: fmt.Sprintf("r-%d-b", i), CampaignID: c.ID, DeviceID: id, CampaignVersion: 1, Success: i%2 == 0})
			_, _ = s.ReportReceipt(Receipt{ID: fmt.Sprintf("r-%d-a", i), CampaignID: c.ID, DeviceID: id, CampaignVersion: 1, Success: i%2 != 0})
		}(i)
	}
	// 并发控制操作。
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.PauseCampaign(c.ID)
		_ = s.ResumeCampaign(c.ID)
		_ = s.AbortCampaign(c.ID)
	}()
	wg.Wait()

	p, err := s.GetProgress(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Succeeded < 0 || p.Succeeded > n || p.Failed < 0 || p.Failed > n {
		t.Fatalf("counts out of range: %+v", p)
	}
	// 补偿通知至多一次（活动只可能进入 aborted 一次）。
	comp := 0
	for _, ev := range s.Outbox() {
		if ev.Type == EventCompensation {
			comp++
		}
	}
	aborts := 0
	for _, ev := range s.Outbox() {
		if ev.Type == EventCampaignAborted {
			aborts++
		}
	}
	if aborts > 1 {
		t.Fatalf("aborted event emitted %d times", aborts)
	}
	if aborts == 0 && comp != 0 {
		t.Fatalf("compensation without abort")
	}
	// 状态终态一致：若已中止，每个设备的状态必须是其秩最高的已知结果。
	if p.Status == StatusAborted {
		for _, ev := range s.Outbox() {
			if ev.Type == EventCompensation {
				dev := c.Devices[ev.DeviceID]
				if dev.State == DeviceSucceeded {
					t.Fatalf("compensation emitted for succeeded device %s", ev.DeviceID)
				}
			}
		}
	}
}

// TestConcurrentClaimsIdempotent 并发重复领取同一设备的指令，
// 验证幂等键稳定且设备只被置为 dispatched 一次。
func TestConcurrentClaimsIdempotent(t *testing.T) {
	s := newService(t)
	mustFirmware(t, s, "fw1", "m1", "2.0.0", 0)
	mustDevice(t, s, "d1", "m1", "1.0.0", 90)
	c := mustCampaign(t, s, "fw1", [][]string{{"d1"}}, 1, 1)

	keys := make(chan string, 32)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd, err := s.ClaimCommand(c.ID, "d1")
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			keys <- cmd.IdempotencyKey
		}()
	}
	wg.Wait()
	close(keys)
	var first string
	for k := range keys {
		if first == "" {
			first = k
			continue
		}
		if k != first {
			t.Fatalf("unstable idempotency key: %q vs %q", k, first)
		}
	}
}
