package gateway

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/dto"
	"github.com/gin-gonic/gin"
)

// ContextKeyGatewayResponseWritten marks that an upstream response has begun
// being streamed to the client; further retries are unsafe.
const ContextKeyGatewayResponseWritten = "gateway_response_written"

const (
	defaultMaxTotalRetries = 30
	defaultMaxTotalTimeout = 1200
)

var (
	ErrAffinityNoFailover     = errors.New("gateway: affinity bound provider failed and failover disabled")
	ErrAllCandidatesFailed    = errors.New("gateway: all candidates failed")
	ErrRetriesExhausted       = errors.New("gateway: retries exhausted")
	ErrResponseAlreadyWritten = errors.New("gateway: response already written, cannot retry")
)

var sleepFunc = time.Sleep

// ExecuteWithPlan walks plan.Candidates with retry/key-switch/cooldown rules
// and returns the first successful response. doOnce performs a single upstream
// attempt for the supplied candidate.
func ExecuteWithPlan(c *gin.Context, plan *GatewayPlan, doOnce func(cand *Candidate) (*http.Response, *dto.OpenAIErrorWithStatusCode)) (*http.Response, error) {
	if plan == nil || len(plan.Candidates) == 0 {
		return nil, ErrAllCandidatesFailed
	}

	maxTotal := defaultMaxTotalRetries
	maxTimeout := defaultMaxTotalTimeout
	if plan.Retry != nil && plan.Retry.Global != nil {
		if plan.Retry.Global.MaxTotalRetries > 0 {
			maxTotal = plan.Retry.Global.MaxTotalRetries
		}
		if plan.Retry.Global.MaxTotalTimeout > 0 {
			maxTimeout = plan.Retry.Global.MaxTotalTimeout
		}
	}
	deadline := nowFunc().Add(time.Duration(maxTimeout) * time.Second)

	totalAttempts := 0
	candidates := plan.Candidates

	for ci := 0; ci < len(candidates); ci++ {
		cand := candidates[ci]
		if cand == nil {
			continue
		}
		if IsProviderInCooldown(cand.ProviderId) {
			continue
		}

		keys := cand.Keys
		if len(keys) == 0 {
			keys = []string{""}
		}
		retryByKey := plan.Retry != nil && plan.Retry.Model != nil && plan.Retry.Model.RetryByKey
		maxRetries := 0
		if plan.Retry != nil && plan.Retry.Model != nil {
			maxRetries = plan.Retry.Model.MaxRetries
		}

		candidateSucceeded := false
		var lastOpenAIErr *dto.OpenAIErrorWithStatusCode

		for ki := 0; ki < len(keys); ki++ {
			cand.KeyIndex = ki

			for r := 0; r <= maxRetries; r++ {
				if v, ok := c.Get(ContextKeyGatewayResponseWritten); ok {
					if b, _ := v.(bool); b {
						return nil, ErrResponseAlreadyWritten
					}
				}
				if totalAttempts >= maxTotal {
					return nil, ErrRetriesExhausted
				}
				if !nowFunc().Before(deadline) {
					return nil, ErrRetriesExhausted
				}

				totalAttempts++
				resp, openaiErr := doOnce(cand)
				if openaiErr == nil && resp != nil {
					onSuccess(cand)
					injectGatewayHeaders(c, cand, plan)
					return resp, nil
				}
				lastOpenAIErr = openaiErr

				code := 0
				if openaiErr != nil {
					code = openaiErr.StatusCode
				}

				if isNonRetryable(plan, code) {
					break
				}
				if !isRetryable(plan, code) {
					break
				}

				if r < maxRetries {
					backoff := backoffFor(plan, r)
					if backoff > 0 {
						sleepFunc(backoff)
					}
					continue
				}
			}

			if candidateSucceeded {
				break
			}
			if !retryByKey {
				break
			}
		}
		_ = lastOpenAIErr

		if candidateSucceeded {
			return nil, nil
		}

		RecordProviderFailure(cand.ProviderId)

		if plan.AffinityHit && ci == 0 {
			if plan.Affinity == nil || plan.Affinity.Failover == nil || !plan.Affinity.Failover.Enable {
				return nil, ErrAffinityNoFailover
			}
			markAffinityBroken(c, plan, cand)
			reorderByFailoverPriority(plan, cand)
			candidates = plan.Candidates
			ci = -1
			continue
		}

		if plan.Retry != nil && plan.Retry.Provider != nil && plan.Retry.Provider.SwitchDelay > 0 {
			sleepFunc(time.Duration(plan.Retry.Provider.SwitchDelay) * time.Second)
		}
	}

	return nil, ErrAllCandidatesFailed
}

func isRetryable(plan *GatewayPlan, code int) bool {
	if plan == nil || plan.Retry == nil || plan.Retry.Model == nil {
		return false
	}
	if code == 0 {
		return plan.Retry.Model.RetryOnTimeout
	}
	for _, c := range plan.Retry.Model.RetryableStatusCodes {
		if c == code {
			return true
		}
	}
	return false
}

func isNonRetryable(plan *GatewayPlan, code int) bool {
	if plan == nil || plan.Retry == nil || plan.Retry.Model == nil {
		return false
	}
	for _, c := range plan.Retry.Model.NonRetryableStatusCodes {
		if c == code {
			return true
		}
	}
	return false
}

func backoffFor(plan *GatewayPlan, r int) time.Duration {
	if plan == nil || plan.Retry == nil || plan.Retry.Model == nil {
		return 0
	}
	intervals := plan.Retry.Model.BackoffIntervals
	if len(intervals) == 0 {
		return 0
	}
	idx := r
	if idx >= len(intervals) {
		idx = len(intervals) - 1
	}
	return time.Duration(intervals[idx]) * time.Second
}

func onSuccess(cand *Candidate) {
	if cand == nil {
		return
	}
	ResetProviderHealth(cand.ProviderId)
}

func markAffinityBroken(c *gin.Context, plan *GatewayPlan, failed *Candidate) {
	if c == nil {
		return
	}
	c.Set("gateway_affinity_broken", true)
	if plan != nil && plan.Affinity != nil && plan.Affinity.Failover != nil && plan.Affinity.Failover.NotifyClient {
		header := plan.Affinity.Failover.NotifyHeader
		if header == "" {
			header = "X-Gateway-Affinity-Broken"
		}
		c.Header(header, "1")
	}
}

// reorderByFailoverPriority removes the failed candidate and reorders the
// remainder placing higher-priority matches first per AffinityFailover.Priority.
func reorderByFailoverPriority(plan *GatewayPlan, failed *Candidate) {
	if plan == nil || failed == nil {
		return
	}
	rest := make([]*Candidate, 0, len(plan.Candidates))
	for _, c := range plan.Candidates {
		if c == failed {
			continue
		}
		rest = append(rest, c)
	}
	priority := ""
	if plan.Affinity != nil && plan.Affinity.Failover != nil {
		priority = plan.Affinity.Failover.Priority
	}

	score := func(c *Candidate) int {
		if c == nil {
			return 99
		}
		switch priority {
		case "sameModel":
			if c.ActualModel == failed.ActualModel {
				return 0
			}
			if modelSeriesPrefix(c.ActualModel) == modelSeriesPrefix(failed.ActualModel) && failed.ActualModel != "" {
				return 1
			}
			return 2
		case "sameProvider":
			if c.ProviderId == failed.ProviderId {
				return 0
			}
			return 1
		default:
			return 0
		}
	}

	stableSort(rest, func(a, b *Candidate) bool {
		return score(a) < score(b)
	})
	plan.Candidates = rest
}

func stableSort(s []*Candidate, less func(a, b *Candidate) bool) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && less(s[j], s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// modelSeriesPrefix extracts a series key from a model name using the first
// run of letters/digits up to a separator (-, _) or the first non-letter.
func modelSeriesPrefix(model string) string {
	if model == "" {
		return ""
	}
	i := 0
	for i < len(model) {
		ch := model[i]
		if ch == '-' || ch == '_' || ch == '.' {
			break
		}
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			i++
			continue
		}
		break
	}
	if i == 0 {
		return model
	}
	return model[:i]
}

// CalcRequestCost computes request cost using model-specific pricing or the
// configured default.
func CalcRequestCost(model string, usage *dto.Usage) float64 {
	if usage == nil {
		return 0
	}
	cfg := GetConfig()
	if cfg == nil || cfg.Cost == nil {
		return 0
	}
	mc := cfg.Cost.Default
	if cfg.Cost.Models != nil {
		if m, ok := cfg.Cost.Models[model]; ok && m != nil {
			mc = m
		}
	}
	if mc == nil {
		return 0
	}
	in := float64(usage.PromptTokens) / 1000.0 * mc.InputPer1kTokens
	out := float64(usage.CompletionTokens) / 1000.0 * mc.OutputPer1kTokens
	return in + out
}

func injectGatewayHeaders(c *gin.Context, cand *Candidate, plan *GatewayPlan) {
	if c == nil || cand == nil {
		return
	}
	c.Header("X-Gateway-Provider", cand.ProviderId)
	c.Header("X-Gateway-Model", cand.ActualModel)
	if plan != nil && plan.AffinityHit {
		c.Header("X-Gateway-Affinity", "hit")
	}
	_ = strings.TrimSpace
}
