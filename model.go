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
	// RollbackSupported 表示设备硬件/策略是否支持版本回退；活动创建时
	// 冻结，决定熔断后该设备是否会收到回退指令。
	RollbackSupported bool
	UpdatedAt         time.Time
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
//	  ├──(本批失败越限, 自动回退)────────> rolling_back
//	  └──abort───────────┴──> aborted
//
// active 内部按波次推进；最后一波收敛达标后进入 completed（终态）。
// 波次失败率超阈值时由系统自动转入 paused，等待运维 Resume 确认放行。
//
// rolling_back 是失败计数熔断的终态：本批失败台数越过 MaxWaveFailures
// 的瞬间在同一事务内原子停止全部下发，并仅为本批已安装成功且具备回退
// 条件的设备生成一次性回退指令；活动此后不再接受暂停/恢复/中止。
type CampaignStatus string

const (
	CampaignStatusActive      CampaignStatus = "active"
	CampaignStatusPaused      CampaignStatus = "paused"
	CampaignStatusAborted     CampaignStatus = "aborted"
	CampaignStatusCompleted   CampaignStatus = "completed"
	CampaignStatusRollingBack CampaignStatus = "rolling_back"
)

// Terminal 报告活动状态是否不可再转移。
func (s CampaignStatus) Terminal() bool {
	return s == CampaignStatusAborted || s == CampaignStatusCompleted || s == CampaignStatusRollingBack
}

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
	// FromVersion 冻结自活动创建时的设备当前版本，即回退指令要回到的版本。
	FromVersion string
	// RollbackAllowed 冻结自设备是否支持回退，决定熔断后是否下发回退指令。
	RollbackAllowed bool
	// RollbackState 是熔断触发瞬间对该设备作出的一次性分类，此后冻结，
	// 迟到回执不会改变它。空串表示该设备所属波次尚未触发回退。
	RollbackState RollbackState
	// RollbackNotified 标记回退指令是否已通知（每个需要补偿的设备恰好一次，
	// 重复扫描、重启恢复都不会重复发令）。
	RollbackNotified bool
}

// RollbackState 是自动回退触发瞬间对单台设备的分类。
type RollbackState string

const (
	// RollbackStateCommand：触发时已安装成功且允许回退，已生成回退指令。
	RollbackStateCommand RollbackState = "command"
	// RollbackStateSkipped：触发时已安装成功，但设备不允许/无条件回退。
	RollbackStateSkipped RollbackState = "skipped"
	// RollbackStateInProgress：触发时仍在处理中（pending/issued），不回退。
	RollbackStateInProgress RollbackState = "in_progress"
	// RollbackStateFailed：本批失败设备，无需回退。
	RollbackStateFailed RollbackState = "failed"
)

// RollbackCommand 是为单台设备生成的带版本回退指令。
type RollbackCommand struct {
	CampaignID     string
	DeviceID       string
	Wave           int
	FromVersion    string // 当前已安装、需要回退掉的版本（= 活动目标版本）
	ToVersion      string // 回退目标版本（冻结自升级前）
	CampaignVer    int64  // 触发回退时的活动版本号
	IdempotencyKey string // 稳定幂等键 rb:<campaign>:<device>
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
	// Rollback 仅对触发自动回退的那一个波次非空，描述其设备分类结果。
	Rollback *WaveRollback
}

// WaveRollback 描述自动回退触发波次的设备分类计数。触发瞬间每台设备被
// 一次性分类（见 DeviceUpgrade.RollbackState），此后冻结，迟到回执不能
// 改变分类；回退指令清单由 GetProgress 依据分类构建。
type WaveRollback struct {
	Triggered  bool
	Installed  int // 触发时已成功安装
	InProgress int // 触发时仍在处理中（pending + issued）
	Failed     int // 触发时已失败
	Notified   int // 已生成回退通知的设备数
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
	// MaxWaveFailures 是每个波次允许的失败台数上限；当前波失败计数越过
	// （严格大于）该值即原子触发自动回退。<=0 表示不启用计数熔断。
	MaxWaveFailures int
	BatchSize       int
	PauseReason     string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	AbortedAt       time.Time
	// RollbackWave 是触发自动回退的波次（0 表示尚未触发）；触发后冻结。
	RollbackWave int
	// RollbackReason 记录熔断原因。
	RollbackReason string
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
	// Rollback 在活动触发自动回退后非空，汇总触发波次的设备分类与回退指令。
	Rollback *ProgressRollback
}

// ProgressRollback 汇总自动回退的触发信息与本批设备分类。
type ProgressRollback struct {
	Wave   int
	Reason string
	// Installed：触发时已安装成功；其中允许回退者对应 Commands 中的一条指令。
	Installed int
	// InProgress：触发时仍在处理中（pending/issued），不生成回退指令。
	InProgress int
	Failed     int
	// Notified：已生成并写入 outbox 的回退通知数（= len(Commands)）。
	Notified int
	Commands []RollbackCommand
	// Skipped：已安装成功但不具备回退条件（不支持回退或无历史版本）的设备。
	Skipped []string
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
	// MaxWaveFailures 是每波失败台数的绝对上限；当前波失败数严格大于该值
	// 即原子触发自动回退（停止下发并对本批已安装设备生成回退指令）。
	// <=0 表示不启用失败计数熔断，仅按失败率自动暂停。
	MaxWaveFailures int
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
	// EventWaveRollingBack：本批失败越限，活动原子进入 rolling_back。
	EventWaveRollingBack = "campaign.wave_rolling_back"
	// EventRollbackCommand：向单台设备下发的带版本回退指令（每台至多一次）。
	EventRollbackCommand = "rollback.command"
)
