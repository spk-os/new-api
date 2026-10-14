package gateway

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withRoutingEnabled flips common.GatewayRoutingEnabled on for the test and
// restores the prior value on cleanup. VirtualModelNames gates on this global,
// so every case needs it set explicitly.
func withRoutingEnabled(t *testing.T) {
	t.Helper()
	prev := common.GatewayRoutingEnabled
	common.GatewayRoutingEnabled = true
	t.Cleanup(func() { common.GatewayRoutingEnabled = prev })
}

func virtualModelConfig() *GatewayYaml {
	enabled := true
	return &GatewayYaml{
		LLMGateway: map[string]*StrategyGroup{
			"default": {
				Version: "1.0",
				Enabled: &enabled,
				Routing: &RoutingConfig{
					ModelSelection: &ModelSelection{Strategy: "priority"},
					ModelAliases: map[string]interface{}{
						"auto":  "auto-priority",
						"gpt-4": []string{"gpt-4-turbo", "gpt-4o"},
					},
					ModelCapabilities: map[string][]string{
						"coding":      {"qwen3.7-max", "deepseek-v4-pro"},
						"free-height": {"deepseek-v4-flash-free"},
					},
				},
			},
			// disabled group: its capabilities must not contribute.
			"off": {
				Version: "1.0",
				Enabled: func() *bool { v := false; return &v }(),
				Routing: &RoutingConfig{
					ModelCapabilities: map[string][]string{
						"secret": {"leaked"},
					},
				},
			},
		},
		Providers: map[string]*Provider{
			"zen": {
				Name: "zen", URL: "u1", Key: "k1", Order: 1, Enabled: true,
				Models: []*ModelGroup{
					{Name: "free", Model: "deepseek-v4-flash-free", Order: 1, Enabled: true, Tags: []string{"free", "free-height"}},
				},
			},
		},
	}
}

func containsStr(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

func TestVirtualModelNames_DerivesAutoAndAliases(t *testing.T) {
	setTestConfig(t, virtualModelConfig())
	withRoutingEnabled(t)

	got := VirtualModelNames()
	require.NotNil(t, got)

	// hardcoded auto / auto-free
	assert.True(t, containsStr(got, "auto"), "auto must be advertised: %v", got)
	assert.True(t, containsStr(got, "auto-free"), "auto-free must be advertised: %v", got)
	// from modelCapabilities keys (auto-<cap>)
	assert.True(t, containsStr(got, "auto-coding"), "auto-coding: %v", got)
	assert.True(t, containsStr(got, "auto-free-height"), "auto-free-height: %v", got)
	// from modelAliases keys
	assert.True(t, containsStr(got, "gpt-4"), "gpt-4 alias: %v", got)

	// disabled group's capability must NOT leak.
	assert.False(t, containsStr(got, "auto-secret"), "disabled group leaked: %v", got)

	// dedup: "auto" appears both hardcoded and as an alias key -> single entry.
	count := 0
	for _, m := range got {
		if m == "auto" {
			count++
		}
	}
	assert.Equal(t, 1, count, "auto duplicated: %v", got)

	// sorted ascending (VirtualModelNames sorts before returning).
	for i := 0; i < len(got)-1; i++ {
		assert.Less(t, got[i], got[i+1], "not sorted at %d: %v", i, got)
	}
}

func TestVirtualModelNames_DisabledWhenRoutingOff(t *testing.T) {
	setTestConfig(t, virtualModelConfig())
	// common.GatewayRoutingEnabled stays false (default) — no withRoutingEnabled.
	assert.Nil(t, VirtualModelNames())
}

func TestVirtualModelNames_NoRoutingContentReturnsNil(t *testing.T) {
	enabled := true
	cfg := &GatewayYaml{
		LLMGateway: map[string]*StrategyGroup{
			"default": {
				Version: "1.0",
				Enabled: &enabled,
				// no Routing block at all
			},
		},
	}
	setTestConfig(t, cfg)
	withRoutingEnabled(t)
	// No aliases/capabilities declared -> nothing to advertise (the hardcoded
	// auto/auto-free would be misleading without any routing content).
	assert.Nil(t, VirtualModelNames())
}

func TestVirtualModelNames_NoConfigReturnsNil(t *testing.T) {
	setTestConfig(t, nil)
	withRoutingEnabled(t)
	assert.Nil(t, VirtualModelNames())
}
