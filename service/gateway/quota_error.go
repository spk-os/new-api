package gateway

import (
	"net/http"
	"strings"
)

// IsQuotaExhaustedError reports whether an upstream failure is a
// quota-exhaustion rejection, e.g. DevEco's
// "You have used up your free quota for the current cycle ... Your quota
// will automatically reset at ...", or WorkBuddy's 402 hard_credit
// (积分不足，充值前对该 provider 永久无效). Such errors are permanent for the
// provider until the quota cycle resets, so callers must not retry and
// should place the provider into cooldown immediately.
//
// Matching is deliberately conservative — a 403 status plus both "quota"
// and "reset" keywords in the message — so ordinary 403 rejections (auth
// failure, context-length disguise) keep their existing retry behavior.
// 402 Payment Required has no such disguise scenario and maps unconditionally.
func IsQuotaExhaustedError(statusCode int, message string) bool {
	if statusCode == http.StatusPaymentRequired {
		return true
	}
	if statusCode != http.StatusForbidden {
		return false
	}
	msg := strings.ToLower(message)
	return strings.Contains(msg, "quota") && strings.Contains(msg, "reset")
}
