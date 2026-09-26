package firmwarecampaign

import (
	"fmt"
	"sort"
	"strings"
)

// 哨兵错误，调用方可用 errors.Is 判定。
var (
	ErrFirmwareNotFound    = fmt.Errorf("firmware not found")
	ErrFirmwareExists      = fmt.Errorf("firmware already registered")
	ErrDeviceNotFound      = fmt.Errorf("device not found")
	ErrCampaignNotFound    = fmt.Errorf("campaign not found")
	ErrDeviceNotInCampaign = fmt.Errorf("device is not part of the campaign")
	ErrNotCurrentWave      = fmt.Errorf("device is not in the current wave")
	ErrCampaignNotRunning  = fmt.Errorf("campaign is not running")
	ErrCampaignNotPaused   = fmt.Errorf("campaign is not paused")
	ErrCampaignTerminal    = fmt.Errorf("campaign is in a terminal state")
	ErrDeviceConflict      = fmt.Errorf("device already enrolled in an active campaign")
	ErrDeviceFinished      = fmt.Errorf("device already reached a terminal state in the campaign")
	ErrInvalidRequest      = fmt.Errorf("invalid request")
	ErrFutureVersion       = fmt.Errorf("receipt carries a campaign version newer than the service")
	ErrKeyMismatch         = fmt.Errorf("idempotency key does not match the issued command")
)

// IneligibilityError 列出每台不满足资格条件的设备及其原因。
type IneligibilityError struct {
	Reasons map[string][]string
}

func (e *IneligibilityError) Error() string {
	ids := make([]string, 0, len(e.Reasons))
	for id := range e.Reasons {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString("devices ineligible for campaign: ")
	for i, id := range ids {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s (%s)", id, strings.Join(e.Reasons[id], ", "))
	}
	return b.String()
}

// ConflictError 列出因已加入其他互斥活动而冲突的设备。
type ConflictError struct {
	Devices map[string]string // deviceID -> 占用它的活动 ID
}

func (e *ConflictError) Error() string {
	ids := make([]string, 0, len(e.Devices))
	for id := range e.Devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString("devices already enrolled in an active campaign: ")
	for i, id := range ids {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s (campaign %s)", id, e.Devices[id])
	}
	return b.String()
}

// Unwrap 使 errors.Is(err, ErrDeviceConflict) 成立。
func (e *ConflictError) Unwrap() error { return ErrDeviceConflict }
