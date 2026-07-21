package gateway

// ResolveEffectiveProfile merges five layers: default, group, provider, modelGroup, client.
func ResolveEffectiveProfile(group, clientId, providerId, modelGroupName string) *EffectiveProfile {
	cfg := GetConfig()
	if cfg == nil {
		return &EffectiveProfile{}
	}

	base := cloneProfile(cfg.LLMGateway["default"])

	if group != "" && group != "default" {
		if g, ok := cfg.LLMGateway[group]; ok && g != nil {
			mergeProfile(base, g)
		}
	}

	if providerId != "" && cfg.Providers != nil {
		if prov, ok := cfg.Providers[providerId]; ok && prov != nil {
			if prov.RateLimiting != nil {
				base.RateLimiting = prov.RateLimiting
			}
			if prov.Timeout != nil {
				base.Timeout = prov.Timeout
			}
		}
	}

	if mg := findModelGroup(providerId, modelGroupName); mg != nil {
		if mg.RateLimiting != nil {
			base.RateLimiting = mg.RateLimiting
		}
	}

	if c := lookupClientPolicy(group, clientId); c != nil {
		if c.Affinity != nil {
			if base.Affinity == nil {
				base.Affinity = cloneAffinity(c.Affinity)
			} else {
				mergeAffinity(base.Affinity, c.Affinity)
			}
		}
		if len(c.ProviderOrders) > 0 {
			base.ProviderOrderOverride = make(map[string]int, len(c.ProviderOrders))
			for k, v := range c.ProviderOrders {
				base.ProviderOrderOverride[k] = v
			}
		}
	}

	return base
}

func cloneProfile(g *StrategyGroup) *EffectiveProfile {
	p := &EffectiveProfile{}
	if g == nil {
		return p
	}
	if g.LLMCommon != nil {
		if g.LLMCommon.Timeout != nil {
			t := *g.LLMCommon.Timeout
			p.Timeout = &t
		}
		if g.LLMCommon.RateLimiting != nil {
			r := *g.LLMCommon.RateLimiting
			p.RateLimiting = &r
		}
		if g.LLMCommon.ProviderRetry != nil {
			p.ProviderRetry = cloneProviderRetry(g.LLMCommon.ProviderRetry)
		}
	}
	if g.Affinity != nil {
		p.Affinity = cloneAffinity(g.Affinity)
	}
	if g.Routing != nil {
		p.Routing = cloneRouting(g.Routing)
	}
	return p
}

func mergeProfile(base *EffectiveProfile, override *StrategyGroup) {
	if override == nil {
		return
	}
	if override.LLMCommon != nil {
		if override.LLMCommon.Timeout != nil {
			t := *override.LLMCommon.Timeout
			base.Timeout = &t
		}
		if override.LLMCommon.RateLimiting != nil {
			r := *override.LLMCommon.RateLimiting
			base.RateLimiting = &r
		}
		if override.LLMCommon.ProviderRetry != nil {
			base.ProviderRetry = cloneProviderRetry(override.LLMCommon.ProviderRetry)
		}
	}
	if override.Affinity != nil {
		if base.Affinity == nil {
			base.Affinity = cloneAffinity(override.Affinity)
		} else {
			mergeAffinity(base.Affinity, override.Affinity)
		}
	}
	if override.Routing != nil {
		if base.Routing == nil {
			base.Routing = cloneRouting(override.Routing)
		} else {
			mergeRouting(base.Routing, override.Routing)
		}
	}
}

func mergeAffinity(base, override *AffinityConfig) {
	if override == nil || base == nil {
		return
	}
	base.Enabled = override.Enabled
	if len(override.ClientIdentification) > 0 {
		base.ClientIdentification = override.ClientIdentification
	}
	if len(override.TaskIdentification) > 0 {
		base.TaskIdentification = override.TaskIdentification
	}
	if override.Binding != nil {
		if base.Binding == nil {
			b := *override.Binding
			base.Binding = &b
		} else {
			if override.Binding.TTL != 0 {
				base.Binding.TTL = override.Binding.TTL
			}
			if override.Binding.MaxTTL != 0 {
				base.Binding.MaxTTL = override.Binding.MaxTTL
			}
			if override.Binding.Storage != "" {
				base.Binding.Storage = override.Binding.Storage
			}
			if override.Binding.KeyPattern != "" {
				base.Binding.KeyPattern = override.Binding.KeyPattern
			}
			base.Binding.ExtendOnAccess = override.Binding.ExtendOnAccess
		}
	}
	if override.Failover != nil {
		if base.Failover == nil {
			f := *override.Failover
			base.Failover = &f
		} else {
			base.Failover.Enable = override.Failover.Enable
			if override.Failover.BreakAfterTimeout != 0 {
				base.Failover.BreakAfterTimeout = override.Failover.BreakAfterTimeout
			}
			if override.Failover.Priority != "" {
				base.Failover.Priority = override.Failover.Priority
			}
			if override.Failover.NotifyHeader != "" {
				base.Failover.NotifyHeader = override.Failover.NotifyHeader
			}
			base.Failover.NotifyClient = override.Failover.NotifyClient
		}
	}
}

func mergeRouting(base, override *RoutingConfig) {
	if override == nil || base == nil {
		return
	}
	if override.ModelSelection != nil {
		ms := *override.ModelSelection
		base.ModelSelection = &ms
	}
	if len(override.ModelAliases) > 0 {
		if base.ModelAliases == nil {
			base.ModelAliases = make(map[string]interface{}, len(override.ModelAliases))
		}
		for k, v := range override.ModelAliases {
			base.ModelAliases[k] = v
		}
	}
	if len(override.ModelCapabilities) > 0 {
		if base.ModelCapabilities == nil {
			base.ModelCapabilities = make(map[string][]string, len(override.ModelCapabilities))
		}
		for k, v := range override.ModelCapabilities {
			cp := make([]string, len(v))
			copy(cp, v)
			base.ModelCapabilities[k] = cp
		}
	}
}

func cloneAffinity(a *AffinityConfig) *AffinityConfig {
	if a == nil {
		return nil
	}
	out := *a
	if a.ClientIdentification != nil {
		out.ClientIdentification = append([]ClientIdentRule(nil), a.ClientIdentification...)
	}
	if a.TaskIdentification != nil {
		out.TaskIdentification = append([]TaskIdentRule(nil), a.TaskIdentification...)
	}
	if a.Binding != nil {
		b := *a.Binding
		out.Binding = &b
	}
	if a.Failover != nil {
		f := *a.Failover
		out.Failover = &f
	}
	return &out
}

func cloneRouting(r *RoutingConfig) *RoutingConfig {
	if r == nil {
		return nil
	}
	out := &RoutingConfig{}
	if r.ModelSelection != nil {
		ms := *r.ModelSelection
		out.ModelSelection = &ms
	}
	if r.ModelAliases != nil {
		out.ModelAliases = make(map[string]interface{}, len(r.ModelAliases))
		for k, v := range r.ModelAliases {
			out.ModelAliases[k] = v
		}
	}
	if r.ModelCapabilities != nil {
		out.ModelCapabilities = make(map[string][]string, len(r.ModelCapabilities))
		for k, v := range r.ModelCapabilities {
			cp := make([]string, len(v))
			copy(cp, v)
			out.ModelCapabilities[k] = cp
		}
	}
	return out
}

func cloneProviderRetry(p *ProviderRetry) *ProviderRetry {
	if p == nil {
		return nil
	}
	out := &ProviderRetry{Enabled: p.Enabled}
	if p.Model != nil {
		m := *p.Model
		if p.Model.BackoffIntervals != nil {
			m.BackoffIntervals = append([]int(nil), p.Model.BackoffIntervals...)
		}
		if p.Model.RetryableStatusCodes != nil {
			m.RetryableStatusCodes = append([]int(nil), p.Model.RetryableStatusCodes...)
		}
		if p.Model.NonRetryableStatusCodes != nil {
			m.NonRetryableStatusCodes = append([]int(nil), p.Model.NonRetryableStatusCodes...)
		}
		out.Model = &m
	}
	if p.Provider != nil {
		ps := *p.Provider
		out.Provider = &ps
	}
	if p.Global != nil {
		g := *p.Global
		out.Global = &g
	}
	return out
}

func findModelGroup(providerId, modelGroupName string) *ModelGroup {
	if providerId == "" || modelGroupName == "" {
		return nil
	}
	cfg := GetConfig()
	if cfg == nil || cfg.Providers == nil {
		return nil
	}
	prov, ok := cfg.Providers[providerId]
	if !ok || prov == nil {
		return nil
	}
	for _, mg := range prov.Models {
		if mg != nil && mg.Name == modelGroupName {
			return mg
		}
	}
	return nil
}

func lookupClientPolicy(group, clientId string) *ClientPolicy {
	if group == "" || clientId == "" {
		return nil
	}
	cfg := GetConfig()
	if cfg == nil {
		return nil
	}
	g, ok := cfg.LLMGateway[group]
	if !ok || g == nil || g.Client == nil {
		return nil
	}
	return g.Client[clientId]
}
