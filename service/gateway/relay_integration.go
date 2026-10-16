package gateway

import (
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
)

// GetGatewayPlanFromContext extracts the gateway plan stored by
// GatewayStrategyMiddleware. Returns nil for non-gateway-routed requests.
func GetGatewayPlanFromContext(c *gin.Context) *GatewayPlan {
	if c == nil {
		return nil
	}
	v, ok := common.GetContextKey(c, constant.ContextKeyGatewayPlan)
	if !ok {
		return nil
	}
	plan, ok := v.(*GatewayPlan)
	if !ok {
		return nil
	}
	return plan
}

// ShouldRetryWithGatewayConfig checks whether a status code should be retried
// according to the gateway's retryableStatusCodes / nonRetryableStatusCodes
// lists. Returns (retryable, decided):
//
//	decided=true  — the code was found in one of the lists; retryable holds the answer.
//	decided=false — the code is in neither list; caller should fall through to core logic.
func ShouldRetryWithGatewayConfig(plan *GatewayPlan, code int) (retryable bool, decided bool) {
	if plan == nil || plan.Retry == nil || plan.Retry.Model == nil {
		return false, false
	}
	// nonRetryable takes precedence (more specific).
	for _, c := range plan.Retry.Model.NonRetryableStatusCodes {
		if c == code {
			return false, true
		}
	}
	for _, c := range plan.Retry.Model.RetryableStatusCodes {
		if c == code {
			return true, true
		}
	}
	return false, false
}

// GetMaxRetries returns the per-model max-retries from the gateway plan, or 0
// when unset (caller should fall back to common.RetryTimes).
func GetMaxRetries(plan *GatewayPlan) int {
	if plan == nil || plan.Retry == nil || plan.Retry.Model == nil {
		return 0
	}
	return plan.Retry.Model.MaxRetries
}

// GetBackoffInterval returns the backoff duration for the given attempt index
// (0-based). Returns 0 when no backoff is configured.
func GetBackoffInterval(plan *GatewayPlan, attempt int) time.Duration {
	if plan == nil || plan.Retry == nil || plan.Retry.Model == nil {
		return 0
	}
	intervals := plan.Retry.Model.BackoffIntervals
	if len(intervals) == 0 {
		return 0
	}
	idx := attempt
	if idx >= len(intervals) {
		idx = len(intervals) - 1
	}
	if idx < 0 {
		return 0
	}
	return time.Duration(intervals[idx]) * time.Second
}

// ShouldRetryByKey reports whether key-level retry is enabled in the gateway
// plan. Returns true when no plan is set (backward-compatible with existing
// relay behaviour).
func ShouldRetryByKey(plan *GatewayPlan) bool {
	if plan == nil || plan.Retry == nil || plan.Retry.Model == nil {
		return true
	}
	return plan.Retry.Model.RetryByKey
}

// GetProviderSwitchDelay returns the configured delay to apply before
// switching to a different provider/channel. Returns 0 when unset.
func GetProviderSwitchDelay(plan *GatewayPlan) time.Duration {
	if plan == nil || plan.Retry == nil || plan.Retry.Provider == nil {
		return 0
	}
	if plan.Retry.Provider.SwitchDelay > 0 {
		return time.Duration(plan.Retry.Provider.SwitchDelay) * time.Second
	}
	return 0
}

// RecordProviderFailureForChannel maps a channel ID to its provider ID via
// the gateway plan's candidate list and records a provider-level failure.
// No-op when no plan exists or the channel is not in the plan.
func RecordProviderFailureForChannel(c *gin.Context, channelId int) {
	plan := GetGatewayPlanFromContext(c)
	if plan == nil {
		return
	}
	for _, cand := range plan.Candidates {
		if cand != nil && cand.ChannelId == channelId {
			RecordProviderFailure(cand.ProviderId)
			return
		}
	}
}

// ForceProviderCooldownForChannel maps a channel ID to its provider ID via
// the gateway plan's candidate list and places the provider into cooldown
// immediately (bypassing the failure threshold). No-op when no plan exists
// or the channel is not in the plan.
func ForceProviderCooldownForChannel(c *gin.Context, channelId int) {
	plan := GetGatewayPlanFromContext(c)
	if plan == nil {
		return
	}
	for _, cand := range plan.Candidates {
		if cand != nil && cand.ChannelId == channelId {
			ForceProviderCooldown(cand.ProviderId)
			return
		}
	}
}

// IsTierRequest reports whether the request targeted a configured tier
// virtual model (auto_low/auto_mid/auto_high/auto_max) — i.e. the plan was
// built with TierRequest set. The relay retry loop uses this to enable
// cross-model failover for tier requests only; every other request keeps
// its pre-existing retry semantics.
func IsTierRequest(plan *GatewayPlan) bool {
	return plan != nil && plan.TierRequest != ""
}

// NextTierCandidate returns the plan's first tier candidate whose channel is
// not the one that just failed and is not in provider cooldown. Candidates
// whose channel cannot be resolved (disabled or missing synced channel) are
// skipped. Daily-tier circuit-broken providers are already absent from a
// freshly-built plan; a candidate that only now hit its tier limit is still
// offered — the request already started under that tier and the daily-tier
// break is a scheduling, not a runtime, concern. The returned candidate is
// the failover target for the retry loop; the loop rewrites the body model
// and re-locks the channel. ok=false means the tier is exhausted and the
// caller must fall back to the pre-existing same-model retry path (or give
// up).
func NextTierCandidate(c *gin.Context, plan *GatewayPlan, failedChannelId int) (*Candidate, bool) {
	if plan == nil || !IsTierRequest(plan) {
		return nil, false
	}
	for _, cand := range plan.Candidates {
		if cand == nil || cand.ChannelId <= 0 {
			continue
		}
		if cand.ChannelId == failedChannelId {
			continue
		}
		if IsProviderInCooldown(cand.ProviderId) {
			continue
		}
		return cand, true
	}
	return nil, false
}
