package gateway

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setHour 指定测试用当前时刻（某天的 h 点 30 分）。
func setHour(t *testing.T, h int) {
	t.Helper()
	prev := timeWindowNowFunc
	fake := time.Date(2026, 10, 26, h, 30, 0, 0, time.Local)
	timeWindowNowFunc = func() time.Time { return fake }
	t.Cleanup(func() { timeWindowNowFunc = prev })
}

func TestParseTimeWindowEntry(t *testing.T) {
	e, err := parseTimeWindowEntry("[01-08]hy4-preview")
	require.NoError(t, err)
	require.True(t, e.hasWindow())
	require.Equal(t, [][2]int{{1, 8}}, e.Windows)
	assert.Equal(t, "hy4-preview", e.Model)

	// 1 位小时与 [01-08] 等价
	e, err = parseTimeWindowEntry("[1-8]hy4-preview")
	require.NoError(t, err)
	assert.Equal(t, [][2]int{{1, 8}}, e.Windows)

	// 裸条目
	e, err = parseTimeWindowEntry("step-5-preview")
	require.NoError(t, err)
	assert.False(t, e.hasWindow())
	assert.Equal(t, "step-5-preview", e.Model)

	// 非法：小时越界 / 缺模型名 / 空
	_, err = parseTimeWindowEntry("[00-24]x")
	assert.Error(t, err, "24 点越界必须报错")
	_, err = parseTimeWindowEntry("[01-08]")
	assert.Error(t, err, "缺模型名必须报错")
	_, err = parseTimeWindowEntry("  ")
	assert.Error(t, err, "空条目必须报错")
}

// TestParseTimeWindowEntry_MultiWindow 保护一个模型挂多个时间窗的解析：
// "[18-08][12-14]model" 是两个独立窗口共享同一模型，而非第二个窗被当成
// 模型名的一部分。
func TestParseTimeWindowEntry_MultiWindow(t *testing.T) {
	e, err := parseTimeWindowEntry("[18-08][12-14]deepseek-v4.1-flash")
	require.NoError(t, err)
	require.True(t, e.hasWindow())
	assert.Equal(t, [][2]int{{18, 8}, {12, 14}}, e.Windows)
	assert.Equal(t, "deepseek-v4.1-flash", e.Model)

	// 窗口间允许空格分隔（条目已在逗号分割后整体 TrimSpace）
	e, err = parseTimeWindowEntry("[01-08] [12-14]model-x")
	require.NoError(t, err)
	assert.Equal(t, [][2]int{{1, 8}, {12, 14}}, e.Windows)
	assert.Equal(t, "model-x", e.Model)

	// 三个窗口
	e, err = parseTimeWindowEntry("[00-02][06-08][22-23]m3")
	require.NoError(t, err)
	assert.Equal(t, [][2]int{{0, 2}, {6, 8}, {22, 23}}, e.Windows)

	// 多窗中任一小时越界整体报错
	_, err = parseTimeWindowEntry("[18-08][12-25]bad")
	assert.Error(t, err, "第二个窗口 25 点越界必须报错")

	// 残缺窗口（右括号缺失）不得被静默当模型名
	_, err = parseTimeWindowEntry("[18-08][12-14model")
	assert.Error(t, err, "残缺窗口必须报错")
}

func TestParseTimeWindowEntries(t *testing.T) {
	entries, err := parseTimeWindowEntries("[01-08]hy4-preview,[09-23]step-5-preview")
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, "hy4-preview", entries[0].Model)
	assert.Equal(t, "step-5-preview", entries[1].Model)

	// 混合裸条目
	entries, err = parseTimeWindowEntries("[09-23]step-5-preview,hy4-preview")
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.False(t, entries[1].hasWindow())

	// 逗号后空格 + 多窗条目（用户实际配置形态）
	entries, err = parseTimeWindowEntries("[01-08]hy4-preview, [18-08][12-14]deepseek-v4.1-flash")
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, [][2]int{{18, 8}, {12, 14}}, entries[1].Windows)

	// 单模型（无窗口）保持原行为
	entries, err = parseTimeWindowEntries("glm-5.3")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "glm-5.3", entries[0].Model)

	// 任一条目非法整体报错
	_, err = parseTimeWindowEntries("ok-model,[9-30]bad")
	assert.Error(t, err)
}

func TestHourInTimeWindow(t *testing.T) {
	assert.True(t, hourInTimeWindow(1, 1, 8), "起点含")
	assert.True(t, hourInTimeWindow(8, 1, 8), "终点含")
	assert.True(t, hourInTimeWindow(5, 1, 8))
	assert.False(t, hourInTimeWindow(0, 1, 8))
	assert.False(t, hourInTimeWindow(9, 1, 8))
	// 跨午夜：[22-06] 覆盖 22,23,0..6
	assert.True(t, hourInTimeWindow(23, 22, 6))
	assert.True(t, hourInTimeWindow(0, 22, 6))
	assert.True(t, hourInTimeWindow(6, 22, 6))
	assert.False(t, hourInTimeWindow(7, 22, 6))
	assert.False(t, hourInTimeWindow(21, 22, 6))
}

func TestPickTimeWindowModel_WindowHitFirstWins(t *testing.T) {
	entries, err := parseTimeWindowEntries("[01-08]hy4-preview,[09-23]step-5-preview")
	require.NoError(t, err)

	setHour(t, 3) // 1~8 点内
	assert.Equal(t, "hy4-preview", pickTimeWindowModel(entries, timeWindowNowFunc()))

	setHour(t, 12) // 9~23 点内
	assert.Equal(t, "step-5-preview", pickTimeWindowModel(entries, timeWindowNowFunc()))

	setHour(t, 0) // 都不命中 -> 取第一条（配置顺序当优先级）
	assert.Equal(t, "hy4-preview", pickTimeWindowModel(entries, timeWindowNowFunc()))
}

func TestPickTimeWindowModel_BareFallback(t *testing.T) {
	// 窗不命中时回落到第一条裸条目，即使它排在带窗条目之后
	entries, err := parseTimeWindowEntries("[09-23]step-5-preview,hy4-preview")
	require.NoError(t, err)

	setHour(t, 3)
	assert.Equal(t, "hy4-preview", pickTimeWindowModel(entries, timeWindowNowFunc()), "裸条目作为 fallback")

	setHour(t, 12)
	assert.Equal(t, "step-5-preview", pickTimeWindowModel(entries, timeWindowNowFunc()), "窗命中优先于裸条目")
}

func TestPickTimeWindowModel_OverlappingWindowsFirstListed(t *testing.T) {
	// 重叠窗口：命中多条时取配置顺序靠前的
	entries, err := parseTimeWindowEntries("[01-12]morning-model,[06-23]day-model")
	require.NoError(t, err)

	setHour(t, 9) // 两个窗口都命中
	assert.Equal(t, "morning-model", pickTimeWindowModel(entries, timeWindowNowFunc()))
}

// TestPickTimeWindowModel_MultiWindowAnyHit 保护多窗口条目的选路语义：
// 任一窗口命中当前小时即选中该条目。
func TestPickTimeWindowModel_MultiWindowAnyHit(t *testing.T) {
	// agent-reviewer 生产目标形态：[01-08]hy4-preview +
	// [18-08][12-14]deepseek-v4.1-flash
	entries, err := parseTimeWindowEntries("[01-08]hy4-preview,[18-08][12-14]deepseek-v4.1-flash")
	require.NoError(t, err)

	// 跨午夜窗 [18-08] 命中（18~23 点与 0 点；1~8 点与靠前的 [01-08]
	// 窗重叠，由配置顺序决定 → 归入下面的 hy4-preview 断言）
	for _, h := range []int{18, 20, 23, 0} {
		setHour(t, h)
		assert.Equal(t, "deepseek-v4.1-flash", pickTimeWindowModel(entries, timeWindowNowFunc()),
			"hour=%d 应命中 [18-08] 窗", h)
	}
	// 日间窗 [12-14] 命中
	for _, h := range []int{12, 13, 14} {
		setHour(t, h)
		assert.Equal(t, "deepseek-v4.1-flash", pickTimeWindowModel(entries, timeWindowNowFunc()),
			"hour=%d 应命中 [12-14] 窗", h)
	}
	// 1~8 点：两窗均命中（[18-08] 也覆盖这些小时），配置顺序靠前的
	// [01-08] 条目胜出
	for _, h := range []int{1, 4, 5, 8} {
		setHour(t, h)
		assert.Equal(t, "hy4-preview", pickTimeWindowModel(entries, timeWindowNowFunc()),
			"hour=%d 重叠窗取列表靠前", h)
	}
	// 9~11、15~17 点：两窗均不命中且无裸条目 → 配置顺序第一条
	for _, h := range []int{9, 10, 11, 15, 16, 17} {
		setHour(t, h)
		assert.Equal(t, "hy4-preview", pickTimeWindowModel(entries, timeWindowNowFunc()),
			"hour=%d 全不命中应按配置顺序取第一条", h)
	}
}

// TestMatchMapping_MultiWindowReplacement 端到端保护用户给定的
// agent-reviewer 条目（多窗 + 逗号后空格）在 model_name_mapping 中按小时
// 正确改写。
func TestMatchMapping_MultiWindowReplacement(t *testing.T) {
	rules := []string{"agent-reviewer:[01-08]hy4-preview, [18-08][12-14]deepseek-v4.1-flash"}

	setHour(t, 20)
	repl, ok := matchMapping(rules, "agent-reviewer")
	require.True(t, ok)
	assert.Equal(t, "deepseek-v4.1-flash", repl, "20 点应命中 [18-08] 跨午夜窗")

	setHour(t, 13)
	repl, ok = matchMapping(rules, "agent-reviewer")
	require.True(t, ok)
	assert.Equal(t, "deepseek-v4.1-flash", repl, "13 点应命中 [12-14] 日间窗")

	setHour(t, 4)
	repl, ok = matchMapping(rules, "agent-reviewer")
	require.True(t, ok)
	assert.Equal(t, "hy4-preview", repl, "4 点应命中靠前的 [01-08] 窗")

	setHour(t, 16)
	repl, ok = matchMapping(rules, "agent-reviewer")
	require.True(t, ok)
	assert.Equal(t, "hy4-preview", repl, "16 点全不命中应取配置顺序第一条")
}

func TestMatchMapping_TimeWindowReplacement(t *testing.T) {
	rules := []string{"agent-tester:[01-08]hy4-preview,[09-23]step-5-preview"}

	setHour(t, 5)
	repl, ok := matchMapping(rules, "agent-tester")
	require.True(t, ok)
	assert.Equal(t, "hy4-preview", repl)

	setHour(t, 20)
	repl, ok = matchMapping(rules, "agent-tester")
	require.True(t, ok)
	assert.Equal(t, "step-5-preview", repl)

	// 普通规则不受影响
	plain := []string{"Sonnet*: glm-5.2"}
	repl, ok = matchMapping(plain, "Sonnet-4")
	require.True(t, ok)
	assert.Equal(t, "glm-5.2", repl)
}

func TestValidateModelNameMappingTimeWindow(t *testing.T) {
	// 合法时间窗 replacement 通过校验（补齐 validate 必需字段）
	cfg := twoProviderConfig()
	cfg.LLMGateway["default"].Enabled = new(bool)
	*cfg.LLMGateway["default"].Enabled = true
	cfg.ModelNameMapping = []string{"agent-tester:[01-08]hy4-preview,[09-23]step-5-preview"}
	assert.NoError(t, validate(cfg))

	// 非法小时被保存时拦截
	cfg.ModelNameMapping = []string{"agent-tester:[01-25]bad-model"}
	assert.Error(t, validate(cfg), "小时 25 越界必须在保存时报错")

	// 残缺时间窗（缺模型名）拦截
	cfg.ModelNameMapping = []string{"agent-tester:[01-08]"}
	assert.Error(t, validate(cfg), "缺模型名必须在保存时报错")
}

// TestModelNameMappingMiddleware_TimeWindowRewrite 端到端保护中间件按时间
// 重写 body model 的行为。
func TestModelNameMappingMiddleware_TimeWindowRewrite(t *testing.T) {
	cfg := twoProviderConfig()
	cfg.ModelNameMapping = []string{"agent-tester:[01-08]hy4-preview,[09-23]step-5-preview"}
	setTestConfig(t, cfg)

	setHour(t, 5)
	c := newMappingContext(t, `{"model":"agent-tester","messages":[]}`)
	ModelNameMappingMiddleware()(c)
	assert.Equal(t, "hy4-preview", bodyModel(t, c))
	assert.Equal(t, "agent-tester", common.GetContextKeyString(c, constant.ContextKeyModelMappedFrom))

	setHour(t, 12)
	c = newMappingContext(t, `{"model":"agent-tester","messages":[]}`)
	ModelNameMappingMiddleware()(c)
	assert.Equal(t, "step-5-preview", bodyModel(t, c))
}

// TestBuildCandidates_AutoTimeWindowCapability 保护 auto 场景：带时间窗的
// modelCapabilities.auto 列表按当前小时把对应模型的候选排到最前。
func TestBuildCandidates_AutoTimeWindowCapability(t *testing.T) {
	cfg := twoProviderConfig()
	// provider order: zen(1) < ali(2)。zen 提供普通模型，ali 提供 day-model。
	cfg.LLMGateway["default"].Routing.ModelCapabilities = map[string][]string{
		"auto": {"[09-23]day-model", "night-model"},
	}
	cfg.Providers["zen"].Models = []*ModelGroup{
		{Name: "free", Model: "night-model", Order: 1, Enabled: true},
	}
	cfg.Providers["ali"].Models = []*ModelGroup{
		{Name: "coder", Model: "day-model", Order: 1, Enabled: true},
	}
	setTestConfig(t, cfg)
	prof := ResolveEffectiveProfile("default", "", "", "")

	// 白天（12 点）：day-model 的 WindowRank=0，越过 provider order 排最前
	setHour(t, 12)
	cands := BuildCandidates(prof, "auto", nil)
	require.Len(t, cands, 2)
	assert.Equal(t, "day-model", cands[0].ActualModel, "白天天窗模型必须排最前")
	assert.Equal(t, 0, cands[0].WindowRank)
	assert.Equal(t, "night-model", cands[1].ActualModel)
	assert.Equal(t, 1, cands[1].WindowRank)

	// 夜间（2 点）：无窗命中，裸条目 night-model 与 day-model 同为 rank 1，
	// provider order 决定 → zen(night-model) 在前
	setHour(t, 2)
	cands = BuildCandidates(prof, "auto", nil)
	require.Len(t, cands, 2)
	assert.Equal(t, "night-model", cands[0].ActualModel)
	assert.Equal(t, "day-model", cands[1].ActualModel)
}

// TestBuildCandidates_PlainCapabilityNoWindowRank 保护无时间窗的普通
// capability 列表不触发 WindowRank 排序（保持既有行为）。
func TestBuildCandidates_PlainCapabilityNoWindowRank(t *testing.T) {
	cfg := twoProviderConfig()
	setTestConfig(t, cfg)
	prof := ResolveEffectiveProfile("default", "", "", "")

	cands := BuildCandidates(prof, "auto", nil)
	require.NotEmpty(t, cands)
	for _, cand := range cands {
		assert.Equal(t, -1, cand.WindowRank, "普通列表不得打 WindowRank 标")
	}
}
