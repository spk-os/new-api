package gateway

import (
	"errors"
	"strings"

	"github.com/QuantumNous/new-api/common"
	channelconstant "github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
)

const GatewayTag = "llm_gateway"

func SyncChannels(cfg *GatewayYaml) (*SyncResult, error) {
	if cfg == nil {
		return nil, errors.New("nil cfg")
	}
	res := &SyncResult{}
	if cfg.Providers == nil {
		return res, nil
	}

	groups := strings.Join(keysOf(cfg.LLMGateway), ",")
	desired := map[string]struct{}{}

	for _, p := range cfg.Providers {
		if p == nil || strings.TrimSpace(p.Name) == "" {
			continue
		}
		desired[p.Name] = struct{}{}
		modelsCSV := strings.Join(expandAllModels(p), ",")

		status := common.ChannelStatusEnabled
		if !p.Enabled {
			status = common.ChannelStatusManuallyDisabled
		}

		ch, err := getChannelByName(p.Name)
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			res.Disabled = append(res.Disabled, p.Name+" (lookup failed: "+err.Error()+")")
			continue
		}

	chType := p.ChannelType
	if chType == 0 {
		chType = 1
	}
	baseURL := resolveBaseURL(p.URL, chType)

	if ch == nil {
		newCh := &model.Channel{
			Name:     p.Name,
			Type:     chType,
			Key:      p.Key,
			Models:   modelsCSV,
			Group:    groups,
			Status:   status,
			BaseURL:  baseURL,
		}
		newCh.SetTag(GatewayTag)
		priority := int64(p.Order)
		newCh.Priority = &priority

		if err := newCh.Insert(); err != nil {
			res.Disabled = append(res.Disabled, p.Name+" (create failed: "+err.Error()+")")
			continue
		}
		res.Created = append(res.Created, p.Name)
		continue
	}

	ch.Key = p.Key
	ch.Models = modelsCSV
	ch.Group = groups
	ch.Status = status
	ch.SetTag(GatewayTag)
	ch.Type = chType
	ch.BaseURL = baseURL
	priority := int64(p.Order)
	ch.Priority = &priority

		if err := ch.Update(); err != nil {
			res.Disabled = append(res.Disabled, p.Name+" (update failed: "+err.Error()+")")
			continue
		}
		res.Updated = append(res.Updated, p.Name)
	}

	managed, err := findManagedChannels()
	if err == nil {
		for _, ch := range managed {
			if _, keep := desired[ch.Name]; keep {
				continue
			}
			if ch.Status == common.ChannelStatusManuallyDisabled {
				continue
			}
			ch.Status = common.ChannelStatusManuallyDisabled
			if err := ch.Update(); err == nil {
				res.Disabled = append(res.Disabled, ch.Name)
			}
		}
	}

	model.InitChannelCache()
	return res, nil
}

func expandAllModels(p *Provider) []string {
	if p == nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, mg := range p.Models {
		if mg == nil {
			continue
		}
		for _, m := range splitCSV(mg.Model) {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// resolveBaseURL determines the channel BaseURL based on the provider URL and channel type.
// For OpenAI-compatible (type=1): strips trailing /v1 since relay re-adds /v1/chat/completions.
// For types with ChannelSpecialBases (VolcEngine=45, Zhipu_v4=26): uses the special plan key if matched,
// otherwise uses the raw URL as-is (adaptor constructs the full path internally).
// For Ali (type=17): uses raw URL as-is (Ali adaptor constructs /compatible-mode/v1/... internally).
func resolveBaseURL(rawURL string, chType int) *string {
	if rawURL == "" {
		return nil
	}
	if _, hasSpecial := channelconstant.ChannelSpecialBases[rawURL]; hasSpecial {
		return &rawURL
	}
	if chType == 1 {
		normalized := normalizeGatewayURL(rawURL)
		return &normalized
	}
	return &rawURL
}

func normalizeGatewayURL(raw string) string {
	u := strings.TrimRight(raw, "/")
	if strings.HasSuffix(u, "/v1") {
		u = strings.TrimSuffix(u, "/v1")
	}
	return u
}

func keysOf(m map[string]*StrategyGroup) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func channelName(providerId, _ string) string {
	cfg := GetConfig()
	if cfg == nil || cfg.Providers == nil {
		return providerId
	}
	if p, ok := cfg.Providers[providerId]; ok && p != nil && p.Name != "" {
		return p.Name
	}
	return providerId
}

func getChannelByName(name string) (*model.Channel, error) {
	var ch model.Channel
	err := model.DB.Where("name = ?", name).First(&ch).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &ch, nil
}

func findManagedChannels() ([]*model.Channel, error) {
	var channels []*model.Channel
	err := model.DB.Where("tag = ?", GatewayTag).Find(&channels).Error
	if err != nil {
		return nil, err
	}
	return channels, nil
}
