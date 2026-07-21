package gateway

import (
	"testing"
)

func setTestConfig(t *testing.T, cfg *GatewayYaml) {
	t.Helper()
	prev := globalConfig.Load()
	swapGlobalConfig(cfg)
	t.Cleanup(func() { swapGlobalConfig(prev) })
}

func baseDefaultGroup() *StrategyGroup {
	return &StrategyGroup{
		Version: "1.0",
		LLMCommon: &LLMCommon{
			Timeout: &Timeout{ConnectTimeout: 5, ReadTimeout: 60, StreamIdleTimeout: 30},
			RateLimiting: &RateLimiting{
				Enabled:           true,
				Concurrency:       10,
				WindowRate:        "1h-100",
				OverLimitStrategy: "queue",
			},
		},
		Affinity: &AffinityConfig{
			Enabled:  true,
			Binding:  &AffinityBinding{TTL: 600, MaxTTL: 3600, Storage: "redis"},
			Failover: &AffinityFailover{Enable: true, Priority: "sameModel"},
		},
		Routing: &RoutingConfig{
			ModelSelection: &ModelSelection{Strategy: "priority"},
		},
	}
}

func TestResolveEffectiveProfile_DefaultOnly(t *testing.T) {
	setTestConfig(t, &GatewayYaml{
		LLMGateway: map[string]*StrategyGroup{
			"default": baseDefaultGroup(),
		},
	})

	p := ResolveEffectiveProfile("default", "", "", "")
	if p == nil {
		t.Fatal("nil profile")
	}
	if p.Timeout == nil || p.Timeout.ConnectTimeout != 5 {
		t.Errorf("timeout not cloned: %+v", p.Timeout)
	}
	if p.RateLimiting == nil || p.RateLimiting.Concurrency != 10 {
		t.Errorf("rateLimiting not cloned: %+v", p.RateLimiting)
	}
	if p.Affinity == nil || !p.Affinity.Failover.Enable {
		t.Errorf("affinity not cloned: %+v", p.Affinity)
	}
	if p.Routing == nil || p.Routing.ModelSelection.Strategy != "priority" {
		t.Errorf("routing not cloned: %+v", p.Routing)
	}
}

func TestResolveEffectiveProfile_GroupOverridesAffinityFailover(t *testing.T) {
	def := baseDefaultGroup()
	quality := &StrategyGroup{
		Version: "1.0",
		Affinity: &AffinityConfig{
			Enabled:  true,
			Failover: &AffinityFailover{Enable: false, Priority: "sameProvider"},
		},
		Routing: &RoutingConfig{
			ModelSelection: &ModelSelection{Strategy: "cost"},
		},
	}
	setTestConfig(t, &GatewayYaml{
		LLMGateway: map[string]*StrategyGroup{
			"default": def,
			"quality": quality,
		},
	})

	p := ResolveEffectiveProfile("quality", "", "", "")
	if p.Affinity.Failover.Enable {
		t.Errorf("expected failover.enable=false from quality, got true")
	}
	if p.Affinity.Failover.Priority != "sameProvider" {
		t.Errorf("expected priority=sameProvider, got %q", p.Affinity.Failover.Priority)
	}
	if p.Routing.ModelSelection.Strategy != "cost" {
		t.Errorf("expected strategy=cost, got %q", p.Routing.ModelSelection.Strategy)
	}
	if p.Timeout == nil || p.Timeout.ConnectTimeout != 5 {
		t.Errorf("default timeout should remain: %+v", p.Timeout)
	}
}

func TestResolveEffectiveProfile_ProviderRateLimitOverride(t *testing.T) {
	setTestConfig(t, &GatewayYaml{
		LLMGateway: map[string]*StrategyGroup{
			"default": baseDefaultGroup(),
		},
		Providers: map[string]*Provider{
			"zen": {
				Name:    "zen",
				URL:     "https://zen.example",
				Key:     "k1",
				Enabled: true,
				RateLimiting: &RateLimiting{
					Enabled:     true,
					Concurrency: 50,
					WindowRate:  "1h-500",
				},
				Timeout: &Timeout{ConnectTimeout: 9, ReadTimeout: 90},
			},
		},
	})

	p := ResolveEffectiveProfile("default", "", "zen", "")
	if p.RateLimiting.Concurrency != 50 {
		t.Errorf("expected provider concurrency=50, got %d", p.RateLimiting.Concurrency)
	}
	if p.Timeout.ConnectTimeout != 9 {
		t.Errorf("expected provider connect=9, got %d", p.Timeout.ConnectTimeout)
	}
}

func TestResolveEffectiveProfile_ModelGroupRateLimitOverride(t *testing.T) {
	setTestConfig(t, &GatewayYaml{
		LLMGateway: map[string]*StrategyGroup{"default": baseDefaultGroup()},
		Providers: map[string]*Provider{
			"zen": {
				Name: "zen", URL: "u", Key: "k", Enabled: true,
				RateLimiting: &RateLimiting{Enabled: true, Concurrency: 50, WindowRate: "1h-500"},
				Models: []*ModelGroup{
					{
						Name: "free", Model: "x-free", Order: 1, Enabled: true,
						Tags:         []string{"free"},
						RateLimiting: &RateLimiting{Enabled: true, Concurrency: 5, WindowRate: "1h-30"},
					},
				},
			},
		},
	})

	p := ResolveEffectiveProfile("default", "", "zen", "free")
	if p.RateLimiting.Concurrency != 5 {
		t.Errorf("expected model-group concurrency=5, got %d", p.RateLimiting.Concurrency)
	}
}

func TestResolveEffectiveProfile_ClientAffinityAndOrders(t *testing.T) {
	def := baseDefaultGroup()
	def.Client = map[string]*ClientPolicy{
		"hermes-agent": {
			Affinity: &AffinityConfig{
				Enabled: true,
				Binding: &AffinityBinding{TTL: 1800},
			},
			ProviderOrders: map[string]int{"zen": 1, "ali": 2},
		},
	}
	setTestConfig(t, &GatewayYaml{
		LLMGateway: map[string]*StrategyGroup{"default": def},
	})

	p := ResolveEffectiveProfile("default", "hermes-agent", "", "")
	if p.Affinity.Binding.TTL != 1800 {
		t.Errorf("expected TTL=1800, got %d", p.Affinity.Binding.TTL)
	}
	if p.Affinity.Binding.MaxTTL != 3600 {
		t.Errorf("expected MaxTTL preserved=3600, got %d", p.Affinity.Binding.MaxTTL)
	}
	if p.ProviderOrderOverride["zen"] != 1 || p.ProviderOrderOverride["ali"] != 2 {
		t.Errorf("provider orders not applied: %+v", p.ProviderOrderOverride)
	}
}

func TestResolveEffectiveProfile_MissingGroupAndClientFallthrough(t *testing.T) {
	setTestConfig(t, &GatewayYaml{
		LLMGateway: map[string]*StrategyGroup{"default": baseDefaultGroup()},
	})

	p := ResolveEffectiveProfile("nonexistent", "ghost", "", "")
	if p == nil {
		t.Fatal("nil profile")
	}
	if p.Timeout == nil || p.Timeout.ConnectTimeout != 5 {
		t.Errorf("default timeout should remain when group missing: %+v", p.Timeout)
	}
	if p.ProviderOrderOverride != nil {
		t.Errorf("expected no client overrides, got %+v", p.ProviderOrderOverride)
	}
}

func TestResolveEffectiveProfile_NilOverrideFieldsPreserved(t *testing.T) {
	def := baseDefaultGroup()
	partial := &StrategyGroup{
		Version: "1.0",
		Routing: &RoutingConfig{
			ModelSelection: &ModelSelection{Strategy: "cost"},
		},
	}
	setTestConfig(t, &GatewayYaml{
		LLMGateway: map[string]*StrategyGroup{
			"default":   def,
			"efficient": partial,
		},
	})

	p := ResolveEffectiveProfile("efficient", "", "", "")
	if p.Routing.ModelSelection.Strategy != "cost" {
		t.Errorf("expected strategy=cost, got %q", p.Routing.ModelSelection.Strategy)
	}
	if p.Timeout == nil || p.Timeout.ConnectTimeout != 5 {
		t.Errorf("expected default timeout preserved, got %+v", p.Timeout)
	}
	if p.RateLimiting == nil || p.RateLimiting.Concurrency != 10 {
		t.Errorf("expected default rateLimiting preserved, got %+v", p.RateLimiting)
	}
	if p.Affinity == nil {
		t.Errorf("expected default affinity preserved")
	}
}

func TestResolveEffectiveProfile_NilConfig(t *testing.T) {
	setTestConfig(t, nil)
	p := ResolveEffectiveProfile("default", "", "", "")
	if p == nil {
		t.Fatal("nil profile returned for nil config")
	}
}

func TestCloneProfile_DeepCopiesTimeout(t *testing.T) {
	g := baseDefaultGroup()
	p := cloneProfile(g)
	p.Timeout.ConnectTimeout = 999
	if g.LLMCommon.Timeout.ConnectTimeout == 999 {
		t.Error("cloneProfile leaked mutation back into source")
	}
}

func TestMergeAffinity_PreservesUnsetFields(t *testing.T) {
	base := &AffinityConfig{
		Enabled: true,
		Binding: &AffinityBinding{TTL: 600, MaxTTL: 3600, Storage: "redis"},
	}
	override := &AffinityConfig{
		Enabled: true,
		Binding: &AffinityBinding{TTL: 1800},
	}
	mergeAffinity(base, override)
	if base.Binding.TTL != 1800 {
		t.Errorf("ttl override failed: %d", base.Binding.TTL)
	}
	if base.Binding.MaxTTL != 3600 {
		t.Errorf("MaxTTL should be preserved: %d", base.Binding.MaxTTL)
	}
	if base.Binding.Storage != "redis" {
		t.Errorf("Storage should be preserved: %q", base.Binding.Storage)
	}
}
