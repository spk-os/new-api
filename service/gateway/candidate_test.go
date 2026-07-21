package gateway

import (
	"testing"
)

func twoProviderConfig() *GatewayYaml {
	return &GatewayYaml{
		LLMGateway: map[string]*StrategyGroup{
			"default": {
				Version: "1.0",
				Routing: &RoutingConfig{
					ModelSelection: &ModelSelection{Strategy: "priority"},
					ModelAliases: map[string]interface{}{
						"qwen-max": []string{"qwen3.7-max", "qwen3.5-max"},
						"gpt-4":    "gpt-4-turbo",
					},
					ModelCapabilities: map[string][]string{
						"coding": {"glm-5.1-coder"},
					},
				},
			},
		},
		Providers: map[string]*Provider{
			"zen": {
				Name: "zen", URL: "u1", Key: "k1,k2", Order: 1, Enabled: true,
				Models: []*ModelGroup{
					{Name: "free", Model: "minimax-m3-free", Order: 1, Enabled: true, Tags: []string{"free"}},
					{Name: "paid", Model: "qwen3.7-max,qwen3.5-max", Order: 2, Enabled: true, Tags: []string{"reasoning"}},
				},
			},
			"ali": {
				Name: "ali", URL: "u2", Key: "k3", Order: 2, Enabled: true,
				Models: []*ModelGroup{
					{Name: "coder", Model: "glm-5.1-coder", Order: 1, Enabled: true, Tags: []string{"coding"}},
					{Name: "free", Model: "qwen-flash-free", Order: 2, Enabled: true, Tags: []string{"free"}},
				},
			},
			"disabled_provider": {
				Name: "off", URL: "u3", Key: "k4", Order: 3, Enabled: false,
				Models: []*ModelGroup{
					{Name: "x", Model: "x-1", Order: 1, Enabled: true},
				},
			},
		},
	}
}

func TestBuildCandidates_AutoAll(t *testing.T) {
	setTestConfig(t, twoProviderConfig())
	prof := ResolveEffectiveProfile("default", "", "", "")
	cands := BuildCandidates(prof, "auto", nil)
	if len(cands) != 5 {
		t.Errorf("expected 5 candidates (zen:3 + ali:2), got %d", len(cands))
	}
	if cands[0].ProviderId != "zen" {
		t.Errorf("expected zen first (order=1), got %q", cands[0].ProviderId)
	}
}

func TestBuildCandidates_AutoFree(t *testing.T) {
	setTestConfig(t, twoProviderConfig())
	prof := ResolveEffectiveProfile("default", "", "", "")
	cands := BuildCandidates(prof, "auto-free", nil)
	if len(cands) != 2 {
		t.Errorf("expected 2 free candidates, got %d", len(cands))
	}
	for _, c := range cands {
		if !c.IsFree {
			t.Errorf("non-free candidate %q included", c.ActualModel)
		}
	}
}

func TestBuildCandidates_AutoCapability(t *testing.T) {
	setTestConfig(t, twoProviderConfig())
	prof := ResolveEffectiveProfile("default", "", "", "")
	cands := BuildCandidates(prof, "auto-coding", nil)
	if len(cands) != 1 {
		t.Errorf("expected 1 coding candidate, got %d", len(cands))
	}
	if cands[0].ActualModel != "glm-5.1-coder" {
		t.Errorf("expected glm-5.1-coder, got %q", cands[0].ActualModel)
	}
}

func TestBuildCandidates_AutoTagFallback(t *testing.T) {
	setTestConfig(t, twoProviderConfig())
	prof := ResolveEffectiveProfile("default", "", "", "")
	cands := BuildCandidates(prof, "auto-reasoning", nil)
	if len(cands) != 2 {
		t.Errorf("expected 2 reasoning candidates (zen.paid CSV expanded), got %d", len(cands))
	}
}

func TestBuildCandidates_SpecificModel(t *testing.T) {
	setTestConfig(t, twoProviderConfig())
	prof := ResolveEffectiveProfile("default", "", "", "")
	cands := BuildCandidates(prof, "qwen3.5-max", nil)
	if len(cands) != 1 {
		t.Errorf("expected 1 exact match, got %d", len(cands))
	}
	if cands[0].ActualModel != "qwen3.5-max" {
		t.Errorf("expected qwen3.5-max, got %q", cands[0].ActualModel)
	}
}

func TestBuildCandidates_AliasMatchListAndString(t *testing.T) {
	setTestConfig(t, twoProviderConfig())
	prof := ResolveEffectiveProfile("default", "", "", "")
	cands := BuildCandidates(prof, "qwen-max", nil)
	if len(cands) != 2 {
		t.Errorf("expected 2 alias matches, got %d", len(cands))
	}

	cfg := twoProviderConfig()
	cfg.Providers["zen"].Models = append(cfg.Providers["zen"].Models,
		&ModelGroup{Name: "openai", Model: "gpt-4-turbo", Order: 3, Enabled: true})
	setTestConfig(t, cfg)
	prof2 := ResolveEffectiveProfile("default", "", "", "")
	cands2 := BuildCandidates(prof2, "gpt-4", nil)
	if len(cands2) != 1 || cands2[0].ActualModel != "gpt-4-turbo" {
		t.Errorf("expected string-alias match for gpt-4 -> gpt-4-turbo, got %+v", cands2)
	}
}

func TestBuildCandidates_PreferProvidersFilter(t *testing.T) {
	setTestConfig(t, twoProviderConfig())
	prof := ResolveEffectiveProfile("default", "", "", "")
	opts := &GatewayOptions{PreferProviders: []string{"ali"}}
	cands := BuildCandidates(prof, "auto", opts)
	if len(cands) != 2 {
		t.Errorf("expected 2 ali-only candidates, got %d", len(cands))
	}
	for _, c := range cands {
		if c.ProviderId != "ali" {
			t.Errorf("non-ali candidate leaked: %q", c.ProviderId)
		}
	}
}

func TestBuildCandidates_DisabledProviderSkipped(t *testing.T) {
	setTestConfig(t, twoProviderConfig())
	prof := ResolveEffectiveProfile("default", "", "", "")
	cands := BuildCandidates(prof, "x-1", nil)
	if len(cands) != 0 {
		t.Errorf("expected disabled provider skipped, got %d", len(cands))
	}
}

func TestBuildCandidates_DisabledModelGroupSkipped(t *testing.T) {
	cfg := twoProviderConfig()
	cfg.Providers["zen"].Models[0].Enabled = false
	setTestConfig(t, cfg)
	prof := ResolveEffectiveProfile("default", "", "", "")
	cands := BuildCandidates(prof, "minimax-m3-free", nil)
	if len(cands) != 0 {
		t.Errorf("expected disabled mg skipped, got %d", len(cands))
	}
}

func TestSortCandidates_PriorityByProviderOrder(t *testing.T) {
	setTestConfig(t, twoProviderConfig())
	prof := ResolveEffectiveProfile("default", "", "", "")
	cands := BuildCandidates(prof, "auto", nil)
	for i := 0; i < len(cands)-1; i++ {
		oi := effectiveProviderOrder(cands[i].ProviderId, prof)
		oj := effectiveProviderOrder(cands[i+1].ProviderId, prof)
		if oi > oj {
			t.Errorf("not sorted by order at %d: %d > %d", i, oi, oj)
		}
	}
}

func TestSortCandidates_CostStrategyFreeFirst(t *testing.T) {
	cfg := twoProviderConfig()
	cfg.LLMGateway["default"].Routing.ModelSelection.Strategy = "cost"
	setTestConfig(t, cfg)
	prof := ResolveEffectiveProfile("default", "", "", "")
	cands := BuildCandidates(prof, "auto", nil)
	if len(cands) == 0 {
		t.Fatal("no candidates")
	}
	if !cands[0].IsFree {
		t.Errorf("expected free candidate first under cost strategy, got %+v", cands[0])
	}
}

func TestSortCandidates_PreferFreeOverridesStrategy(t *testing.T) {
	setTestConfig(t, twoProviderConfig())
	prof := ResolveEffectiveProfile("default", "", "", "")
	opts := &GatewayOptions{PreferFree: true}
	cands := BuildCandidates(prof, "auto", opts)
	if !cands[0].IsFree {
		t.Errorf("expected free first with PreferFree, got %+v", cands[0])
	}
}

func TestEffectiveProviderOrder_ClientOverride(t *testing.T) {
	cfg := twoProviderConfig()
	cfg.LLMGateway["default"].Client = map[string]*ClientPolicy{
		"hermes": {ProviderOrders: map[string]int{"ali": 0}},
	}
	setTestConfig(t, cfg)
	prof := ResolveEffectiveProfile("default", "hermes", "", "")
	if effectiveProviderOrder("ali", prof) != 0 {
		t.Errorf("expected client override to set ali=0, got %d", effectiveProviderOrder("ali", prof))
	}
	if effectiveProviderOrder("zen", prof) != 1 {
		t.Errorf("expected zen to remain order=1, got %d", effectiveProviderOrder("zen", prof))
	}
	cands := BuildCandidates(prof, "auto", nil)
	if cands[0].ProviderId != "ali" {
		t.Errorf("expected ali first after client override, got %q", cands[0].ProviderId)
	}
}

func TestMatchModel_AutoVariants(t *testing.T) {
	setTestConfig(t, twoProviderConfig())
	prof := ResolveEffectiveProfile("default", "", "", "")
	mg := &ModelGroup{Name: "free", Tags: []string{"free", "fast"}}

	if !matchModel("any", mg, "", prof) {
		t.Error("empty req should match")
	}
	if !matchModel("any", mg, "auto", prof) {
		t.Error("auto should match")
	}
	if !matchModel("any", mg, "auto-free", prof) {
		t.Error("auto-free should match free-tagged mg")
	}
	if !matchModel("any", mg, "auto-fast", prof) {
		t.Error("auto-fast should fallback to tag")
	}
	mg2 := &ModelGroup{Name: "x", Tags: []string{"slow"}}
	if matchModel("any", mg2, "auto-fast", prof) {
		t.Error("non-fast mg should not match auto-fast")
	}
}

func TestSplitCSV(t *testing.T) {
	if got := splitCSV(""); got != nil {
		t.Errorf("empty: %v", got)
	}
	got := splitCSV(" a , b,, c ")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("len: got %d want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q want %q", i, got[i], want[i])
		}
	}
}

func TestSortByOrder_Stable(t *testing.T) {
	mgs := []*ModelGroup{
		{Name: "b", Order: 2},
		{Name: "a", Order: 1},
		{Name: "c", Order: 1},
	}
	sorted := sortByOrder(mgs)
	if sorted[0].Name != "a" || sorted[1].Name != "c" || sorted[2].Name != "b" {
		t.Errorf("unexpected order: %v %v %v", sorted[0].Name, sorted[1].Name, sorted[2].Name)
	}
}

func TestNewCandidate_KeysAndIsFree(t *testing.T) {
	p := &Provider{Name: "zen", Key: "k1, k2 ,k3"}
	mg := &ModelGroup{Name: "free", Tags: []string{"free"}}
	c := newCandidate("zen", p, mg, "x")
	if len(c.Keys) != 3 {
		t.Errorf("expected 3 keys, got %v", c.Keys)
	}
	if !c.IsFree {
		t.Error("expected IsFree=true")
	}
}

func TestAliasMatch_InterfaceSliceFromYAML(t *testing.T) {
	aliases := map[string]interface{}{
		"big": []interface{}{"big-v1", "big-v2"},
	}
	if !aliasMatch(aliases, "big", "big-v2") {
		t.Error("expected alias match in []interface{}")
	}
	if aliasMatch(aliases, "big", "small") {
		t.Error("unexpected match")
	}
	if aliasMatch(aliases, "missing", "x") {
		t.Error("missing key should not match")
	}
}
