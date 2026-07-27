package gateway

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetGatewayPlanFromContext(t *testing.T) {
	c := &gin.Context{}
	// No plan set → nil
	assert.Nil(t, GetGatewayPlanFromContext(c))

	// Plan set → returned
	plan := &GatewayPlan{Retry: &ProviderRetry{}}
	common.SetContextKey(c, constant.ContextKeyGatewayPlan, plan)
	got := GetGatewayPlanFromContext(c)
	require.NotNil(t, got)
	assert.Same(t, plan, got)

	// Wrong type → nil
	c2 := &gin.Context{}
	common.SetContextKey(c2, constant.ContextKeyGatewayPlan, "not-a-plan")
	assert.Nil(t, GetGatewayPlanFromContext(c2))

	// Nil context → nil
	assert.Nil(t, GetGatewayPlanFromContext(nil))
}

func TestShouldRetryWithGatewayConfig(t *testing.T) {
	plan := &GatewayPlan{
		Retry: &ProviderRetry{
			Model: &ModelRetry{
				RetryableStatusCodes:    []int{500, 503, 504},
				NonRetryableStatusCodes: []int{401, 403, 429},
			},
		},
	}

	tests := []struct {
		name              string
		plan              *GatewayPlan
		code              int
		wantRetryable     bool
		wantDecided       bool
	}{
		{"retryable 500", plan, 500, true, true},
		{"retryable 504", plan, 504, true, true},
		{"nonRetryable 401", plan, 401, false, true},
		{"nonRetryable 429", plan, 429, false, true},
		{"undecided 502", plan, 502, false, false},
		{"undecided 200", plan, 200, false, false},
		{"nil plan", nil, 500, false, false},
		{"nil retry", &GatewayPlan{}, 500, false, false},
		{"nil model", &GatewayPlan{Retry: &ProviderRetry{}}, 500, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			retryable, decided := ShouldRetryWithGatewayConfig(tt.plan, tt.code)
			assert.Equal(t, tt.wantRetryable, retryable)
			assert.Equal(t, tt.wantDecided, decided)
		})
	}
}

func TestGetMaxRetries(t *testing.T) {
	tests := []struct {
		name string
		plan *GatewayPlan
		want int
	}{
		{"nil plan", nil, 0},
		{"nil retry", &GatewayPlan{}, 0},
		{"nil model", &GatewayPlan{Retry: &ProviderRetry{}}, 0},
		{"maxRetries 10", &GatewayPlan{Retry: &ProviderRetry{Model: &ModelRetry{MaxRetries: 10}}}, 10},
		{"maxRetries 0", &GatewayPlan{Retry: &ProviderRetry{Model: &ModelRetry{MaxRetries: 0}}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, GetMaxRetries(tt.plan))
		})
	}
}

func TestGetBackoffInterval(t *testing.T) {
	plan := &GatewayPlan{
		Retry: &ProviderRetry{
			Model: &ModelRetry{
				BackoffIntervals: []int{2, 4, 8, 16},
			},
		},
	}

	tests := []struct {
		name    string
		plan    *GatewayPlan
		attempt int
		want    time.Duration
	}{
		{"attempt 0", plan, 0, 2 * time.Second},
		{"attempt 1", plan, 1, 4 * time.Second},
		{"attempt 3", plan, 3, 16 * time.Second},
		{"attempt 5 (clamped)", plan, 5, 16 * time.Second},
		{"nil plan", nil, 0, 0},
		{"nil retry", &GatewayPlan{}, 0, 0},
		{"empty intervals", &GatewayPlan{Retry: &ProviderRetry{Model: &ModelRetry{}}}, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, GetBackoffInterval(tt.plan, tt.attempt))
		})
	}
}

func TestShouldRetryByKey(t *testing.T) {
	tests := []struct {
		name string
		plan *GatewayPlan
		want bool
	}{
		{"nil plan defaults true", nil, true},
		{"nil retry defaults true", &GatewayPlan{}, true},
		{"nil model defaults true", &GatewayPlan{Retry: &ProviderRetry{}}, true},
		{"retryByKey true", &GatewayPlan{Retry: &ProviderRetry{Model: &ModelRetry{RetryByKey: true}}}, true},
		{"retryByKey false", &GatewayPlan{Retry: &ProviderRetry{Model: &ModelRetry{RetryByKey: false}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ShouldRetryByKey(tt.plan))
		})
	}
}

func TestGetProviderSwitchDelay(t *testing.T) {
	tests := []struct {
		name string
		plan *GatewayPlan
		want time.Duration
	}{
		{"nil plan", nil, 0},
		{"nil retry", &GatewayPlan{}, 0},
		{"nil provider", &GatewayPlan{Retry: &ProviderRetry{}}, 0},
		{"switchDelay 10", &GatewayPlan{Retry: &ProviderRetry{Provider: &ProviderSwitch{SwitchDelay: 10}}}, 10 * time.Second},
		{"switchDelay 0", &GatewayPlan{Retry: &ProviderRetry{Provider: &ProviderSwitch{SwitchDelay: 0}}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, GetProviderSwitchDelay(tt.plan))
		})
	}
}

func TestRecordProviderFailureForChannel(t *testing.T) {
	// Reset health state to avoid interference from other tests.
	ResetProviderHealth("test-provider-1")
	ResetProviderHealth("test-provider-2")

	c := &gin.Context{}
	// No plan → no-op, no panic.
	RecordProviderFailureForChannel(c, 999)

	// Plan with candidate channelId=42, providerId="test-provider-1".
	plan := &GatewayPlan{
		Retry: &ProviderRetry{
			Model: &ModelRetry{MaxRetries: 3},
			Provider: &ProviderSwitch{
				FailureThreshold: 1,
				FailureWindow:    600,
				CooldownPeriod:    300,
			},
		},
		Candidates: []*Candidate{
			{ChannelId: 42, ProviderId: "test-provider-1"},
			{ChannelId: 43, ProviderId: "test-provider-2"},
		},
	}
	common.SetContextKey(c, constant.ContextKeyGatewayPlan, plan)

	// Channel 42 → provider "test-provider-1" failure.
	RecordProviderFailureForChannel(c, 42)
	h := GetProviderHealth("test-provider-1")
	require.NotNil(t, h)
	assert.Equal(t, 1, h.FailureCount)

	// Channel 43 → provider "test-provider-2" failure.
	RecordProviderFailureForChannel(c, 43)
	h2 := GetProviderHealth("test-provider-2")
	require.NotNil(t, h2)
	assert.Equal(t, 1, h2.FailureCount)

	// Unknown channel → no-op.
	RecordProviderFailureForChannel(c, 999)
	assert.Nil(t, GetProviderHealth("nonexistent"))

	// Cleanup.
	ResetProviderHealth("test-provider-1")
	ResetProviderHealth("test-provider-2")
}
