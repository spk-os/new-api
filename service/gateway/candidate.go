package gateway

import (
	"sort"
	"strings"
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

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
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

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}
