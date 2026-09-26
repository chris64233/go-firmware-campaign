package firmwarecampaign

import "time"

// CampaignStatus 是活动的生命周期状态。状态只会单调向前：
// running -> paused -> running（可反复），最终进入 completed 或 aborted 之一。
type CampaignStatus string

const (
	StatusRunning   CampaignStatus = "running"
	StatusPaused    CampaignStatus = "paused"
	StatusCompleted CampaignStatus = "completed"
	StatusAborted   CampaignStatus = "aborted"
)

// Terminal 报告活动是否已进入终态（不再发放新指令）。
func (s CampaignStatus) Terminal() bool {
	return s == StatusCompleted || s == StatusAborted
}

// DeviceState 是设备在活动内的推进状态。状态按秩单调推进，
// succeeded 为吸收态：迟到的失败回执不得回退已成功的设备。
type DeviceState string

const (
	DevicePending    DeviceState = "pending"
	DeviceDispatched DeviceState = "dispatched"
	DeviceFailed     DeviceState = "failed"
	DeviceSucceeded  DeviceState = "succeeded"
)

// deviceStateRank 定义状态秩，只允许向更高秩迁移（failed -> succeeded 合法，
// succeeded 不可被任何回执覆盖）。
func deviceStateRank(s DeviceState) int {
	switch s {
	case DevicePending:
		return 0
	case DeviceDispatched:
		return 1
	case DeviceFailed:
		return 2
	case DeviceSucceeded:
		return 3
	default:
		return -1
	}
}

// Firmware 是登记在册的固件版本。
type Firmware struct {
	ID         string    `json:"id"`
	Model      string    `json:"model"`       // 适用设备型号
	Version    string    `json:"version"`     // 固件版本号
	Digest     string    `json:"digest"`      // 固件摘要（创建活动时冻结进活动）
	MinBattery int       `json:"min_battery"` // 升级所需最低电量（百分比）
	CreatedAt  time.Time `json:"created_at"`
}

// Device 是设备清单中的一台设备及其资格属性。
type Device struct {
	ID             string    `json:"id"`
	Model          string    `json:"model"`
	CurrentVersion string    `json:"current_version"`
	Battery        int       `json:"battery"` // 当前电量百分比
	UpdatedAt      time.Time `json:"updated_at"`
}

// Campaign 是一次分批固件升级活动。目标设备、固件摘要与波次划分
// 在创建时冻结，之后不可修改。
type Campaign struct {
	ID               string                     `json:"id"`
	FirmwareID       string                     `json:"firmware_id"`
	FirmwareVersion  string                     `json:"firmware_version"`
	FirmwareDigest   string                     `json:"firmware_digest"`
	Status           CampaignStatus             `json:"status"`
	Version          int                        `json:"version"` // 活动版本号，恢复时递增，用于识别过期回执
	Waves            [][]string                 `json:"waves"`   // 冻结的波次划分（设备 ID 列表）
	CurrentWave      int                        `json:"current_wave"`
	SuccessThreshold float64                    `json:"success_threshold"` // 当前波次成功率达到该值即开放下一波
	FailureThreshold float64                    `json:"failure_threshold"` // 当前波次失败率超过该值即自动暂停
	PauseReason      string                     `json:"pause_reason,omitempty"`
	Devices          map[string]*CampaignDevice `json:"devices"`
	CreatedAt        time.Time                  `json:"created_at"`
	UpdatedAt        time.Time                  `json:"updated_at"`
}

// CampaignDevice 是设备在某个活动内的冻结视图与推进状态。
type CampaignDevice struct {
	DeviceID  string      `json:"device_id"`
	Wave      int         `json:"wave"`
	State     DeviceState `json:"state"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// Command 是设备领取到的升级指令，携带活动版本号与稳定幂等键。
type Command struct {
	CampaignID      string    `json:"campaign_id"`
	CampaignVersion int       `json:"campaign_version"`
	DeviceID        string    `json:"device_id"`
	FirmwareID      string    `json:"firmware_id"`
	FirmwareVersion string    `json:"firmware_version"`
	Digest          string    `json:"digest"`
	IdempotencyKey  string    `json:"idempotency_key"`
	IssuedAt        time.Time `json:"issued_at"`
}

// Receipt 是设备上报的升级结果回执。回执可能重复或乱序到达。
type Receipt struct {
	ID              string `json:"id"` // 回执唯一 ID，用于去重
	CampaignID      string `json:"campaign_id"`
	DeviceID        string `json:"device_id"`
	CampaignVersion int    `json:"campaign_version"` // 指令上的活动版本号
	IdempotencyKey  string `json:"idempotency_key"`  // 指令上的幂等键（可选校验）
	Success         bool   `json:"success"`
	Detail          string `json:"detail,omitempty"`
}

// ReceiptResult 描述一条回执的处理结果。
type ReceiptResult string

const (
	ReceiptApplied    ReceiptResult = "applied"    // 已采纳并推进状态
	ReceiptDuplicate  ReceiptResult = "duplicate"  // 相同回执 ID 已处理过
	ReceiptStale      ReceiptResult = "stale"      // 旧活动版本的回执，已忽略
	ReceiptSuperseded ReceiptResult = "superseded" // 设备已处于更高秩状态，未覆盖
)

// OutboxEvent 是持久化 outbox 中的一条待投递事件。
type OutboxEvent struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	CampaignID string            `json:"campaign_id"`
	DeviceID   string            `json:"device_id,omitempty"`
	Payload    map[string]string `json:"payload,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
}

// outbox 事件类型。
const (
	EventCampaignCreated  = "campaign_created"
	EventCampaignPaused   = "campaign_paused"
	EventCampaignResumed  = "campaign_resumed"
	EventCampaignComplete = "campaign_completed"
	EventCampaignAborted  = "campaign_aborted"
	EventWaveAdvanced     = "wave_advanced"
	EventCompensation     = "compensation_required"
)

// CreateCampaignRequest 是创建活动的入参。
type CreateCampaignRequest struct {
	ID               string     // 可选，留空则自动生成
	FirmwareID       string     // 已登记的固件 ID
	Waves            [][]string // 波次划分（每个元素是一波设备 ID），创建后冻结
	SuccessThreshold float64    // (0, 1]
	FailureThreshold float64    // [0, 1]，失败率超过该值自动暂停；1 表示永不自动暂停
}

// WaveProgress 是单个波次的进度统计，分母为冻结的波次设备数。
type WaveProgress struct {
	Index       int     `json:"index"`
	Total       int     `json:"total"`
	Pending     int     `json:"pending"`
	Dispatched  int     `json:"dispatched"`
	Succeeded   int     `json:"succeeded"`
	Failed      int     `json:"failed"`
	SuccessRate float64 `json:"success_rate"`
	FailureRate float64 `json:"failure_rate"`
}

// Progress 是活动进度查询的返回视图。
type Progress struct {
	CampaignID   string         `json:"campaign_id"`
	Status       CampaignStatus `json:"status"`
	Version      int            `json:"version"`
	CurrentWave  int            `json:"current_wave"`
	TotalDevices int            `json:"total_devices"`
	Succeeded    int            `json:"succeeded"`
	Failed       int            `json:"failed"`
	PauseReason  string         `json:"pause_reason,omitempty"`
	Waves        []WaveProgress `json:"waves"`
}
