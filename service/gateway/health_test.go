package gateway

import (
	"sync"
	"testing"
	"time"
)

func resetHealthState() {
	providerHealthMap = sync.Map{}
	nowFunc = time.Now
	SetConfig(nil)
}

func withFakeNow(t *testing.T, start time.Time) func(time.Duration) {
	t.Helper()
	cur := start
	nowFunc = func() time.Time { return cur }
	t.Cleanup(func() { nowFunc = time.Now })
	return func(d time.Duration) { cur = cur.Add(d) }
}

func TestRecordProviderFailure_IncrementsCount(t *testing.T) {
	resetHealthState()
	advance := withFakeNow(t, time.Unix(1_700_000_000, 0))
	_ = advance

	RecordProviderFailure("zen")
	RecordProviderFailure("zen")

	h := GetProviderHealth("zen")
	if h == nil {
		t.Fatal("expected health record")
	}
	if h.FailureCount != 2 {
		t.Fatalf("FailureCount = %d, want 2", h.FailureCount)
	}
	if !h.CooldownUntil.IsZero() {
		t.Fatalf("cooldown should not yet be set, got %v", h.CooldownUntil)
	}
}

func TestRecordProviderFailure_TriggersCooldownAtThreshold(t *testing.T) {
	resetHealthState()
	withFakeNow(t, time.Unix(1_700_000_000, 0))

	for i := 0; i < 3; i++ {
		RecordProviderFailure("zen")
	}

	if !IsProviderInCooldown("zen") {
		t.Fatalf("provider should be in cooldown after 3 failures")
	}
	cooldowns := GetCooldownProviders()
	found := false
	for _, p := range cooldowns {
		if p == "zen" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("zen should be in GetCooldownProviders, got %v", cooldowns)
	}
}

func TestIsProviderInCooldown_FalseWhenExpired(t *testing.T) {
	resetHealthState()
	advance := withFakeNow(t, time.Unix(1_700_000_000, 0))

	for i := 0; i < 3; i++ {
		RecordProviderFailure("zen")
	}
	if !IsProviderInCooldown("zen") {
		t.Fatal("expected cooldown active")
	}

	advance(301 * time.Second)

	if IsProviderInCooldown("zen") {
		t.Fatal("cooldown should expire after 300s default")
	}
}

func TestResetProviderHealth_ClearsState(t *testing.T) {
	resetHealthState()
	withFakeNow(t, time.Unix(1_700_000_000, 0))

	for i := 0; i < 3; i++ {
		RecordProviderFailure("zen")
	}
	if !IsProviderInCooldown("zen") {
		t.Fatal("expected cooldown active before reset")
	}

	ResetProviderHealth("zen")

	if IsProviderInCooldown("zen") {
		t.Fatal("cooldown should be cleared after reset")
	}
	if GetProviderHealth("zen") != nil {
		t.Fatal("health entry should be removed after reset")
	}
}

func TestRecordProviderFailure_WindowExpiryResetsCount(t *testing.T) {
	resetHealthState()
	advance := withFakeNow(t, time.Unix(1_700_000_000, 0))

	RecordProviderFailure("zen")
	RecordProviderFailure("zen")

	advance(1201 * time.Second)

	RecordProviderFailure("zen")

	h := GetProviderHealth("zen")
	if h == nil {
		t.Fatal("expected health record")
	}
	if h.FailureCount != 1 {
		t.Fatalf("FailureCount = %d, want 1 after window reset", h.FailureCount)
	}
	if IsProviderInCooldown("zen") {
		t.Fatal("should not be in cooldown after window reset and a single failure")
	}
}

func TestHealthDefaults_UsesConfigWhenAvailable(t *testing.T) {
	resetHealthState()
	withFakeNow(t, time.Unix(1_700_000_000, 0))

	cfg := &GatewayYaml{
		LLMGateway: map[string]*StrategyGroup{
			"default": {
				LLMCommon: &LLMCommon{
					ProviderRetry: &ProviderRetry{
						Provider: &ProviderSwitch{
							FailureThreshold: 2,
							FailureWindow:    600,
							CooldownPeriod:   60,
						},
					},
				},
			},
		},
	}
	SetConfig(cfg)
	t.Cleanup(func() { SetConfig(nil) })

	RecordProviderFailure("zen")
	if IsProviderInCooldown("zen") {
		t.Fatal("should not yet be cooling")
	}
	RecordProviderFailure("zen")
	if !IsProviderInCooldown("zen") {
		t.Fatal("threshold=2 should trigger cooldown after 2 failures")
	}
}

func TestEmptyProviderId_NoOps(t *testing.T) {
	resetHealthState()
	RecordProviderFailure("")
	if got := GetProviderHealth(""); got != nil {
		t.Fatalf("empty provider should not record health, got %+v", got)
	}
	if IsProviderInCooldown("") {
		t.Fatal("empty provider should not be in cooldown")
	}
	ResetProviderHealth("")
}
