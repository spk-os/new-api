package gateway

import (
	"sync"
	"testing"
	"time"
)

func resetLimiterState() {
	rateLimitState = sync.Map{}
	nowFunc = time.Now
	SetConfig(nil)
}

func TestParseWindowRate_ValidFormats(t *testing.T) {
	cases := []struct {
		input    string
		wantDur  time.Duration
		wantReqs int64
	}{
		{"1s-5", time.Second, 5},
		{"10s-100", 10 * time.Second, 100},
		{"1m-10", time.Minute, 10},
		{"30m-50", 30 * time.Minute, 50},
		{"1h-100", time.Hour, 100},
		{"2h-1000", 2 * time.Hour, 1000},
		{"1d-10000", 24 * time.Hour, 10000},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			d, n, err := parseWindowRate(tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d != tc.wantDur {
				t.Errorf("duration = %v, want %v", d, tc.wantDur)
			}
			if n != tc.wantReqs {
				t.Errorf("count = %d, want %d", n, tc.wantReqs)
			}
		})
	}
}

func TestParseWindowRate_InvalidFormats(t *testing.T) {
	bad := []string{"", "1h", "1h-", "-100", "1x-100", "h-100", "1h-abc", "abc-100", "0h-100", "1h-0"}
	for _, s := range bad {
		t.Run(s, func(t *testing.T) {
			if _, _, err := parseWindowRate(s); err == nil {
				t.Fatalf("expected error for %q", s)
			}
		})
	}
}

func TestGetEffectiveRateLimiting_ThreeLevelResolution(t *testing.T) {
	resetLimiterState()

	commonRL := &RateLimiting{Enabled: true, Concurrency: 1}
	providerRL := &RateLimiting{Enabled: true, Concurrency: 2}
	groupRL := &RateLimiting{Enabled: true, Concurrency: 3}

	cfg := &GatewayYaml{
		Providers: map[string]*Provider{
			"zen": {
				RateLimiting: providerRL,
				Models: []*ModelGroup{
					{Name: "free", RateLimiting: groupRL},
					{Name: "paid"},
				},
			},
			"plain": {},
		},
	}
	SetConfig(cfg)
	t.Cleanup(func() { SetConfig(nil) })

	profile := &EffectiveProfile{RateLimiting: commonRL}

	if got := GetEffectiveRateLimiting(profile, "zen", "free"); got != groupRL {
		t.Errorf("model group level: want groupRL, got %+v", got)
	}
	if got := GetEffectiveRateLimiting(profile, "zen", "paid"); got != providerRL {
		t.Errorf("provider level (no model RL): want providerRL, got %+v", got)
	}
	if got := GetEffectiveRateLimiting(profile, "plain", "missing"); got != commonRL {
		t.Errorf("common level: want commonRL, got %+v", got)
	}
	if got := GetEffectiveRateLimiting(nil, "unknown", ""); got != nil {
		t.Errorf("no profile, no provider: want nil, got %+v", got)
	}
}

func TestCheckRateLimit_AllowsWhenDisabled(t *testing.T) {
	resetLimiterState()
	rl := &RateLimiting{Enabled: false, Concurrency: 0, WindowRate: "1h-1"}
	allowed, _ := CheckRateLimit("k", rl)
	if !allowed {
		t.Fatal("disabled rate limit must allow")
	}
}

func TestCheckRateLimit_AllowsBelowLimit(t *testing.T) {
	resetLimiterState()
	rl := &RateLimiting{Enabled: true, Concurrency: 5, WindowRate: "1h-100"}
	allowed, strat := CheckRateLimit("k", rl)
	if !allowed {
		t.Fatalf("expected allowed, strategy=%s", strat)
	}
}

func TestCheckRateLimit_DeniesOverWindowLimit(t *testing.T) {
	resetLimiterState()
	rl := &RateLimiting{Enabled: true, WindowRate: "1h-2", OverLimitStrategy: "reject"}

	for i := 0; i < 2; i++ {
		allowed, _ := CheckRateLimit("kw", rl)
		if !allowed {
			t.Fatalf("attempt %d should be allowed", i)
		}
		IncrementRateLimitCount("kw")
		DecrementConcurrency("kw")
	}

	allowed, strategy := CheckRateLimit("kw", rl)
	if allowed {
		t.Fatal("expected denied")
	}
	if strategy != "reject" {
		t.Fatalf("strategy = %q, want reject", strategy)
	}
}

func TestCheckRateLimit_DeniesOverConcurrencyWithDowngrade(t *testing.T) {
	resetLimiterState()
	rl := &RateLimiting{Enabled: true, Concurrency: 1, OverLimitStrategy: "downgrade"}

	allowed, _ := CheckRateLimit("kc", rl)
	if !allowed {
		t.Fatal("first should be allowed")
	}
	IncrementRateLimitCount("kc")

	allowed, strategy := CheckRateLimit("kc", rl)
	if allowed {
		t.Fatal("second should be denied (concurrency=1)")
	}
	if strategy != "downgrade" {
		t.Fatalf("strategy = %q, want downgrade", strategy)
	}
}

func TestCheckRateLimit_WindowResetsAfterDuration(t *testing.T) {
	resetLimiterState()
	cur := time.Unix(1_700_000_000, 0)
	nowFunc = func() time.Time { return cur }
	t.Cleanup(func() { nowFunc = time.Now })

	rl := &RateLimiting{Enabled: true, WindowRate: "1s-2", OverLimitStrategy: "reject"}

	for i := 0; i < 2; i++ {
		if a, _ := CheckRateLimit("w", rl); !a {
			t.Fatalf("attempt %d should be allowed", i)
		}
		IncrementRateLimitCount("w")
	}
	if a, _ := CheckRateLimit("w", rl); a {
		t.Fatal("third should be denied within window")
	}

	cur = cur.Add(2 * time.Second)

	if a, _ := CheckRateLimit("w", rl); !a {
		t.Fatal("after window should be allowed again")
	}
}

func TestDecrementConcurrency_StopsAtZero(t *testing.T) {
	resetLimiterState()
	IncrementRateLimitCount("d")
	DecrementConcurrency("d")
	DecrementConcurrency("d")

	v, ok := rateLimitState.Load("d")
	if !ok {
		t.Fatal("entry should exist")
	}
	entry := v.(*RateLimitEntry)
	if entry.CurrentConcurrency != 0 {
		t.Fatalf("concurrency = %d, want 0", entry.CurrentConcurrency)
	}
}

func TestApplyRateLimit_RejectFiltersOut(t *testing.T) {
	resetLimiterState()
	rl := &RateLimiting{Enabled: true, Concurrency: 1, OverLimitStrategy: "reject"}
	cfg := &GatewayYaml{
		Providers: map[string]*Provider{
			"zen": {RateLimiting: rl},
		},
	}
	SetConfig(cfg)
	t.Cleanup(func() { SetConfig(nil) })

	IncrementRateLimitCount(rateLimitKey("zen", "free"))

	cands := []*Candidate{{ProviderId: "zen", ModelGroup: "free"}}
	out := applyRateLimit(cands, &EffectiveProfile{})
	if len(out) != 0 {
		t.Fatalf("reject should remove candidate, got %d", len(out))
	}
}

func TestApplyRateLimit_QueueKeepsCandidate(t *testing.T) {
	resetLimiterState()
	rl := &RateLimiting{Enabled: true, Concurrency: 1, OverLimitStrategy: "queue"}
	cfg := &GatewayYaml{
		Providers: map[string]*Provider{
			"zen": {RateLimiting: rl},
		},
	}
	SetConfig(cfg)
	t.Cleanup(func() { SetConfig(nil) })

	IncrementRateLimitCount(rateLimitKey("zen", "free"))

	cands := []*Candidate{{ProviderId: "zen", ModelGroup: "free"}}
	out := applyRateLimit(cands, &EffectiveProfile{})
	if len(out) != 1 {
		t.Fatalf("queue should keep candidate, got %d", len(out))
	}
}

func TestApplyRateLimit_DowngradeKeepsCandidate(t *testing.T) {
	resetLimiterState()
	rl := &RateLimiting{Enabled: true, Concurrency: 1, OverLimitStrategy: "downgrade"}
	cfg := &GatewayYaml{
		Providers: map[string]*Provider{
			"zen": {RateLimiting: rl},
		},
	}
	SetConfig(cfg)
	t.Cleanup(func() { SetConfig(nil) })

	IncrementRateLimitCount(rateLimitKey("zen", "free"))

	cands := []*Candidate{{ProviderId: "zen", ModelGroup: "free"}}
	out := applyRateLimit(cands, &EffectiveProfile{})
	if len(out) != 1 {
		t.Fatalf("downgrade should keep candidate, got %d", len(out))
	}
}
