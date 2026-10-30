package gateway

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildCandidates_AutoFreeTimeWindow 保护 auto-free 场景：modelCapabilities.free
// 列表带时间窗时，auto-free 请求按当前小时解析窗模型/裸条目，而不是只做
// free tag 匹配。窗外小时回落到裸条目。
func TestBuildCandidates_AutoFreeTimeWindow(t *testing.T) {
	cfg := twoProviderConfig()
	// 用户目标形态：free: ['[01-07]hy4-preview', 'hy3']
	cfg.LLMGateway["default"].Routing.ModelCapabilities = map[string][]string{
		"free": {"[01-07]hy4-preview", "hy3"},
	}
	// zen 提供窗模型，ali 提供裸条目模型（provider order: zen 1 < ali 2）
	cfg.Providers["zen"].Models = []*ModelGroup{
		{Name: "free", Model: "hy4-preview", Order: 1, Enabled: true},
	}
	cfg.Providers["ali"].Models = []*ModelGroup{
		{Name: "coder", Model: "hy3", Order: 1, Enabled: true},
	}
	setTestConfig(t, cfg)
	prof := ResolveEffectiveProfile("default", "", "", "")

	// 窗内（3 点）：auto-free 命中窗模型 hy4-preview
	setHour(t, 3)
	cands := BuildCandidates(prof, "auto-free", nil)
	require.NotEmpty(t, cands, "窗内小时必须解析出候选")
	assert.Equal(t, "hy4-preview", cands[0].ActualModel, "窗内 auto-free 首选必须是窗模型")

	// 窗外（12 点）：回落到裸条目 hy3
	setHour(t, 12)
	cands = BuildCandidates(prof, "auto-free", nil)
	require.NotEmpty(t, cands, "窗外小时必须回落到裸条目")
	assert.Equal(t, "hy3", cands[0].ActualModel, "窗外 auto-free 首选必须是裸条目")
}

// TestMatchModel_AutoFreeNoCapabilityFallback 保护无 modelCapabilities.free
// 配置时 auto-free 回落到 free tag 匹配的既有行为。
func TestMatchModel_AutoFreeNoCapabilityFallback(t *testing.T) {
	cfg := twoProviderConfig()
	setTestConfig(t, cfg)
	prof := ResolveEffectiveProfile("default", "", "", "")
	mg := &ModelGroup{Tags: []string{"free"}}
	assert.True(t, matchModel("any", mg, "auto-free", prof), "无 capability 配置时仍须按 free tag 匹配")
}
