package gateway

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tierTestConfig builds a config with one tier (auto_high: [glmA, kimiB])
// spanning two providers plus a free model, for tier selection tests.
func tierTestConfig() *GatewayYaml {
	return &GatewayYaml{
		LLMGateway: map[string]*StrategyGroup{
			"default": {
				Version: "1.0",
				Routing: &RoutingConfig{
					ModelSelection: &ModelSelection{Strategy: "priority"},
					ModelAliases: map[string]interface{}{
						"gpt-4": "gpt-4-turbo",
					},
					Tiers: map[string]*TierConfig{
						"auto_high": {Models: []string{"glmA", "kimiB"}},
					},
				},
			},
		},
		Providers: map[string]*Provider{
			// ali has the SECOND tier model but the FIRST provider order —
			// tier model order (glmA first) must outrank provider order.
			"ali": {
				Name: "ali", URL: "u1", Key: "k1", Order: 1, Enabled: true,
				Models: []*ModelGroup{
					{Name: "main", Model: "kimiB", Order: 1, Enabled: true},
					{Name: "free", Model: "free-x", Order: 2, Enabled: true, Tags: []string{"free"}},
				},
			},
			"zen": {
				Name: "zen", URL: "u2", Key: "k2", Order: 2, Enabled: true,
				Models: []*ModelGroup{
					{Name: "main", Model: "glmA", Order: 1, Enabled: true},
				},
			},
		},
	}
}

// TestMatchModel_TierRequests protects the tier selection contract: a tier
// name matches only its member models, non-member models (including
// capability-style auto- names) are excluded, and tier names never fall
// through to alias/exact matching when the tier is not configured.
func TestMatchModel_TierRequests(t *testing.T) {
	setTestConfig(t, tierTestConfig())
	prof := ResolveEffectiveProfile("default", "", "", "")

	mg := &ModelGroup{Name: "main", Model: "glmA", Enabled: true}
	assert.True(t, matchModel("glmA", mg, "auto_high", prof), "tier member must match")
	assert.False(t, matchModel("free-x", mg, "auto_high", prof), "non-member must not match")
	assert.False(t, matchModel("glmA", mg, "auto_low", prof), "unconfigured tier must not match anything")
	// Capability-style names are NOT tiers: auto-high (hyphen) is a
	// capability key lookup, distinct from auto_high.
	assert.False(t, matchModel("glmA", mg, "auto-high", prof), "hyphen variant is not a tier")
}

// TestBuildCandidates_TierExpandsAndSorts protects the admin-intent ordering:
// the tier Models array order (front = preferred) outranks provider order, so
// glmA (index 0, provider zen order=2) precedes kimiB (index 1, provider ali
// order=1). Every candidate carries its tier index.
func TestBuildCandidates_TierExpandsAndSorts(t *testing.T) {
	setTestConfig(t, tierTestConfig())
	prof := ResolveEffectiveProfile("default", "", "", "")

	cands := BuildCandidates(prof, "auto_high", nil)
	require.Len(t, cands, 2)
	assert.Equal(t, "zen", cands[0].ProviderId)
	assert.Equal(t, "glmA", cands[0].ActualModel)
	assert.Equal(t, 0, cands[0].TierIndex)
	assert.Equal(t, "ali", cands[1].ProviderId)
	assert.Equal(t, "kimiB", cands[1].ActualModel)
	assert.Equal(t, 1, cands[1].TierIndex)
}

// TestBuildCandidates_TierSameModelTwoProviders protects the cross-provider
// expansion of one tier entry: a model served by two providers yields one
// candidate per provider, both with the same tier index, ordered by provider
// order inside the model.
func TestBuildCandidates_TierSameModelTwoProviders(t *testing.T) {
	cfg := tierTestConfig()
	cfg.LLMGateway["default"].Routing.Tiers["auto_high"] = &TierConfig{Models: []string{"kimiB"}}
	cfg.Providers["zen"].Models = append(cfg.Providers["zen"].Models,
		&ModelGroup{Name: "dup", Model: "kimiB", Order: 2, Enabled: true})
	setTestConfig(t, cfg)
	prof := ResolveEffectiveProfile("default", "", "", "")

	cands := BuildCandidates(prof, "auto_high", nil)
	require.Len(t, cands, 2)
	assert.Equal(t, 0, cands[0].TierIndex)
	assert.Equal(t, 0, cands[1].TierIndex)
	assert.Equal(t, "ali", cands[0].ProviderId, "provider order decides inside one tier model")
	assert.Equal(t, "zen", cands[1].ProviderId)
}

// TestBuildCandidates_NonTierKeepsTierIndexMinusOne protects the non-tier
// regression surface: ordinary requests must leave TierIndex at -1 so the
// sort layer's tier branch never fires for them.
func TestBuildCandidates_NonTierKeepsTierIndexMinusOne(t *testing.T) {
	setTestConfig(t, tierTestConfig())
	prof := ResolveEffectiveProfile("default", "", "", "")

	for _, req := range []string{"auto", "auto-free", "kimiB"} {
		cands := BuildCandidates(prof, req, nil)
		require.NotEmpty(t, cands, "request %q should produce candidates", req)
		for _, cand := range cands {
			assert.Equal(t, -1, cand.TierIndex, "request %q must not stamp TierIndex", req)
		}
	}
	// An unknown non-tier name resolves nothing (exact+alias miss).
	cands := BuildCandidates(prof, "gpt-4", nil)
	assert.Empty(t, cands, "unknown model must produce no candidates")
}

// TestBuildCandidates_TierStrategyCost protects the tier-level cost strategy:
// a tier configured with strategy=cost orders free candidates ahead of paid
// ones even when the group strategy is priority. The sort is driven by
// TierConfig.Strategy, not the group ModelSelection.
func TestBuildCandidates_TierStrategyCost(t *testing.T) {
	cfg := tierTestConfig()
	cfg.LLMGateway["default"].Routing.Tiers["auto_low"] = &TierConfig{
		Models:   []string{"kimiB", "free-x"},
		Strategy: "cost",
	}
	setTestConfig(t, cfg)
	prof := ResolveEffectiveProfile("default", "", "", "")

	cands := BuildCandidates(prof, "auto_low", nil)
	require.Len(t, cands, 2)
	// Tier model order puts kimiB first, but strategy=cost re-ranks the
	// free model ahead regardless of tier index.
	assert.Equal(t, "free-x", cands[0].ActualModel, "cost strategy must put the free model first")
	assert.True(t, cands[0].IsFree)
	assert.Equal(t, "kimiB", cands[1].ActualModel)
}

// TestVirtualModelNames_TiersExposesConfiguredTier protects the /v1/models
// contract: only whitelisted, configured tier names are advertised; a
// rogue tier key can never leak into the model list.
func TestVirtualModelNames_TiersExposesConfiguredTier(t *testing.T) {
	withRoutingEnabled(t)
	cfg := tierTestConfig()
	cfg.LLMGateway["default"].Routing.Tiers["rogue_tier"] = &TierConfig{Models: []string{"glmA"}}
	setTestConfig(t, cfg)

	names := VirtualModelNames()
	assert.Contains(t, names, "auto_high")
	assert.NotContains(t, names, "rogue_tier", "non-whitelisted tier must not be advertised")
	assert.NotContains(t, names, "auto_low", "unconfigured tier must not be advertised")
}

// TestTierModelIndexAliasResolution protects tier entries that name an alias
// instead of a concrete model: the alias expands through modelAliases and
// every expanded model keeps the alias's position in the tier order.
func TestTierModelIndexAliasResolution(t *testing.T) {
	cfg := tierTestConfig()
	// gpt-4 → gpt-4-turbo (alias from tierTestConfig); zen serves it.
	cfg.Providers["zen"].Models = append(cfg.Providers["zen"].Models,
		&ModelGroup{Name: "legacy", Model: "gpt-4-turbo", Order: 3, Enabled: true})
	cfg.LLMGateway["default"].Routing.Tiers["auto_high"] = &TierConfig{
		Models: []string{"gpt-4", "glmA"},
	}
	setTestConfig(t, cfg)
	prof := ResolveEffectiveProfile("default", "", "", "")

	cands := BuildCandidates(prof, "auto_high", nil)
	require.NotEmpty(t, cands)
	// The alias entry keeps tier index 0; glmA shifts to 1.
	for _, cand := range cands {
		switch cand.ActualModel {
		case "gpt-4-turbo":
			assert.Equal(t, 0, cand.TierIndex)
		case "glmA":
			assert.Equal(t, 1, cand.TierIndex)
		default:
			t.Fatalf("unexpected candidate model %q", cand.ActualModel)
		}
	}
}
