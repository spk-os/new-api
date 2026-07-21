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

		var plan *GatewayPlan
		var stickyHit bool
		if common.GatewayAffinityEnabled && prof.Affinity != nil && prof.Affinity.Enabled && stickyEnabled(opts, c) && clientId != "" && taskId != "" {
			if rec := GetAffinityBinding(clientId, taskId); rec != nil && !rec.Broken {
				if cand := candidateFromBinding(cfg, rec, prof); cand != nil {
					plan = &GatewayPlan{
						Profile:     prof,
						Retry:       prof.ProviderRetry,
						Candidates:  []*Candidate{cand},
						AffinityHit: true,
						Affinity:    prof.Affinity,
					}
					stickyHit = true
					if prof.Affinity.Binding != nil && prof.Affinity.Binding.ExtendOnAccess {
						ttl := time.Duration(prof.Affinity.Binding.TTL) * time.Second
						maxTTL := time.Duration(prof.Affinity.Binding.MaxTTL) * time.Second
						ExtendAffinityBinding(rec, ttl, maxTTL)
					}
				}
			}
		}
		if plan == nil {
			cands := BuildCandidates(prof, requestedModel, opts)
			plan = &GatewayPlan{Profile: prof, Retry: prof.ProviderRetry, Candidates: cands, Affinity: prof.Affinity}
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
