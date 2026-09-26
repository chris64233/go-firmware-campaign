package firmwarecampaign

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// 设备相关状态。
type DeviceStatus string

const (
	DeviceStatusOnline  DeviceStatus = "online"
	DeviceStatusOffline DeviceStatus = "offline"
)

// Device 是设备快照。活动创建时只会读取（冻结）其属性，后续升级结果
// 通过 ApplyUpgradeResult 回写设备当前版本。
type Device struct {
	ID             string
	Model          string
	CurrentVersion string
	BatteryLevel   int // 0-100
	Status         DeviceStatus
	UpdatedAt      time.Time
}

// Firmware 是登记过的固件。
type Firmware struct {
	ID          string
	Model       string
	Version     string
	SHA256      string
	SizeBytes   int64
	Description string
	CreatedAt   time.Time
}

// Checksum 返回固件摘要，登记时若未显式提供则由型号与版本派生，
// 便于测试；生产环境必须显式传入真实 SHA256。
func (f *Firmware) Checksum() string {
	if f.SHA256 != "" {
		return f.SHA256
	}
	sum := sha256.Sum256([]byte(f.Model + ":" + f.Version))
	return hex.EncodeToString(sum[:])
}

// 活动生命周期状态。
//
// 状态机：
//
//	active ──pause──> paused ──resume──> active
//	  │                  │
//	  ├──(末波达标)─────────────────────> completed
//	  └──abort───────────┴──> aborted
//
// active 内部按波次推进；最后一波收敛达标后进入 completed（终态）。
// 波次失败率超阈值时由系统自动转入 paused，等待运维 Resume 确认放行。
type CampaignStatus string

const (
	CampaignStatusActive    CampaignStatus = "active"
	CampaignStatusPaused    CampaignStatus = "paused"
	CampaignStatusAborted   CampaignStatus = "aborted"
	CampaignStatusCompleted CampaignStatus = "completed"
)

// 单台设备升级状态。状态只允许单调前进。
type UpgradeStatus string

const (
	UpgradeStatusPending   UpgradeStatus = "pending"   // 已冻结，尚未下发
	UpgradeStatusIssued    UpgradeStatus = "issued"    // 指令已被设备领取
	UpgradeStatusSucceeded UpgradeStatus = "succeeded" // 成功（终态）
	UpgradeStatusFailed    UpgradeStatus = "failed"    // 失败（终态）
)

// rank 定义升级状态的单调等级，旧回执的等级不允许覆盖更高等级。
func (s UpgradeStatus) rank() int {
	switch s {
	case UpgradeStatusPending:
		return 0
	case UpgradeStatusIssued:
		return 1
	case UpgradeStatusFailed:
		return 2
	case UpgradeStatusSucceeded:
		return 3
	default:
		return -1
	}
}

// Terminal 报告升级状态是否为终态。
func (s UpgradeStatus) Terminal() bool {
	return s == UpgradeStatusSucceeded || s == UpgradeStatusFailed
}

// DeviceUpgrade 是某台设备在某个活动中的（被冻结的）升级条目。
type DeviceUpgrade struct {
	DeviceID       string
	CampaignID     string
	Wave           int
	Status         UpgradeStatus
	CampaignVer    int64 // 领取时记录的活动版本，回执必须匹配
	IdempotencyKey string
	TargetVersion  string
	IssuedAt       time.Time
	ResolvedAt     time.Time
	FailureReason  string
	Attempts       int
	// ReceiptVer 是已接受回执的最高活动版本水位，旧版本回执一律拒绝。
	ReceiptVer int64
	// CompensationSent 标记补偿通知是否已生成（每台设备至多一次）。
	CompensationSent bool
}

// WaveStat 是一个波次的实时统计。
type WaveStat struct {
	Wave        int
	Total       int // 冻结的设备分母（恒定）
	Succeeded   int
	Failed      int
	InProgress  int // pending + issued
	SuccessRate float64
	Open        bool // 是否仍是当前波次
}

// Campaign 是活动聚合根（持久化记录）。
type Campaign struct {
	ID             string
	FirmwareID     string
	Model          string // 冻结自固件
	TargetVersion  string // 冻结自固件
	FirmwareDigest string // 冻结的固件摘要
	Status         CampaignStatus
	CurrentWave    int
	TotalWaves     int
	Version        int64 // 活动版本号，每次状态/波次推进 +1
	SuccessRate    float64
	MaxFailureRate float64
	BatchSize      int
	PauseReason    string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	AbortedAt      time.Time
}

// CampaignSnapshot 是活动创建时冻结的完整内容（用于审计）。
type CampaignSnapshot struct {
	CampaignID     string
	FirmwareID     string
	FirmwareDigest string
	TargetVersion  string
	Model          string
	DeviceIDs      []string
	Waves          [][]string
	CreatedAt      time.Time
}

// Progress 是进度查询结果。
type Progress struct {
	CampaignID  string
	Status      CampaignStatus
	CurrentWave int
	TotalWaves  int
	Version     int64
	Total       int
	Succeeded   int
	Failed      int
	InProgress  int
	SuccessRate float64
	PauseReason string
	Waves       []WaveStat
	Finished    bool // 所有波次设备均已进入终态（可能是 aborted 后全部收敛）
}

// 指令状态（与升级状态同源，面向设备的回执取值）。
type ReceiptResult string

const (
	ReceiptSucceeded ReceiptResult = "succeeded"
	ReceiptFailed    ReceiptResult = "failed"
)

// ---- 请求 / 响应 DTO ----

type RegisterFirmwareInput struct {
	ID          string
	Model       string
	Version     string
	SHA256      string // 可选，为空时由 model:version 派生（仅限测试）
	SizeBytes   int64
	Description string
}

type CreateCampaignInput struct {
	ID                   string
	FirmwareID           string
	DeviceSelector       DeviceSelector
	BatchSize            int     // 每波设备数，>0
	SuccessRateThreshold float64 // 当前波成功率达到该值才开放下一波，[0,1]
	MaxFailureRate       float64 // 当前波失败率超过该值则自动暂停，[0,1]
}

// DeviceSelector 是设备资格条件。
type DeviceSelector struct {
	Model              string
	MinBatteryLevel    int
	AllowedFromVersion []string // 为空表示不限制当前版本
}

type ClaimCommand struct {
	CampaignID     string
	DeviceID       string
	IdempotencyKey string // 可空：首次领取时由服务端分配稳定键；后续领取须原样回传
}

type ClaimResult struct {
	CampaignID     string
	DeviceID       string
	FirmwareID     string
	TargetVersion  string
	FirmwareDigest string
	Wave           int
	CampaignVer    int64
	IdempotencyKey string
	AlreadyIssued  bool // true 表示同一幂等键的重复领取
}

type ReceiptInput struct {
	CampaignID     string
	DeviceID       string
	CampaignVer    int64
	IdempotencyKey string
	Result         ReceiptResult
	Reason         string
}

// OutboxEvent 是与业务状态在同一事务中写入的出站事件。
type OutboxEvent struct {
	ID         int64
	EventType  string
	CampaignID string
	DeviceID   string
	Payload    []byte
	CreatedAt  time.Time
	Dispatched bool
}

// 事件类型。
const (
	EventCampaignCreated   = "campaign.created"
	EventCampaignPaused    = "campaign.paused"
	EventCampaignResumed   = "campaign.resumed"
	EventCampaignAborted   = "campaign.aborted"
	EventWaveAdvanced      = "campaign.wave_advanced"
	EventCampaignCompleted = "campaign.completed"
	EventUpgradeSucceeded  = "upgrade.succeeded"
	EventUpgradeFailed     = "upgrade.failed"
	EventCompensation      = "upgrade.compensation"
)
