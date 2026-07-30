package service

import (
	"fmt"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/gin-gonic/gin"
)

// Channel-level circuit breaker / cooldown mechanism.
//
// When a channel returns a rate-limit error (429) it is immediately placed
// in cooldown for CooldownDurationSec seconds. During cooldown the channel
// is excluded from selection so requests route to healthy channels without
// wasting time on a known-rate-limited upstream.
//
// For other retryable errors (5xx, network) the channel enters cooldown
// after GeneralFailureThreshold consecutive failures within
// FailureWindowSec seconds.
//
// Cooldown expires automatically — the next request that would have selected
// the channel serves as a probe. If the probe succeeds the failure state is
// cleared; if it fails again the channel re-enters cooldown.

const (
	// CooldownDurationSec is how long a channel stays in cooldown.
	CooldownDurationSec = 180 // 3 minutes
	// RateLimitFailureThreshold is the failure count that triggers cooldown for 429/402.
	RateLimitFailureThreshold = 1
	// GeneralFailureThreshold is the failure count that triggers cooldown for other errors.
	GeneralFailureThreshold = 3
	// FailureWindowSec is the sliding window for counting consecutive failures.
	FailureWindowSec = 300 // 5 minutes
)

// channelCooldownEntry tracks per-channel failure and cooldown state.
type channelCooldownEntry struct {
	FailureCount       int
	FailureWindowStart time.Time
	CooldownUntil      time.Time
	LastStatusCode     int
}

var (
	channelCooldownMap sync.Map
	channelCooldownMu  sync.Mutex
	channelNowFunc     = time.Now
)

// isRateLimitStatus returns true for HTTP status codes that indicate the
// upstream provider is rate-limiting the channel (not a model or request
// error). A single such failure is enough to trigger cooldown.
func isRateLimitStatus(statusCode int) bool {
	return statusCode == 429 || statusCode == 402
}

// RecordChannelFailure increments the failure count for a channel and
// places it in cooldown when the threshold is reached. The threshold is 1
// for rate-limit errors (429/402) and GeneralFailureThreshold for others.
func RecordChannelFailure(channelId int, statusCode int) {
	if channelId <= 0 {
		return
	}

	threshold := GeneralFailureThreshold
	if isRateLimitStatus(statusCode) {
		threshold = RateLimitFailureThreshold
	}

	now := channelNowFunc()
	channelCooldownMu.Lock()
	defer channelCooldownMu.Unlock()

	var e *channelCooldownEntry
	if v, ok := channelCooldownMap.Load(channelId); ok {
		e, _ = v.(*channelCooldownEntry)
	}
	if e == nil {
		e = &channelCooldownEntry{}
	}

	// Reset failure window if expired
	if e.FailureWindowStart.IsZero() ||
		now.Sub(e.FailureWindowStart) > time.Duration(FailureWindowSec)*time.Second {
		e.FailureWindowStart = now
		e.FailureCount = 0
	}

	e.FailureCount++
	e.LastStatusCode = statusCode

	if e.FailureCount >= threshold {
		e.CooldownUntil = now.Add(time.Duration(CooldownDurationSec) * time.Second)
		channelCooldownMap.Store(channelId, e)
		common.SysLog(fmt.Sprintf("渠道 #%d 因连续失败 (%d次, status=%d) 进入冷却期 %d秒",
			channelId, e.FailureCount, statusCode, CooldownDurationSec))
	} else {
		channelCooldownMap.Store(channelId, e)
	}
}

// IsChannelInCooldown reports whether the channel is currently in cooldown.
func IsChannelInCooldown(channelId int) bool {
	if channelId <= 0 {
		return false
	}
	v, ok := channelCooldownMap.Load(channelId)
	if !ok {
		return false
	}
	e, _ := v.(*channelCooldownEntry)
	if e == nil {
		return false
	}
	return e.CooldownUntil.After(channelNowFunc())
}

// RecordChannelSuccess clears all failure and cooldown state for a channel.
// Called when a request through the channel succeeds.
func RecordChannelSuccess(channelId int) {
	if channelId <= 0 {
		return
	}
	channelCooldownMu.Lock()
	defer channelCooldownMu.Unlock()
	// Only log if the channel was actually in cooldown
	if v, ok := channelCooldownMap.Load(channelId); ok {
		e, _ := v.(*channelCooldownEntry)
		if e != nil && e.CooldownUntil.After(channelNowFunc()) {
			common.SysLog(fmt.Sprintf("渠道 #%d 恢复正常，退出冷却期", channelId))
		}
	}
	channelCooldownMap.Delete(channelId)
}

// ClearChannelCooldown removes all failure and cooldown state for a channel.
// Used when a channel is auto-recovered as a last-resort fallback.
func ClearChannelCooldown(channelId int) {
	if channelId <= 0 {
		return
	}
	channelCooldownMap.Delete(channelId)
}

// GetCooldownChannelIds returns the IDs of all channels currently in cooldown.
func GetCooldownChannelIds() []int {
	now := channelNowFunc()
	var out []int
	channelCooldownMap.Range(func(k, v interface{}) bool {
		id, _ := k.(int)
		e, _ := v.(*channelCooldownEntry)
		if e != nil && e.CooldownUntil.After(now) {
			out = append(out, id)
		}
		return true
	})
	return out
}

// LogCooldownStatus writes a summary of cooldown state to the request log
// for debugging visibility.
func LogCooldownStatus(c *gin.Context) {
	ids := GetCooldownChannelIds()
	if len(ids) == 0 {
		return
	}
	logger.LogInfo(c, fmt.Sprintf("渠道冷却状态: %v", ids))
}
