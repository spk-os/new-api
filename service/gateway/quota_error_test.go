package gateway

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsQuotaExhaustedError(t *testing.T) {
	quotaMsg := "You have used up your free quota for the current cycle (used 50058395 tokens). Your quota will automatically reset at 2026-10-11 00:00:00. For a higher quota, please complete enterprise verification."
	cases := []struct {
		name       string
		statusCode int
		message    string
		want       bool
	}{
		{"deveco free-cycle exhaustion", http.StatusForbidden, quotaMsg, true},
		{"case-insensitive match", http.StatusForbidden, "QUOTA exhausted, will RESET tomorrow", true},
		{"403 without quota/reset keywords keeps retrying", http.StatusForbidden, "Full inference timed out", false},
		{"403 quota without reset", http.StatusForbidden, "insufficient quota balance", false},
		{"workbuddy 402 hard_credit matches regardless of message", http.StatusPaymentRequired, "credit not enough", true},
		{"workbuddy 402 empty message", http.StatusPaymentRequired, "", true},
		{"429 insufficient_quota does not match", http.StatusTooManyRequests, "You exceeded your current quota, please check your plan and billing details", false},
		{"401 auth failure does not match", http.StatusUnauthorized, "quota will reset soon", false},
		{"500 with keywords does not match", http.StatusInternalServerError, "quota reset failed", false},
		{"empty message", http.StatusForbidden, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsQuotaExhaustedError(tc.statusCode, tc.message))
		})
	}
}

func quotaErrAt(code int, message string) *dto.OpenAIErrorWithStatusCode {
	return &dto.OpenAIErrorWithStatusCode{
		StatusCode: code,
		Error:      types.OpenAIError{Message: message, Type: "upstream_error"},
	}
}

func quotaRetryPlan(cands ...*Candidate) *GatewayPlan {
	plan := newPlan(cands...)
	// Mirror the production gateway config where 403 is retryable: the
	// keyword check must be what stops the retry, not the status-code list.
	plan.Retry.Model.NonRetryableStatusCodes = []int{401}
	plan.Retry.Model.RetryableStatusCodes = []int{403, 500, 503}
	return plan
}

func TestExecuteWithPlan_QuotaExhaustedNoRetryAndImmediateCooldown(t *testing.T) {
	resetExecutorState()
	c := newTestCtx()
	cand := &Candidate{
		ProviderId:  "deveco_3633",
		ChannelId:   24,
		Keys:        []string{"k1", "k2"},
		ActualModel: "hw_glm-5.3",
	}
	plan := quotaRetryPlan(cand)
	plan.Retry.Model.MaxRetries = 5

	calls := 0
	_, err := ExecuteWithPlan(c, plan, func(_ *Candidate) (*http.Response, *dto.OpenAIErrorWithStatusCode) {
		calls++
		return nil, quotaErrAt(403, "You have used up your free quota for the current cycle (used 50058395 tokens). Your quota will automatically reset at 2026-10-11 00:00:00.")
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAllCandidatesFailed)
	// One attempt only: no per-key retries, no retryByKey pass.
	assert.Equal(t, 1, calls)
	// Provider enters cooldown immediately, bypassing the failure threshold.
	assert.True(t, IsProviderInCooldown("deveco_3633"))
}

func TestExecuteWithPlan_QuotaExhaustedFailsOverToNextCandidate(t *testing.T) {
	resetExecutorState()
	c := newTestCtx()
	exhausted := &Candidate{ProviderId: "deveco_3633", ChannelId: 24, Keys: []string{"k1"}, ActualModel: "hw_glm-5.3"}
	healthy := &Candidate{ProviderId: "deveco_7306", ChannelId: 23, Keys: []string{"k1"}, ActualModel: "hw_glm-5.3"}
	plan := quotaRetryPlan(exhausted, healthy)
	plan.Retry.Model.MaxRetries = 5

	resp, err := ExecuteWithPlan(c, plan, func(cand *Candidate) (*http.Response, *dto.OpenAIErrorWithStatusCode) {
		if cand.ProviderId == "deveco_3633" {
			return nil, quotaErrAt(403, "Your quota will automatically reset at 2026-10-11 00:00:00; free quota used up.")
		}
		return &http.Response{StatusCode: 200}, nil
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, 200, resp.StatusCode)
	assert.True(t, IsProviderInCooldown("deveco_3633"))
	assert.False(t, IsProviderInCooldown("deveco_7306"))
}

func TestExecuteWithPlan_OrdinaryForbiddenStillRetries(t *testing.T) {
	resetExecutorState()
	c := newTestCtx()
	cand := &Candidate{ProviderId: "deveco_3633", ChannelId: 24, Keys: []string{"k1"}, ActualModel: "hw_glm-5.3"}
	plan := quotaRetryPlan(cand)
	plan.Retry.Model.MaxRetries = 2

	calls := 0
	_, _ = ExecuteWithPlan(c, plan, func(_ *Candidate) (*http.Response, *dto.OpenAIErrorWithStatusCode) {
		calls++
		return nil, quotaErrAt(403, "Full inference timed out")
	})
	// Without quota/reset keywords the 403 consumes all retries as before.
	assert.Equal(t, 3, calls)
	assert.False(t, IsProviderInCooldown("deveco_3633"))
}

func TestForceProviderCooldown_BypassesFailureThreshold(t *testing.T) {
	resetExecutorState()
	assert.False(t, IsProviderInCooldown("deveco_6506"))

	ForceProviderCooldown("deveco_6506")
	assert.True(t, IsProviderInCooldown("deveco_6506"))

	h := GetProviderHealth("deveco_6506")
	require.NotNil(t, h)
	assert.Zero(t, h.FailureCount)
	assert.True(t, h.CooldownUntil.After(nowFunc()))

	ResetProviderHealth("deveco_6506")
	assert.False(t, IsProviderInCooldown("deveco_6506"))
}
