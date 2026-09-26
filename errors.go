package firmwarecampaign

import (
	"errors"
	"fmt"
)

// 错误码。
const (
	CodeInvalidArgument   = "invalid_argument"
	CodeNotFound          = "not_found"
	CodeAlreadyExists     = "already_exists"
	CodeConflict          = "conflict"
	CodeDeviceBusy        = "device_busy"
	CodeCampaignNotActive = "campaign_not_active"
	CodeNotCurrentWave    = "not_current_wave"
	CodeStaleReceipt      = "stale_receipt"
	CodeUpgradeTerminal   = "upgrade_terminal"
	CodeInvalidTransition = "invalid_transition"
)

// 可通过 errors.Is 判定的哨兵错误。
var (
	ErrInvalidArgument   = &Error{Code: CodeInvalidArgument, Message: "invalid argument"}
	ErrNotFound          = &Error{Code: CodeNotFound, Message: "not found"}
	ErrAlreadyExists     = &Error{Code: CodeAlreadyExists, Message: "already exists"}
	ErrConflict          = &Error{Code: CodeConflict, Message: "conflict"}
	ErrDeviceBusy        = &Error{Code: CodeDeviceBusy, Message: "device is locked by another active campaign"}
	ErrCampaignNotActive = &Error{Code: CodeCampaignNotActive, Message: "campaign is not active"}
	ErrNotCurrentWave    = &Error{Code: CodeNotCurrentWave, Message: "device does not belong to the current wave"}
	ErrStaleReceipt      = &Error{Code: CodeStaleReceipt, Message: "receipt is stale and cannot overwrite newer state"}
	ErrUpgradeTerminal   = &Error{Code: CodeUpgradeTerminal, Message: "upgrade is already in a terminal state"}
	ErrInvalidTransition = &Error{Code: CodeInvalidTransition, Message: "invalid campaign state transition"}
)

// Error 是服务返回的统一错误类型，携带稳定的错误码与可读信息。
type Error struct {
	Code    string
	Message string
	Op      string // 可选：产生错误的操作名
	Detail  string // 可选：补充细节
	Err     error  // 被包装的哨兵错误
}

func (e *Error) Error() string {
	msg := e.Message
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if e.Op != "" {
		return fmt.Sprintf("%s: %s", e.Op, msg)
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

func wrapError(op string, sentinel *Error, detailFmt string, args ...any) error {
	return &Error{
		Code:    sentinel.Code,
		Message: sentinel.Message,
		Op:      op,
		Detail:  fmt.Sprintf(detailFmt, args...),
		Err:     sentinel,
	}
}

// IsError 判断 err 是否携带指定错误码。
func IsError(err error, code string) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Code == code
	}
	return false
}
