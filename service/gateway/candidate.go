package gateway

import (
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// capabilityModels resolves the capability list for capName from the
// profile's modelCapabilities map. Entries support the "[HH-HH]model"
// time-window syntax: returns (windowEntries, bareOrPlain, anyWindowed)
// where windowEntries holds the parsed list (with bare entries kept in
// order for fallback) and anyWindowed reports whether at least one entry
// carried a window. A plain list (no windows) returns the models as-is.
func capabilityModels(p *EffectiveProfile, capName string) (entries []timeWindowEntry, anyWindowed bool, configured bool) {
	if p == nil || p.Routing == nil || p.Routing.ModelCapabilities == nil {
		return nil, false, false
	}
	raw, ok := p.Routing.ModelCapabilities[capName]
	if !ok {
		return nil, false, false
	}
	for _, item := range raw {
		e, err := parseTimeWindowEntry(item)
		if err != nil {
			continue // save-time validate rejects bad entries; defensive here
		}
		if e.hasWindow() {
			anyWindowed = true
		}
		entries = append(entries, e)
	}
	return entries, anyWindowed, len(entries) > 0
}

// stampWindowRank sets WindowRank on candidates of an "auto" request whose
// capability list carried time windows: 0 for the active window's model,
// 1 for every other model of the list (bare fallback included). No-op for
// lists without windows (all candidates stay at the init value).
func stampWindowRank(out []*Candidate, entries []timeWindowEntry, now time.Time) {
	if len(entries) == 0 {
		return
	}
	active := pickTimeWindowModel(entries, now)
	for _, cand := range out {
		if cand == nil {
			continue
		}
		if cand.ActualModel == active {
			cand.WindowRank = 0
		} else {
			cand.WindowRank = 1
		}
	}
}

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
				tierIdx := -1
				if isTierName(requestModel) {
					if idx, ok := tierCandidateModel(profile, requestModel, m); ok {
						tierIdx = idx
					} else {
						continue
					}
				}
				if !matchModel(m, mg, requestModel, profile) {
					continue
				}
				cand := newCandidate(pid, p, mg, m)
				cand.TierIndex = tierIdx
				cand.WindowRank = -1
				out = append(out, cand)
			}
		}
	}

	// "auto" requests whose capability list carries "[HH-HH]" time windows:
	// stamp the per-candidate window rank so sortCandidates can put the
	// active window's model ahead of provider order.
	if requestModel == "auto" {
		if entries, anyWindowed, configured := capabilityModels(profile, "auto"); anyWindowed && configured {
			stampWindowRank(out, entries, timeWindowNowFunc())
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

// allowedTierNames is the closed set of tier virtual-model names. Tiers live
// under routing.tiers; a name outside this set is rejected by validateTiers.
var allowedTierNames = map[string]bool{
	"auto_low":  true,
	"auto_mid":  true,
	"auto_high": true,
	"auto_max":  true,
}

// isTierName reports whether s is one of the four tier virtual-model names.
// Exact-match only — the "auto-" prefix branch below must keep handling
// capability names (auto-coding etc.) that are NOT tiers.
func isTierName(s string) bool {
	return allowedTierNames[s]
}

// tierModelIndex returns the position of model m inside tier's Models array
// (0-based), resolving through modelAliases exactly like matchModel's exact
// path: first a direct name match, then aliases whose expansion contains m.
// -1 = m is not a member of the tier.
func tierModelIndex(p *EffectiveProfile, tier *TierConfig, m string) int {
	if tier == nil {
		return -1
	}
	for i, name := range tier.Models {
		if name == m {
			return i
		}
	}
	// Alias resolution: a tier entry may name an alias ("gpt-4") whose
	// expansion lists concrete models. Use matchModel's exact/alias semantics
	// against the alias entry so tier membership follows the same rules the
	// default matchModel path enforces.
	for i, name := range tier.Models {
		if matchModel(m, nil, name, p) {
			return i
		}
	}
	return -1
}

// tierCandidateModel reports whether provider model m belongs to the tier
// named by tierName, and returns its Models-array index.
func tierCandidateModel(p *EffectiveProfile, tierName, m string) (int, bool) {
	if p == nil || p.Routing == nil || p.Routing.Tiers == nil {
		return -1, false
	}
	tier := p.Routing.Tiers[tierName]
	if tier == nil || len(tier.Models) == 0 {
		return -1, false
	}
	if idx := tierModelIndex(p, tier, m); idx >= 0 {
		return idx, true
	}
	// matchModel's default branch already covers aliases against the profile;
	// tierModelIndex above covers the alias-inside-tier case. Nothing else.
	return -1, false
}

// matchCapabilityEntry reports whether model m is reachable through the
// modelCapabilities list at now. The list may mix plain names and
// "[HH-HH]model" time-windowed entries; the model selected for the current
// moment (active window's model, else first bare entry, else first entry —
// pickTimeWindowModel's semantics) matches, plus plain names always match
// so non-windowed lists keep their historical behavior.
func matchCapabilityEntry(list []string, m string, now time.Time) bool {
	entries := make([]timeWindowEntry, 0, len(list))
	anyWindowed := false
	for _, item := range list {
		e, err := parseTimeWindowEntry(item)
		if err != nil {
			continue // save-time validate rejects bad entries; defensive here
		}
		if e.hasWindow() {
			anyWindowed = true
		}
		entries = append(entries, e)
		if !e.hasWindow() && e.Model == m {
			return true
		}
	}
	if !anyWindowed {
		return false
	}
	// Windowed list: the time-selected model is reachable; other windowed
	// entries are held for their own windows.
	return pickTimeWindowModel(entries, now) == m
}

func matchModel(m string, mg *ModelGroup, req string, p *EffectiveProfile) bool {
	switch {
	case isTierName(req):
		// Tier virtual model: only the tier's member models match. The
		// tier index is stamped on the candidate by BuildCandidates so the
		// sort layer can put the admin's model order first.
		_, ok := tierCandidateModel(p, req, m)
		return ok
	case req == "" || req == "auto":
		return true
	case req == "auto-free":
		return hasTag(mg, "free")
	case strings.HasPrefix(req, "auto-"):
		capName := strings.TrimPrefix(req, "auto-")
		if p != nil && p.Routing != nil && p.Routing.ModelCapabilities != nil {
			if list, ok := p.Routing.ModelCapabilities[capName]; ok {
				if matchCapabilityEntry(list, m, timeWindowNowFunc()) {
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
	// A tier's own strategy can force cost-first ordering for its candidates
	// (tier strategy outranks the group-level ModelSelection).
	tierCostFirst := false
	if p != nil && p.Routing != nil && len(p.Routing.Tiers) > 0 {
		for _, tier := range p.Routing.Tiers {
			if tier != nil && tier.Strategy == "cost" {
				costFirst = true
				tierCostFirst = true
				break
			}
		}
	}
	sort.SliceStable(cs, func(i, j int) bool {
		// Tier requests: the tier Models-array order (TierIndex) is the
		// admin's model priority and outranks everything else — except a
		// tier-level cost strategy, which puts free models first inside that
		// tier. costFirst here also covers the group-level cost strategy and
		// the prefer_free request option, both of which rank free models
		// above the tier model order as well.
		if cs[i].TierIndex >= 0 && cs[j].TierIndex >= 0 && cs[i].TierIndex != cs[j].TierIndex && !tierCostFirst {
			return cs[i].TierIndex < cs[j].TierIndex
		}
		// Time-windowed "auto" capability lists: the active window's model
		// (WindowRank 0) ranks above the rest (1) but below tiers and the
		// cost strategies — an admin declaring windows wants time-based
		// dispatch to win over provider order, not over an explicit
		// cost-first intent.
		if !costFirst && cs[i].WindowRank != cs[j].WindowRank &&
			cs[i].WindowRank >= 0 && cs[j].WindowRank >= 0 {
			return cs[i].WindowRank < cs[j].WindowRank
		}
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
		// Configured tiers are callable virtual models exactly like aliases;
		// only the four whitelisted names are exposed (validateTiers enforces
		// the set, the guard here keeps /v1/models clean even if a bad config
		// somehow reached memory).
		for tierName := range prof.Routing.Tiers {
			if allowedTierNames[tierName] {
				set[tierName] = struct{}{}
			}
		}
	}

	// Drop the two hardcoded entries if no group declared any routing content —
	// otherwise we'd advertise auto/auto-free even when the gateway has no
	// aliases/capabilities configured at all.
	if len(set) <= 2 {
		// keep them only when at least one alias/capability/tier was collected
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
// declares modelAliases, modelCapabilities or tiers. Used by VirtualModelNames
// to decide whether the hardcoded "auto"/"auto-free" entries should be
// advertised.
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
		if len(prof.Routing.ModelCapabilities) > 0 || len(prof.Routing.ModelAliases) > 0 ||
			len(prof.Routing.Tiers) > 0 {
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
