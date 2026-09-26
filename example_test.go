package firmwarecampaign_test

import (
	"context"
	"fmt"
	"log"

	fwc "github.com/chris64233/go-firmware-campaign"
)

// ExampleService 演示固件登记、活动创建、分批领取、回执与进度查询的完整流程。
func ExampleService() {
	svc := fwc.NewService(fwc.NewMemoryRepository())

	if _, err := svc.RegisterFirmware(fwc.RegisterFirmwareInput{
		ID: "fw-2.0.0", Model: "sensor-x", Version: "2.0.0", SHA256: "abc123",
	}); err != nil {
		log.Fatal(err)
	}
	for _, id := range []string{"dev-1", "dev-2", "dev-3"} {
		if _, err := svc.RegisterDevice(fwc.Device{
			ID: id, Model: "sensor-x", CurrentVersion: "1.9.0", BatteryLevel: 80,
		}); err != nil {
			log.Fatal(err)
		}
	}

	// 每批 2 台、成功率门槛 80%、失败率上限 20%。
	c, snap, err := svc.CreateCampaign(fwc.CreateCampaignInput{
		ID:         "cp-1",
		FirmwareID: "fw-2.0.0",
		DeviceSelector: fwc.DeviceSelector{
			Model:           "sensor-x",
			MinBatteryLevel: 50,
		},
		BatchSize:            2,
		SuccessRateThreshold: 0.8,
		MaxFailureRate:       0.2,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("waves:", c.TotalWaves, "wave1 devices:", len(snap.Waves[0]))

	// 第一波设备领取指令（带活动版本号与稳定幂等键）。
	cmd, err := svc.ClaimCommand(fwc.ClaimCommand{CampaignID: "cp-1", DeviceID: "dev-1"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("claimed ver=%d key=%s\n", cmd.CampaignVer, cmd.IdempotencyKey)

	// 设备回报成功（重复回执是安全的）。
	receipt := fwc.ReceiptInput{
		CampaignID: "cp-1", DeviceID: "dev-1",
		CampaignVer: cmd.CampaignVer, IdempotencyKey: cmd.IdempotencyKey,
		Result: fwc.ReceiptSucceeded,
	}
	if err := svc.SubmitReceipt(receipt); err != nil {
		log.Fatal(err)
	}
	if err := svc.SubmitReceipt(receipt); err != nil {
		log.Fatal(err)
	}

	// 第二台失败：成功率 0.5 < 0.8，活动自动暂停。
	cmd2, _ := svc.ClaimCommand(fwc.ClaimCommand{CampaignID: "cp-1", DeviceID: "dev-2"})
	_ = svc.SubmitReceipt(fwc.ReceiptInput{
		CampaignID: "cp-1", DeviceID: "dev-2",
		CampaignVer: cmd2.CampaignVer, IdempotencyKey: cmd2.IdempotencyKey,
		Result: fwc.ReceiptFailed, Reason: "checksum mismatch",
	})
	p, _ := svc.GetProgress("cp-1")
	fmt.Println("status:", p.Status, "wave:", p.CurrentWave)

	// 运维确认后恢复，放行第二波。
	if err := svc.Resume("cp-1"); err != nil {
		log.Fatal(err)
	}
	p, _ = svc.GetProgress("cp-1")
	fmt.Println("resumed wave:", p.CurrentWave)

	// 事务性 outbox：事件至少投递一次。
	n, err := svc.DispatchOutbox(context.Background(), func(_ context.Context, e fwc.OutboxEvent) error {
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("dispatched events:", n > 0)

	// Output:
	// waves: 2 wave1 devices: 2
	// claimed ver=1 key=upg:cp-1:dev-1
	// status: paused wave: 1
	// resumed wave: 2
	// dispatched events: true
}
