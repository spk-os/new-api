package gateway

import (
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// GatewayStrategyMiddleware identifies client/task, builds candidates, applies affinity,
// and locks the chosen channel id into the gin context for the existing distributor.
func GatewayStrategyMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !common.GatewayRoutingEnabled {
			c.Next()
			return
		}
		cfg := GetConfig()
		if cfg == nil {
			c.Next()
			return
		}
		groupName := pickGroupName(c)
		group := cfg.LLMGateway[groupName]
		if group == nil {
			c.Next()
			return
		}
		if group.Enabled != nil && !*group.Enabled {
			c.Next()
			return
		}

		clientId := identifyClient(c, group.Affinity)
		taskId := identifyTask(c, group.Affinity)
		opts := parseGatewayOptions(c)
		prof := ResolveEffectiveProfile(groupName, clientId, "", "")

		if clientId != "" {
			common.SetContextKey(c, constant.ContextKeyGatewayClientId, clientId)
		}
		if taskId != "" {
			common.SetContextKey(c, constant.ContextKeyGatewayTaskId, taskId)
		}

		requestedModel := requestedModelFrom(c)
		if requestedModel != "" {
			common.SetContextKey(c, constant.ContextKeyGatewayRequestedModel, requestedModel)
		}

		var plan *GatewayPlan
		var stickyHit bool
		if common.GatewayAffinityEnabled && prof.Affinity != nil && prof.Affinity.Enabled && stickyEnabled(opts, c) && clientId != "" && taskId != "" {
			if rec := GetAffinityBinding(clientId, taskId); rec != nil && !rec.Broken {
				if cand := candidateFromBinding(cfg, rec, prof); cand != nil {
					// Daily tier drift check: a circuit-broken sticky provider
					// must always be dropped, and a healthy one is dropped when
					// it trails the best reachable tier by more than one
					// (one-tier gap keeps the session sticky).
					if shouldBreakAffinityByDailyTier(cand, prof, requestedModel, opts) {
						BreakAffinityBinding(clientId, taskId)
					} else {
						plan = &GatewayPlan{
							Profile:     prof,
							Retry:       prof.ProviderRetry,
							Candidates:  []*Candidate{cand},
							AffinityHit: true,
							Affinity:    prof.Affinity,
						}
						// Tier requests keep the full tier candidate chain
						// behind the sticky candidate so the retry layer can
						// fail over across the tier's other models when the
						// bound provider fails (cross-model failover).
						appendTierFallbackCandidates(plan, prof, requestedModel)
						stickyHit = true
						if prof.Affinity.Binding != nil && prof.Affinity.Binding.ExtendOnAccess {
							ttl := time.Duration(prof.Affinity.Binding.TTL) * time.Second
							maxTTL := time.Duration(prof.Affinity.Binding.MaxTTL) * time.Second
							ExtendAffinityBinding(rec, ttl, maxTTL)
						}
					}
				}
			}
		}
		if plan == nil {
			cands := BuildCandidates(prof, requestedModel, opts)
			// Daily token-tier scheduling: reorder by today's usage tier
			// (outranks the static order) and drop circuit-broken providers.
			// When every candidate is broken, short-circuit with 403 — the
			// request must not fall through to the native channel path.
			cands, allBroken := ApplyDailyTierScheduling(cands)
			if allBroken {
				abortWithGatewayDailyTierBroken(c, requestedModel)
				return
			}
			// Session balancing: spread distinct sessions across equal-priority
			// providers (reorders the first equal-priority block in place).
			if sid := ExtractSessionId(c); sid != "" {
				SessionBalanceCandidates(cands, sid)
			}
			plan = &GatewayPlan{Profile: prof, Retry: prof.ProviderRetry, Candidates: cands, Affinity: prof.Affinity}
			if isTierName(requestedModel) && tierConfigured(prof, requestedModel) {
				plan.TierRequest = requestedModel
			}
		}

		var picked *Candidate
		for _, cand := range plan.Candidates {
			if IsProviderInCooldown(cand.ProviderId) {
				continue
			}
			if id, ok := resolveChannelId(cand); ok {
				cand.ChannelId = id
				picked = cand
				break
			}
		}
		if picked == nil {
			c.Next()
			return
		}

		common.SetContextKey(c, constant.ContextKeyGatewayPlan, plan)
		common.SetContextKey(c, constant.ContextKeyGatewayLockedChannelId, picked.ChannelId)
		common.SetContextKey(c, constant.ContextKeyGatewayActualModel, picked.ActualModel)

		// Rewrite request body model field so the entire downstream chain
		// (distributor, relay) uses the resolved actual model name.
		// This is critical for auto-{capability} models where the request
		// body contains a virtual model name like "auto-document".
		if requestedModel != picked.ActualModel {
			rewriteRequestBodyModel(c, picked.ActualModel)
		}

		if !stickyHit && common.GatewayAffinityEnabled && prof.Affinity != nil && prof.Affinity.Enabled && stickyEnabled(opts, c) && clientId != "" && taskId != "" {
			ttl := time.Duration(0)
			if prof.Affinity.Binding != nil {
				ttl = time.Duration(prof.Affinity.Binding.TTL) * time.Second
			}
			rec := &AffinityBindingRecord{
				ClientId: clientId, TaskId: taskId,
				ProviderId: picked.ProviderId, ProviderName: picked.ProviderName,
				ChannelId: picked.ChannelId, KeyIndex: picked.KeyIndex,
				ActualModel: picked.ActualModel, ModelGroup: picked.ModelGroup,
			}
			SetAffinityBinding(rec, ttl)
		}

		if prof.Affinity != nil && prof.Affinity.Failover != nil && prof.Affinity.Failover.NotifyClient {
			h := prof.Affinity.Failover.NotifyHeader
			if h == "" {
				h = "X-Gateway-Notice"
			}
			if !stickyHit {
				c.Header(h, "no-affinity")
			}
		}
		c.Next()
	}
}

func pickGroupName(c *gin.Context) string {
	if g := c.GetHeader("X-Gateway-Group"); g != "" {
		return g
	}
	return "default"
}

func identifyClient(c *gin.Context, aff *AffinityConfig) string {
	if v := c.GetHeader("X-Client-ID"); v != "" {
		return v
	}
	if aff == nil {
		return ""
	}
	for _, r := range aff.ClientIdentification {
		switch r.Source {
		case "header":
			if r.Key != "" {
				if v := c.GetHeader(r.Key); v != "" {
					return v
				}
			}
		case "userAgent":
			ua := c.GetHeader("User-Agent")
			for _, p := range r.Patterns {
				if p.Pattern != "" && strings.Contains(strings.ToLower(ua), strings.ToLower(p.Pattern)) {
					return p.ClientId
				}
			}
		}
	}
	return ""
}

func identifyTask(c *gin.Context, aff *AffinityConfig) string {
	if v := c.GetHeader("X-Task-ID"); v != "" {
		return v
	}
	if v := c.GetHeader("X-Conversation-ID"); v != "" {
		return v
	}
	if aff == nil {
		return ""
	}
	for _, r := range aff.TaskIdentification {
		if r.Source == "header" && r.Key != "" {
			if v := c.GetHeader(r.Key); v != "" {
				return v
			}
		}
	}
	return ""
}

func parseGatewayOptions(c *gin.Context) *GatewayOptions {
	bs := readRequestBodyBytes(c)
	if len(bs) == 0 {
		return nil
	}
	var raw struct {
		GatewayOptions *GatewayOptions `json:"gateway_options"`
	}
	if err := json.Unmarshal(bs, &raw); err != nil {
		return nil
	}
	return raw.GatewayOptions
}

func requestedModelFrom(c *gin.Context) string {
	if m, ok := common.GetContextKey(c, constant.ContextKeyOriginalModel); ok {
		if s, ok2 := m.(string); ok2 && s != "" {
			return s
		}
	}
	bs := readRequestBodyBytes(c)
	if len(bs) == 0 {
		return ""
	}
	if !gjson.ValidBytes(bs) {
		return ""
	}
	return gjson.GetBytes(bs, "model").String()
}

func rewriteRequestBodyModel(c *gin.Context, actualModel string) {
	bs := readRequestBodyBytes(c)
	if len(bs) == 0 {
		return
	}
	if !gjson.ValidBytes(bs) {
		return
	}
	modelVal := gjson.GetBytes(bs, "model")
	if !modelVal.Exists() {
		return
	}
	start := modelVal.Index
	end := start + len(modelVal.Raw)
	quoted := `"` + actualModel + `"`
	newBs := make([]byte, 0, len(bs)+len(quoted)-(end-start))
	newBs = append(newBs, bs[:start]...)
	newBs = append(newBs, quoted...)
	newBs = append(newBs, bs[end:]...)
	newStorage, err := common.CreateBodyStorage(newBs)
	if err != nil {
		return
	}
	c.Set(common.KeyBodyStorage, newStorage)
	c.Request.Body = io.NopCloser(newStorage)
}

func readRequestBodyBytes(c *gin.Context) []byte {
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil
	}
	bs, err := storage.Bytes()
	if err != nil {
		return nil
	}
	if _, seekErr := storage.Seek(0, io.SeekStart); seekErr != nil {
		return nil
	}
	c.Request.Body = io.NopCloser(storage)
	return bs
}

func stickyEnabled(opts *GatewayOptions, c *gin.Context) bool {
	if opts != nil && opts.Sticky != nil {
		return *opts.Sticky
	}
	if v := c.GetHeader("X-Sticky-Route"); v != "" {
		v = strings.ToLower(strings.TrimSpace(v))
		return v == "1" || v == "true" || v == "yes"
	}
	return true
}

// RewriteRequestBodyModel rewrites the request body's top-level "model" field
// to actualModel so the downstream chain (distributor, relay, upstream) uses
// the concrete model. Exported for the tier failover path in the relay retry
// loop, which must switch the body model when moving to the tier's next
// candidate. No-op when the body is missing, invalid JSON, or has no model
// field.
func RewriteRequestBodyModel(c *gin.Context, actualModel string) {
	rewriteRequestBodyModel(c, actualModel)
}

// tierConfigured reports whether the group's effective routing actually
// declares tierName. A request naming a tier that is not configured (e.g.
// "auto_max" with only auto_low defined) is not a tier request — it falls
// through to the alias/exact match semantics.
func tierConfigured(prof *EffectiveProfile, tierName string) bool {
	if prof == nil || prof.Routing == nil || prof.Routing.Tiers == nil {
		return false
	}
	t := prof.Routing.Tiers[tierName]
	return t != nil && len(t.Models) > 0
}

// appendTierFallbackCandidates extends an affinity-hit tier plan with the
// tier's full candidate chain: sticky candidate first, then every tier
// candidate not already covered by the sticky one (dedup by
// provider+model+channel). The retry layer walks this chain on failure, so a
// sticky hit on a tier request still has cross-model failover available.
func appendTierFallbackCandidates(plan *GatewayPlan, prof *EffectiveProfile, requestedModel string) {
	if plan == nil || !isTierName(requestedModel) || !tierConfigured(prof, requestedModel) {
		return
	}
	if plan.TierRequest == "" {
		plan.TierRequest = requestedModel
	}
	if len(plan.Candidates) == 0 {
		return
	}
	sticky := plan.Candidates[0]
	extra := BuildCandidates(prof, requestedModel, nil)
	seen := make(map[string]bool, len(extra)+1)
	seen[stickyKey(sticky)] = true
	for _, cand := range extra {
		if cand == nil {
			continue
		}
		k := stickyKey(cand)
		if seen[k] {
			continue
		}
		seen[k] = true
		plan.Candidates = append(plan.Candidates, cand)
	}
}

// stickyKey identifies a candidate for affinity-hit dedup: the same provider
// serving the same concrete model is the same failover unit.
func stickyKey(c *Candidate) string {
	return c.ProviderId + "\x00" + c.ActualModel
}

func candidateFromBinding(cfg *GatewayYaml, rec *AffinityBindingRecord, prof *EffectiveProfile) *Candidate {
	if cfg == nil || rec == nil {
		return nil
	}
	p := cfg.Providers[rec.ProviderId]
	if p == nil || !p.Enabled {
		return nil
	}
	for _, mg := range p.Models {
		if mg == nil || mg.Name != rec.ModelGroup || !mg.Enabled {
			continue
		}
		actual := rec.ActualModel
		if actual == "" {
			parts := splitCSV(mg.Model)
			if len(parts) > 0 {
				actual = parts[0]
			}
		}
		keys := splitCSV(p.Key)
		return &Candidate{
			ProviderId: rec.ProviderId, ProviderName: p.Name,
			KeyIndex: rec.KeyIndex, Keys: keys,
			ModelGroup: mg.Name, ActualModel: actual,
			IsFree: hasTag(mg, "free"),
		}
	}
	_ = prof
	return nil
}

func resolveChannelId(c *Candidate) (int, bool) {
	if c == nil {
		return 0, false
	}
	if c.ChannelId > 0 {
		return c.ChannelId, true
	}
	name := channelName(c.ProviderId, c.ModelGroup)
	var ch model.Channel
	if err := model.DB.Select("id, status").Where("name = ?", name).First(&ch).Error; err != nil {
		return 0, false
	}
	if ch.Status != common.ChannelStatusEnabled {
		return 0, false
	}
	return ch.Id, true
}
