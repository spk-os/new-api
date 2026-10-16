package gateway

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tiersYaml returns a minimal valid gateway YAML with a tiers block for the
// given group, so validateTiers paths run through ParseAndValidate (the same
// entry point SaveConfig uses).
func tiersYaml(tiersYamlText string) string {
	base := `
llm_gateway:
  default:
    version: "1.0"
    enabled: true
    routing:
      modelSelection: { strategy: priority }
`
	return base + tiersYamlText + `
providers:
  ali:
    name: ali
    url: https://ali.example
    key: "test-key"
    order: 1
    enabled: true
    models:
      - name: main
        model: kimiB,glmA
        order: 1
        enabled: true
`
}

func TestParseAndValidate_TiersValid(t *testing.T) {
	cfg, err := ParseAndValidate(tiersYaml(`
      tiers:
        auto_high:
          models: [glmA, kimiB]
          strategy: priority
        auto_low:
          models: [kimiB]
          strategy: cost
`))
	require.NoError(t, err)
	require.NotNil(t, cfg.LLMGateway["default"].Routing)
	require.Len(t, cfg.LLMGateway["default"].Routing.Tiers, 2)
	high := cfg.LLMGateway["default"].Routing.Tiers["auto_high"]
	require.NotNil(t, high)
	assert.Equal(t, []string{"glmA", "kimiB"}, high.Models)
	assert.Equal(t, "priority", high.Strategy)
}

func TestParseAndValidate_TiersRejectsBadName(t *testing.T) {
	cases := []struct {
		name      string
		tierYaml  string
		wantError string
	}{
		{"typo tier name", `
      tiers:
        auto_medium:
          models: [glmA]
`, "must be one of [auto_low, auto_mid, auto_high, auto_max]"},
		{"empty models", `
      tiers:
        auto_high:
          models: []
`, "must be a non-empty list"},
		{"null tier", `
      tiers:
        auto_high:
`, "is null"},
		{"blank model entry", `
      tiers:
        auto_high:
          models: [" "]
`, "must not be blank"},
		{"bad strategy", `
      tiers:
        auto_high:
          models: [glmA]
          strategy: cheapest
`, "must be one of [priority, session-balance, cost]"},
		{"self-referencing fallback", `
      tiers:
        auto_high:
          models: [glmA]
          fallbackTier: auto_high
`, "must not reference itself"},
		{"undefined fallback", `
      tiers:
        auto_high:
          models: [glmA]
          fallbackTier: auto_mid
`, "references undefined tier"},
		{"fallback cycle", `
      tiers:
        auto_high:
          models: [glmA]
          fallbackTier: auto_mid
        auto_mid:
          models: [kimiB]
          fallbackTier: auto_low
        auto_low:
          models: [kimiB]
          fallbackTier: auto_high
`, "forms a cycle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAndValidate(tiersYaml(tc.tierYaml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantError)
		})
	}
}

func TestParseAndValidate_TiersRejectsAliasCollision(t *testing.T) {
	_, err := ParseAndValidate(tiersYaml(`
      modelAliases:
        auto_high: kimiB
      tiers:
        auto_high:
          models: [glmA]
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collides with a modelAliases key")
}

func TestParseAndValidate_TiersRejectsCapabilityCollision(t *testing.T) {
	_, err := ParseAndValidate(tiersYaml(`
      modelCapabilities:
        auto_high: [glmA]
      tiers:
        auto_high:
          models: [glmA]
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collides with a modelCapabilities key")
}

func TestParseAndValidate_TiersRejectsMappingSwallow(t *testing.T) {
	yamlText := `
llm_gateway:
  default:
    version: "1.0"
    enabled: true
    routing:
      modelSelection: { strategy: priority }
      tiers:
        auto_high:
          models: [glmA]
providers:
  ali:
    name: ali
    url: https://ali.example
    key: "test-key"
    order: 1
    enabled: true
    models:
      - name: main
        model: glmA
        order: 1
        enabled: true
model_name_mapping:
  - "auto_high: glmA"
`
	_, err := ParseAndValidate(yamlText)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "would rewrite tier name(s) away")
}

// TestParseAndValidate_TiersFallbackChainOfTwo protects the non-cyclic
// fallback chain: auto_high → auto_mid must parse cleanly (the feature is
// reserved for a later phase but the config shape is already accepted).
func TestParseAndValidate_TiersFallbackChainOfTwo(t *testing.T) {
	cfg, err := ParseAndValidate(tiersYaml(`
      tiers:
        auto_high:
          models: [glmA]
          fallbackTier: auto_mid
        auto_mid:
          models: [kimiB]
`))
	require.NoError(t, err)
	high := cfg.LLMGateway["default"].Routing.Tiers["auto_high"]
	require.NotNil(t, high)
	assert.Equal(t, "auto_mid", high.FallbackTier)
}

// TestMergeRouting_TiersKeyLevelOverride protects the group merge contract:
// a non-default group overriding one tier replaces that tier wholesale and
// keeps the other tiers inherited from default.
func TestMergeRouting_TiersKeyLevelOverride(t *testing.T) {
	base := &RoutingConfig{
		ModelSelection: &ModelSelection{Strategy: "priority"},
		Tiers: map[string]*TierConfig{
			"auto_high": {Models: []string{"glmA"}},
			"auto_low":  {Models: []string{"kimiB"}},
		},
	}
	override := &RoutingConfig{
		Tiers: map[string]*TierConfig{
			"auto_high": {Models: []string{"glmA", "kimiB"}, Strategy: "cost"},
		},
	}
	merged := cloneRouting(base)
	mergeRouting(merged, override)

	require.Len(t, merged.Tiers, 2)
	high := merged.Tiers["auto_high"]
	require.NotNil(t, high)
	assert.Equal(t, []string{"glmA", "kimiB"}, high.Models, "group tier replaces default's wholesale")
	assert.Equal(t, "cost", high.Strategy)
	low := merged.Tiers["auto_low"]
	require.NotNil(t, low)
	assert.Equal(t, []string{"kimiB"}, low.Models, "unmentioned tier stays inherited")
}
