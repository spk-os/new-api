package gateway

// Daily token-tier scheduling and circuit breaking.
//
// Operators configure a daily token usage ladder (daily_token_tiers globally,
// dailyTokenTiers per provider, in W = 10k tokens) per local day
// (00:00~23:59). Candidates are re-ordered by the tier their channel's
// today-usage has reached (ascending; tier scheduling outranks the static
// order) and providers that reached the highest tier are circuit-broken: when
// every candidate of a request is broken the middleware answers 403
// GATEWAY_DAILY_TIER_CIRCUIT_BROKEN instead of routing.
//
// Usage accounting: an in-memory per-channel counter (keyed by channel id,
// because quota_data rows carry only channel_id) seeded once per day from
// quota_data (token_used = prompt+completion+cache) and incremented from the
// AfterRecordConsumeLogHooks chain. quota_data is flushed every
// DataExportInterval minutes, so the seed baseline lags by at most that
// interval; on top of it increments are exact for this process. In the HA
// blue/green deployment each node sees the shared DB baseline plus its own
// increments, so tier boundaries across nodes may drift by a few minutes of
// traffic — accepted by design. LogConsumeEnabled must stay on, otherwise no
// increments are recorded and tiers never advance.

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

const (
	// dailyTierWUnit converts yaml W values into tokens (1 W = 10000 tokens).
	dailyTierWUnit = 10000

	// dailyTierCircuitBrokenCode is the error code returned when every
	// candidate of a request reached the provider's highest daily tier.
	dailyTierCircuitBrokenCode = "GATEWAY_DAILY_TIER_CIRCUIT_BROKEN"

	// dailyTierChanIdTTL bounds the provider-name → channel-id cache.
	dailyTierChanIdTTL = 60 * time.Second
)

// dailyTierNowFunc is a replaceable clock so tests can drive day switches.
var dailyTierNowFunc = time.Now

// dailyTierUsage is the per-channel in-memory daily counter. seeded=false
// forces a quota_data re-seed on the next read (process start or day switch).
type dailyTierUsage struct {
	mu     sync.Mutex
	dayKey string
	used   int64
	seeded bool
}

var dailyTierUsageMap sync.Map // channelId(int) -> *dailyTierUsage

type dailyTierChanIdEntry struct {
	id     int
	expire time.Time
}

var dailyTierChanIdCache sync.Map // providerName(string) -> dailyTierChanIdEntry

func init() {
	model.RegisterAfterRecordConsumeLogHook(recordDailyTierUsageFromLog)
}

// recordDailyTierUsageFromLog adapts the post-consume-log hook to the daily
// counter. Token accounting matches the quota_data TokenUsed composition
// (model.RecordConsumeLog): prompt + completion + cache.
func recordDailyTierUsageFromLog(c *gin.Context, userId int, createdAt int64, params model.RecordConsumeLogParams) {
	if params.ChannelId <= 0 {
		return
	}
	RecordChannelDailyUsage(params.ChannelId, int64(params.PromptTokens+params.CompletionTokens+params.CacheTokens))
}

// RecordChannelDailyUsage adds tokens to the in-memory per-channel counter.
// Pure memory: quota_data persistence is already handled by RecordConsumeLog.
func RecordChannelDailyUsage(channelId int, tokens int64) {
	if channelId <= 0 || tokens <= 0 {
		return
	}
	dayKey := dailyTierNowFunc().Format("2006-01-02")
	v, _ := dailyTierUsageMap.LoadOrStore(channelId, &dailyTierUsage{})
	u := v.(*dailyTierUsage)
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.dayKey != dayKey {
		// Day switched since the counter was last touched: drop the stale
		// value and force a re-seed from quota_data on the next read (the
		// flusher will carry this request's tokens there anyway).
		u.used = 0
		u.dayKey = dayKey
		u.seeded = false
	}
	u.used += tokens
}

// channelDailyUsedTokens returns the channel's today token usage, seeding the
// counter from quota_data on first touch of each local day.
func channelDailyUsedTokens(channelId int) int64 {
	if channelId <= 0 {
		return 0
	}
	now := dailyTierNowFunc()
	dayKey := now.Format("2006-01-02")
	v, _ := dailyTierUsageMap.LoadOrStore(channelId, &dailyTierUsage{})
	u := v.(*dailyTierUsage)
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.seeded || u.dayKey != dayKey {
		// First read of the day: seed from quota_data. Increments recorded
		// before this first read (no prior seed this day) are preserved on
		// top of the baseline; they cannot yet exist in quota_data because
		// every producing request must have been scheduled — i.e. read —
		// first. A stale entry from a previous day contributes nothing.
		pending := int64(0)
		if !u.seeded && u.dayKey == dayKey && u.used > 0 {
			pending = u.used
		}
		u.used = seedChannelDailyUsedTokens(channelId, now) + pending
		u.dayKey = dayKey
		u.seeded = true
	}
	return u.used
}

// seedChannelDailyUsedTokens sums today's quota_data token_used rows for the
// channel as the day baseline. With DataExportEnabled off no rows are ever
// written, so the baseline is 0 (in-memory increments still work).
func seedChannelDailyUsedTokens(channelId int, now time.Time) int64 {
	if !common.DataExportEnabled || model.DB == nil {
		return 0
	}
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	var total int64
	err := model.DB.Table("quota_data").
		Select("COALESCE(SUM(token_used), 0)").
		Where("channel_id = ? AND created_at >= ?", channelId, dayStart).
		Scan(&total).Error
	if err != nil {
		common.SysError(fmt.Sprintf("daily tiers: seed usage for channel %d failed: %s", channelId, err.Error()))
		return 0
	}
	if total < 0 {
		total = 0
	}
	return total
}

// effectiveTierTokens resolves the token-unit tier ladder for a providerId:
// a provider-level dailyTokenTiers replaces the global list entirely (empty
// non-nil list = explicitly disabled); nil inherits the global
// daily_token_tiers. Values are converted from W to tokens, positive-filtered,
// sorted ascending and deduped. nil = feature off for this provider.
func effectiveTierTokens(providerId string) []float64 {
	cfg := GetConfig()
	if cfg == nil {
		return nil
	}
	var tiers []float64
	if p, ok := cfg.Providers[providerId]; ok && p != nil && p.DailyTokenTiers != nil {
		tiers = p.DailyTokenTiers
	} else {
		tiers = cfg.DailyTokenTiers
	}
	return normalizeTierTokens(tiers)
}

// normalizeTierTokens converts W → tokens, drops non-positive entries, sorts
// ascending and dedups. Returns nil for an empty result (feature off).
func normalizeTierTokens(tiers []float64) []float64 {
	if len(tiers) == 0 {
		return nil
	}
	toks := make([]float64, 0, len(tiers))
	for _, t := range tiers {
		if t > 0 {
			toks = append(toks, t*dailyTierWUnit)
		}
	}
	if len(toks) == 0 {
		return nil
	}
	sort.Float64s(toks)
	dedup := toks[:1]
	for _, v := range toks[1:] {
		if v != dedup[len(dedup)-1] {
			dedup = append(dedup, v)
		}
	}
	return dedup
}

// tierOfUsage counts how many tiers the usage has reached: tier = #{Ti :
// usage >= Ti}. tierTokens must be ascending. Usage == Ti counts as reached.
func tierOfUsage(used int64, tierTokens []float64) int {
	tier := 0
	for _, t := range tierTokens {
		if float64(used) >= t {
			tier++
		} else {
			break
		}
	}
	return tier
}

// providerChannelId resolves the synced channel id for a provider (channel
// name == provider.Name, enforced by channel sync). Cached 60s including
// negative results so the scheduling hot path stays off the database. Channel
// status is intentionally not filtered — usage follows the channel even while
// it is disabled.
func providerChannelId(providerName string) int {
	if providerName == "" || model.DB == nil {
		return 0
	}
	now := dailyTierNowFunc()
	if v, ok := dailyTierChanIdCache.Load(providerName); ok {
		if e, ok := v.(*dailyTierChanIdEntry); ok && now.Before(e.expire) {
			return e.id
		}
	}
	id := 0
	var ch model.Channel
	if err := model.DB.Select("id").Where("name = ?", providerName).First(&ch).Error; err == nil {
		id = ch.Id
	}
	dailyTierChanIdCache.Store(providerName, &dailyTierChanIdEntry{id: id, expire: now.Add(dailyTierChanIdTTL)})
	return id
}

// dailyTierState returns the provider's ladder and today's usage tier.
// enabled=false when the feature is off for this provider (no tier config).
func dailyTierState(providerId string) (tierTokens []float64, tier int, enabled bool) {
	tierTokens = effectiveTierTokens(providerId)
	if len(tierTokens) == 0 {
		return nil, 0, false
	}
	cfg := GetConfig()
	if cfg == nil {
		return nil, 0, false
	}
	p := cfg.Providers[providerId]
	if p == nil {
		return nil, 0, false
	}
	used := channelDailyUsedTokens(providerChannelId(p.Name))
	return tierTokens, tierOfUsage(used, tierTokens), true
}

// ApplyDailyTierScheduling reorders candidates by today's usage tier
// (ascending, stable — candidates inside one tier keep the caller's
// costFirst/order sequence) and drops circuit-broken candidates (tier
// reached the highest configured tier). Returns the filtered list and
// allBroken=true when the input was non-empty but every candidate is broken.
// Sets Candidate.DailyTier for downstream consumers (session balancing).
func ApplyDailyTierScheduling(cands []*Candidate) ([]*Candidate, bool) {
	if len(cands) == 0 {
		return cands, false
	}
	type tierState struct {
		tokens []float64
		tier   int
		on     bool
	}
	states := make(map[string]*tierState, len(cands))
	configured := false
	for _, cand := range cands {
		if cand == nil {
			continue
		}
		if _, ok := states[cand.ProviderId]; ok {
			continue
		}
		tt, tier, enabled := dailyTierState(cand.ProviderId)
		st := &tierState{tokens: tt, tier: tier, on: enabled}
		states[cand.ProviderId] = st
		if enabled {
			configured = true
			cand.DailyTier = tier
		} else {
			cand.DailyTier = 0
		}
	}
	if !configured {
		return cands, false
	}

	sorted := make([]*Candidate, len(cands))
	copy(sorted, cands)
	sort.SliceStable(sorted, func(i, j int) bool {
		return states[sorted[i].ProviderId].tier < states[sorted[j].ProviderId].tier
	})
	out := make([]*Candidate, 0, len(sorted))
	allBroken := true
	for _, cand := range sorted {
		st := states[cand.ProviderId]
		if st.on && st.tier >= len(st.tokens) {
			continue // reached the highest tier — circuit broken
		}
		allBroken = false
		out = append(out, cand)
	}
	if allBroken {
		return nil, true
	}
	return out, false
}

// shouldBreakAffinityByTier is the pure decision core of the affinity
// daily-tier break: a circuit-broken sticky provider must always be dropped
// (otherwise the sticky plan would route into an exhausted provider or 403
// while alternatives exist); a healthy sticky provider is dropped only when
// it trails the best reachable tier by more than one (session keep-alive
// tolerates a one-tier gap).
func shouldBreakAffinityByTier(stickyBroken bool, stickyTier, bestTier int) bool {
	if stickyBroken {
		return true
	}
	return stickyTier > bestTier+1
}

// shouldBreakAffinityByDailyTier evaluates the break rule for a sticky-bound
// candidate. bestReachableDailyTier runs on the full (cooldown-excluded)
// candidate list; when no non-broken alternative exists the binding is kept —
// the sticky provider itself is not exhausted and a broken binding would not
// change the outcome (the request would 403 either way only when EVERY
// provider including the sticky one is exhausted, which the first branch
// already covers).
func shouldBreakAffinityByDailyTier(cand *Candidate, prof *EffectiveProfile, requestModel string, opts *GatewayOptions) bool {
	if cand == nil {
		return false
	}
	tt, stickyTier, enabled := dailyTierState(cand.ProviderId)
	if !enabled {
		return false
	}
	if stickyTier >= len(tt) {
		return true
	}
	best := -1
	for _, alt := range BuildCandidates(prof, requestModel, opts) {
		att, altTier, altEnabled := dailyTierState(alt.ProviderId)
		if !altEnabled {
			best = 0 // an unconfigured provider is permanently tier 0
			break
		}
		if altTier < len(att) && (best < 0 || altTier < best) {
			best = altTier
		}
	}
	if best < 0 {
		return false
	}
	return shouldBreakAffinityByTier(false, stickyTier, best)
}

// abortWithGatewayDailyTierBroken answers 403 when every candidate reached
// its highest daily tier. Mirrors middleware.abortWithOpenAiMessage (gateway
// cannot import middleware — import cycle); middleware-layer Abort means the
// request never reaches relay, so no retry logic applies.
func abortWithGatewayDailyTierBroken(c *gin.Context, requestModel string) {
	msg := fmt.Sprintf("daily token limit reached: every routed provider for model %q has hit its highest daily_token_tiers tier; the counter resets at local midnight (00:00)", requestModel)
	c.JSON(http.StatusForbidden, gin.H{
		"error": gin.H{
			"message": common.MessageWithRequestId(msg, c.GetString(common.RequestIdKey)),
			"type":    "new_api_error",
			"code":    dailyTierCircuitBrokenCode,
		},
	})
	c.Abort()
	logger.LogError(c.Request.Context(), msg)
}
