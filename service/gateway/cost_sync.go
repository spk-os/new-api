package gateway

import (
	"encoding/json"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
)

// SyncCostFromGateway adapts the SyncCost function to accept a *GatewayYaml
// (matching the syncCost hook signature used by SaveConfig).
func SyncCostFromGateway(cfg *GatewayYaml) (*CostSyncResult, error) {
	if cfg == nil {
		return &CostSyncResult{}, nil
	}
	return SyncCost(cfg.Cost)
}

func SyncCost(cost *CostConfig) (*CostSyncResult, error) {
	if cost == nil || cost.Models == nil {
		return &CostSyncResult{}, nil
	}

	modelRatio := loadCurrentModelRatio()
	completionRatio := loadCurrentCompletionRatio()

	// New API's internal base: 1 ratio unit = $0.002/1K tokens (USD).
	// For CNY-priced models, use the RMB constant (USD/USD2RMB) so the
	// resulting ratio is consistent with model_ratio.go's CNY entries.
	// Dividing CNY values by 0.002 (the USD base) would inflate ratios
	// by USD2RMB (7.3x), causing incorrect billing display.
	isCNY := cost.Currency == "CNY" || cost.Currency == "RMB"

	for name, mc := range cost.Models {
		if mc == nil || name == "" {
			continue
		}
		if isCNY {
			modelRatio[name] = mc.InputPer1kTokens * ratio_setting.RMB
		} else {
			modelRatio[name] = mc.InputPer1kTokens / 0.002
		}
		if mc.InputPer1kTokens > 0 {
			completionRatio[name] = mc.OutputPer1kTokens / mc.InputPer1kTokens
		} else {
			completionRatio[name] = 0
		}
	}

	if err := ratio_setting.UpdateModelRatioByJSONString(marshal(modelRatio)); err != nil {
		return nil, err
	}
	if err := ratio_setting.UpdateCompletionRatioByJSONString(marshal(completionRatio)); err != nil {
		return nil, err
	}
	if err := model.UpdateOption("ModelRatio", marshal(modelRatio)); err != nil {
		return nil, err
	}
	if err := model.UpdateOption("CompletionRatio", marshal(completionRatio)); err != nil {
		return nil, err
	}

	return &CostSyncResult{Updated: len(cost.Models)}, nil
}

func loadCurrentModelRatio() map[string]float64 {
	cp := ratio_setting.GetModelRatioCopy()
	if cp == nil {
		return map[string]float64{}
	}
	return cp
}

func loadCurrentCompletionRatio() map[string]float64 {
	cp := ratio_setting.GetCompletionRatioCopy()
	if cp == nil {
		return map[string]float64{}
	}
	return cp
}

func marshal(m map[string]float64) string {
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}
