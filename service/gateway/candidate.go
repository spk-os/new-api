package gateway

import (
	"sort"
	"strings"

	"github.com/QuantumNous/new-api/common"
)

func BuildCandidates(profile *EffectiveProfile, requestModel string, opts *GatewayOptions) []*Candidate {
	cfg := GetConfig()
	if cfg == nil || cfg.Providers == nil {
		return nil
	}

	pids := make([]string, 0, len(cfg.Providers))
	for pid := range cfg.Providers {
		pids = append(pids, pid)
	}
	sort.SliceStable(pids, func(i, j int) bool {
		pi, pj := cfg.Providers[pids[i]], cfg.Providers[pids[j]]
		oi, oj := 0, 0
		if pi != nil {
			oi = pi.Order
		}
		if pj != nil {
			oj = pj.Order
		}
		if oi != oj {
			return oi < oj
		}
		return pids[i] < pids[j]
	})

	var out []*Candidate
	for _, pid := range pids {
		p := cfg.Providers[pid]
		if p == nil || !p.Enabled {
			continue
		}
		if IsProviderInCooldown(pid) {
			continue
		}
		if opts != nil && len(opts.PreferProviders) > 0 && !contains(opts.PreferProviders, pid) {
			continue
		}
		for _, mg := range sortByOrder(p.Models) {
			if mg == nil || !mg.Enabled {
				continue
			}
			for _, m := range splitCSV(mg.Model) {
				if !matchModel(m, mg, requestModel, profile) {
					continue
				}
				out = append(out, newCandidate(pid, p, mg, m))
			}
		}
	}

	sortCandidates(out, profile, opts)
	return out
}

func newCandidate(pid string, p *Provider, mg *ModelGroup, actualModel string) *Candidate {
	return &Candidate{
		ProviderId:   pid,
		ProviderName: p.Name,
		ChannelId:    0,
		KeyIndex:     0,
		Keys:         splitCSV(p.Key),
		ModelGroup:   mg.Name,
		ActualModel:  actualModel,
		IsFree:       hasTag(mg, "free"),
	}
}

func matchModel(m string, mg *ModelGroup, req string, p *EffectiveProfile) bool {
	switch {
	case req == "" || req == "auto":
		return true
	case req == "auto-free":
		return hasTag(mg, "free")
	case strings.HasPrefix(req, "auto-"):
		capName := strings.TrimPrefix(req, "auto-")
		if p != nil && p.Routing != nil && p.Routing.ModelCapabilities != nil {
			if list, ok := p.Routing.ModelCapabilities[capName]; ok {
				if contains(list, m) {
					return true
				}
			}
		}
		return hasTag(mg, capName)
	default:
		if m == req {
			return true
		}
		if p != nil && p.Routing != nil {
			return aliasMatch(p.Routing.ModelAliases, req, m)
		}
		return false
	}
}

func sortCandidates(cs []*Candidate, p *EffectiveProfile, opts *GatewayOptions) {
	costFirst := false
	if p != nil && p.Routing != nil && p.Routing.ModelSelection != nil &&
		p.Routing.ModelSelection.Strategy == "cost" {
		costFirst = true
	}
	if opts != nil && opts.PreferFree {
		costFirst = true
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if costFirst && cs[i].IsFree != cs[j].IsFree {
			return cs[i].IsFree
		}
		oi := effectiveProviderOrder(cs[i].ProviderId, p)
		oj := effectiveProviderOrder(cs[j].ProviderId, p)
		if oi != oj {
			return oi < oj
		}
		return false
	})
}

func effectiveProviderOrder(providerId string, profile *EffectiveProfile) int {
	if profile != nil && profile.ProviderOrderOverride != nil {
		if o, ok := profile.ProviderOrderOverride[providerId]; ok {
			return o
		}
	}
	cfg := GetConfig()
	if cfg == nil || cfg.Providers == nil {
		return 0
	}
	if p, ok := cfg.Providers[providerId]; ok && p != nil {
		return p.Order
	}
	return 0
}

func aliasMatch(aliases map[string]interface{}, requested, actual string) bool {
	if aliases == nil {
		return false
	}
	v, ok := aliases[requested]
	if !ok {
		return false
	}
	switch t := v.(type) {
	case string:
		return t == actual
	case []string:
		return contains(t, actual)
	case []interface{}:
		for _, e := range t {
			if s, ok := e.(string); ok && s == actual {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func hasTag(mg *ModelGroup, tag string) bool {
	if mg == nil {
		return false
	}
	for _, t := range mg.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// splitCSV splits a comma-separated field (provider key, model list) into
// segments. Segments containing commas or quotes (e.g. credential JSON keys
// synced back into the YAML by the gateway reverse sync) are wrapped in
// double quotes with inner quotes escaped as ""; quoted segments are
// unescaped here so callers always see the raw value. Unquoted input parses
// exactly like a plain comma split with whitespace trimming.
func splitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []string
	var cur strings.Builder
	inQuotes := false
	quoteStarted := false
	flush := func() {
		seg := cur.String()
		if !quoteStarted {
			seg = strings.TrimSpace(seg)
		}
		if seg != "" {
			out = append(out, seg)
		}
		cur.Reset()
		quoteStarted = false
	}
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '"' && (i == 0 || runes[i-1] == ',' || strings.TrimSpace(cur.String()) == "") && !inQuotes && !quoteStarted:
			// segment opens with a quote
			inQuotes = true
			quoteStarted = true
		case r == '"' && inQuotes:
			if i+1 < len(runes) && runes[i+1] == '"' {
				cur.WriteRune('"')
				i++
			} else {
				inQuotes = false
			}
		case r == ',' && !inQuotes:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

func sortByOrder(groups []*ModelGroup) []*ModelGroup {
	out := make([]*ModelGroup, len(groups))
	copy(out, groups)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i] == nil || out[j] == nil {
			return out[j] == nil
		}
		return out[i].Order < out[j].Order
	})
	return out
}

// VirtualModelNames returns the virtual model names the gateway can route —
// derived from modelAliases keys and modelCapabilities keys (prefixed with
// "auto-"), plus the hardcoded "auto" and "auto-free". These are not real
// channel models; the strategy middleware rewrites them to a real model at
// request time (see matchModel). Exposing them in GET /v1/models lets agents
// discover and use the gateway's auto-routing entry points.
//
// The set is the union across every enabled strategy group (each merged with
// the default group via ResolveEffectiveProfile, so default capabilities also
// apply to non-default groups). Returns nil when gateway routing is disabled
// or no config is loaded, so callers can treat the slice as optional.
func VirtualModelNames() []string {
	if !common.GatewayRoutingEnabled {
		return nil
	}
	cfg := GetConfig()
	if cfg == nil || len(cfg.LLMGateway) == 0 {
		return nil
	}

	set := make(map[string]struct{})
	// matchModel routes these two unconditionally (case req == "auto" / "auto-free"),
	// so they are always callable virtual models when the gateway is on.
	set["auto"] = struct{}{}
	set["auto-free"] = struct{}{}

	for name, group := range cfg.LLMGateway {
		if group == nil {
			continue
		}
		if group.Enabled != nil && !*group.Enabled {
			continue
		}
		// ResolveEffectiveProfile merges default into this group, so default's
		// capabilities/aliases are captured once per group; the map dedups.
		prof := ResolveEffectiveProfile(name, "", "", "")
		if prof == nil || prof.Routing == nil {
			continue
		}
		for capName := range prof.Routing.ModelCapabilities {
			if capName == "" {
				continue
			}
			set["auto-"+capName] = struct{}{}
		}
		for alias := range prof.Routing.ModelAliases {
			if alias == "" {
				continue
			}
			set[alias] = struct{}{}
		}
	}

	// Drop the two hardcoded entries if no group declared any routing content —
	// otherwise we'd advertise auto/auto-free even when the gateway has no
	// aliases/capabilities configured at all.
	if len(set) <= 2 {
		// keep them only when at least one alias/capability was collected
		_, hasAliasOrCap := peekRouting(cfg)
		if !hasAliasOrCap {
			return nil
		}
	}

	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// peekRouting reports whether any enabled strategy group (merged with default)
// declares modelAliases or modelCapabilities. Used by VirtualModelNames to
// decide whether the hardcoded "auto"/"auto-free" entries should be advertised.
func peekRouting(cfg *GatewayYaml) (string, bool) {
	for name, group := range cfg.LLMGateway {
		if group == nil {
			continue
		}
		if group.Enabled != nil && !*group.Enabled {
			continue
		}
		prof := ResolveEffectiveProfile(name, "", "", "")
		if prof == nil || prof.Routing == nil {
			continue
		}
		if len(prof.Routing.ModelCapabilities) > 0 || len(prof.Routing.ModelAliases) > 0 {
			return name, true
		}
	}
	return "", false
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}
