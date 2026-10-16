package gateway

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetGlobalState(t *testing.T) {
	t.Helper()
	dbReadOption = nil
	dbWriteOption = nil
	syncChannels = nil
	syncCost = nil
	globalConfig.Store(nil)
	configMu.Lock()
	configPath = defaultConfigPath
	lastFileMod = time.Time{}
	configMu.Unlock()
}

const validYAML = `
llm_gateway:
  default:
    version: "1.0"
    enabled: true
    llmCommon:
      timeout:
        connectTimeout: 5
        readTimeout: 60
        streamIdleTimeout: 30
      rateLimiting:
        enabled: true
        concurrency: 10
        windowRate: "1h-100"
        overLimitStrategy: queue
        queueTimeout: 5
      providerRetry:
        enabled: true
        model:
          maxRetries: 3
          backoffIntervals: [1, 2, 4]
          retryOnTimeout: true
    affinity:
      enabled: true
      binding:
        ttl: 3600
        maxTTL: 86400
        storage: memory
      failover:
        enable: true
        priority: sameModel
    routing:
      modelSelection:
        strategy: priority
providers:
  zen:
    name: zen
    url: https://api.zen.example/v1
    key: sk-aaa,sk-bbb
    order: 1
    enabled: true
    models:
      - name: chat-default
        model: zen-large,zen-medium
        order: 1
        enabled: true
`

func TestParseAndValidate_Valid(t *testing.T) {
	resetGlobalState(t)
	cfg, err := ParseAndValidate(validYAML)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.NotNil(t, cfg.LLMGateway["default"])
	assert.Equal(t, "1.0", cfg.LLMGateway["default"].Version)
	require.NotNil(t, cfg.LLMGateway["default"].Enabled)
	assert.True(t, *cfg.LLMGateway["default"].Enabled)
	require.Contains(t, cfg.Providers, "zen")
	assert.Equal(t, "zen", cfg.Providers["zen"].Name)
	assert.Equal(t, "sk-aaa,sk-bbb", cfg.Providers["zen"].Key)
	require.Len(t, cfg.Providers["zen"].Models, 1)
	assert.Equal(t, 1, cfg.Providers["zen"].Models[0].Order)
}

func TestParseAndValidate_Empty(t *testing.T) {
	resetGlobalState(t)
	_, err := ParseAndValidate("")
	assert.Error(t, err)
	_, err = ParseAndValidate("   \n\t  ")
	assert.Error(t, err)
}

func TestParseAndValidate_AlarmDefaultsWhenOmitted(t *testing.T) {
	resetGlobalState(t)
	cfg, err := ParseAndValidate(validYAML)
	require.NoError(t, err)
	require.NotNil(t, cfg.Alarm)
	assert.Equal(t, int64(defaultAlarmOneReqTokens), cfg.Alarm.OneReqTokens)
	assert.Equal(t, int64(defaultAlarmTokensPerMinute), cfg.Alarm.TokensPerMinute)
	assert.Equal(t, int64(defaultAlarmIntervalMs), cfg.Alarm.Interval)
	assert.Empty(t, cfg.Alarm.FeishuWebhook, "empty webhook must stay empty so runtime falls back to the compiled-in default")
}

func TestParseAndValidate_AlarmExplicitValuesPreserved(t *testing.T) {
	resetGlobalState(t)
	src := validYAML + `
alarm:
  one-req-tokens: 500000
  tokens-per-minute: 5000000
  interval: 300000
  feishu-webhook: https://example.com/hook
`
	cfg, err := ParseAndValidate(src)
	require.NoError(t, err)
	require.NotNil(t, cfg.Alarm)
	assert.Equal(t, int64(500000), cfg.Alarm.OneReqTokens)
	assert.Equal(t, int64(5000000), cfg.Alarm.TokensPerMinute)
	assert.Equal(t, int64(300000), cfg.Alarm.Interval)
	assert.Equal(t, "https://example.com/hook", cfg.Alarm.FeishuWebhook)
}

func TestParseAndValidate_AlarmLoopDefaultsWhenOmitted(t *testing.T) {
	resetGlobalState(t)
	cfg, err := ParseAndValidate(validYAML)
	require.NoError(t, err)
	require.NotNil(t, cfg.Alarm)
	assert.Equal(t, int64(defaultAlarmLoopMinTokens), cfg.Alarm.LoopMinTokens)
	assert.Equal(t, defaultAlarmLoopSizeDiffPct, cfg.Alarm.LoopSizeDiffPct)
	assert.Equal(t, int64(defaultAlarmLoopMaxRespMs), cfg.Alarm.LoopMaxRespMs)
	assert.Equal(t, int64(defaultAlarmLoopSleepMs), cfg.Alarm.LoopSleepMs)
	assert.Equal(t, int64(defaultAlarmLoopWindowMs), cfg.Alarm.LoopWindowMs)
	assert.Equal(t, defaultAlarmLoopMinHits, cfg.Alarm.LoopMinHits)
}

func TestParseAndValidate_AlarmLoopExplicitValuesPreserved(t *testing.T) {
	resetGlobalState(t)
	src := validYAML + `
alarm:
  loop-min-tokens: 80000
  loop-size-diff-pct: 5
  loop-max-resp-ms: 3000
  loop-sleep-ms: 600000
  loop-window-ms: 120000
  loop-min-hits: 5
`
	cfg, err := ParseAndValidate(src)
	require.NoError(t, err)
	require.NotNil(t, cfg.Alarm)
	assert.Equal(t, int64(80000), cfg.Alarm.LoopMinTokens)
	assert.Equal(t, 5.0, cfg.Alarm.LoopSizeDiffPct)
	assert.Equal(t, int64(3000), cfg.Alarm.LoopMaxRespMs)
	assert.Equal(t, int64(600000), cfg.Alarm.LoopSleepMs)
	assert.Equal(t, int64(120000), cfg.Alarm.LoopWindowMs)
	assert.Equal(t, 5, cfg.Alarm.LoopMinHits)
}

// TestParseAndValidate_AlarmLoopFractionalSizeDiffPct locks the int→float64
// field fix: a fractional 0.2 must survive parsing instead of being silently
// truncated to 0 (then masked back to the default 1 by the zero-fill). Under
// the old int field, yaml parsed 0.2 into int as 0 and the operator's tighter
// threshold was lost — the runtime used 1%.
func TestParseAndValidate_AlarmLoopFractionalSizeDiffPct(t *testing.T) {
	resetGlobalState(t)
	src := validYAML + `
alarm:
  loop-size-diff-pct: 0.2
`
	cfg, err := ParseAndValidate(src)
	require.NoError(t, err)
	require.NotNil(t, cfg.Alarm)
	assert.Equal(t, 0.2, cfg.Alarm.LoopSizeDiffPct, "fractional threshold must be preserved, not truncated to 0")
}

func TestParseAndValidate_MissingLLMGateway(t *testing.T) {
	resetGlobalState(t)
	_, err := ParseAndValidate("providers:\n  zen:\n    name: zen\n    url: https://x\n    key: sk-1\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "llm_gateway is required")
}

func TestParseAndValidate_MissingDefaultGroup(t *testing.T) {
	resetGlobalState(t)
	src := "llm_gateway:\n  quality:\n    version: \"1\"\n    enabled: true\nproviders:\n  zen:\n    name: zen\n    url: https://x\n    key: sk-1\n    models: [{name: m, model: a, order: 1}]\n"
	_, err := ParseAndValidate(src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "default")
}

func TestParseAndValidate_DefaultMissingVersion(t *testing.T) {
	resetGlobalState(t)
	src := "llm_gateway:\n  default:\n    enabled: true\nproviders:\n  zen:\n    name: zen\n    url: https://x\n    key: sk-1\n    models: [{name: m, model: a, order: 1}]\n"
	_, err := ParseAndValidate(src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "version is required")
}

func TestParseAndValidate_DefaultMissingEnabled(t *testing.T) {
	resetGlobalState(t)
	src := "llm_gateway:\n  default:\n    version: \"1\"\nproviders:\n  zen:\n    name: zen\n    url: https://x\n    key: sk-1\n    models: [{name: m, model: a, order: 1}]\n"
	_, err := ParseAndValidate(src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "enabled is required")
}

func TestParseAndValidate_ProviderMissingFields(t *testing.T) {
	resetGlobalState(t)
	base := "llm_gateway:\n  default: {version: \"1\", enabled: true}\nproviders:\n  zen:\n"
	cases := map[string]string{
		"missing name": base + "    url: https://x\n    key: sk-1\n    models: [{name: m, model: a, order: 1}]\n",
		"missing url":  base + "    name: zen\n    key: sk-1\n    models: [{name: m, model: a, order: 1}]\n",
		"missing key":  base + "    name: zen\n    url: https://x\n    models: [{name: m, model: a, order: 1}]\n",
	}
	for label, text := range cases {
		t.Run(label, func(t *testing.T) {
			_, err := ParseAndValidate(text)
			assert.Error(t, err, label)
		})
	}
}

func TestParseAndValidate_ProvidersRequired(t *testing.T) {
	resetGlobalState(t)
	_, err := ParseAndValidate("llm_gateway:\n  default: {version: \"1\", enabled: true}\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "providers is required")
}

func TestParseAndValidate_ModelGroupRules(t *testing.T) {
	resetGlobalState(t)
	base := "llm_gateway:\n  default: {version: \"1\", enabled: true}\nproviders:\n  zen:\n    name: zen\n    url: https://x\n    key: sk-1\n"
	cases := map[string]string{
		"missing name":   base + "    models: [{model: a, order: 1}]\n",
		"empty model":    base + "    models: [{name: m, model: \"\", order: 1}]\n",
		"order zero":     base + "    models: [{name: m, model: a, order: 0}]\n",
		"order negative": base + "    models: [{name: m, model: a, order: -1}]\n",
	}
	for label, text := range cases {
		t.Run(label, func(t *testing.T) {
			_, err := ParseAndValidate(text)
			assert.Error(t, err, label)
		})
	}
}

func TestParseAndValidate_EnumOverLimit(t *testing.T) {
	resetGlobalState(t)
	src := "llm_gateway:\n  default:\n    version: \"1\"\n    enabled: true\n    llmCommon:\n      rateLimiting:\n        enabled: true\n        overLimitStrategy: bogus\nproviders:\n  zen:\n    name: zen\n    url: https://x\n    key: sk-1\n    models: [{name: m, model: a, order: 1}]\n"
	_, err := ParseAndValidate(src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overLimitStrategy")
}

func TestParseAndValidate_EnumFailoverPriority(t *testing.T) {
	resetGlobalState(t)
	src := "llm_gateway:\n  default:\n    version: \"1\"\n    enabled: true\n    affinity:\n      enabled: true\n      binding: {ttl: 60}\n      failover: {enable: true, priority: nonsense}\nproviders:\n  zen:\n    name: zen\n    url: https://x\n    key: sk-1\n    models: [{name: m, model: a, order: 1}]\n"
	_, err := ParseAndValidate(src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "priority")
}

func TestParseAndValidate_EnumRoutingStrategy(t *testing.T) {
	resetGlobalState(t)
	src := "llm_gateway:\n  default:\n    version: \"1\"\n    enabled: true\n    routing:\n      modelSelection: {strategy: random}\nproviders:\n  zen:\n    name: zen\n    url: https://x\n    key: sk-1\n    models: [{name: m, model: a, order: 1}]\n"
	_, err := ParseAndValidate(src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "modelSelection.strategy")
}

func TestParseAndValidate_AffinityTTLZero(t *testing.T) {
	resetGlobalState(t)
	src := "llm_gateway:\n  default:\n    version: \"1\"\n    enabled: true\n    affinity:\n      enabled: true\n      binding: {ttl: 0}\nproviders:\n  zen:\n    name: zen\n    url: https://x\n    key: sk-1\n    models: [{name: m, model: a, order: 1}]\n"
	_, err := ParseAndValidate(src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ttl")
}

func TestParseAndValidate_RetryBackoffMissing(t *testing.T) {
	resetGlobalState(t)
	src := "llm_gateway:\n  default:\n    version: \"1\"\n    enabled: true\n    llmCommon:\n      providerRetry:\n        enabled: true\n        model: {maxRetries: 3, backoffIntervals: []}\nproviders:\n  zen:\n    name: zen\n    url: https://x\n    key: sk-1\n    models: [{name: m, model: a, order: 1}]\n"
	_, err := ParseAndValidate(src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backoffIntervals")
}

func TestParseAndValidate_TypoNormalization(t *testing.T) {
	resetGlobalState(t)
	src := "llm_gateway:\n  default:\n    version: \"1\"\n    enabled: true\n    affinity:\n      enabled: true\n      binding: {ttl: 60}\n      failover: {enable: true, priority: samepPovider}\nproviders:\n  zen:\n    name: zen\n    url: https://x\n    key: sk-1\n    models:\n      - name: payed-tier\n        model: a\n        order: 1\n        tags: [payed, free]\n"
	cfg, err := ParseAndValidate(src)
	require.NoError(t, err)
	assert.Equal(t, "sameProvider", cfg.LLMGateway["default"].Affinity.Failover.Priority)
	assert.Equal(t, "paid-tier", cfg.Providers["zen"].Models[0].Name)
	assert.Equal(t, []string{"paid", "free"}, cfg.Providers["zen"].Models[0].Tags)
}

func TestParseAndValidate_TypoCaseInsensitive(t *testing.T) {
	resetGlobalState(t)
	src := "llm_gateway:\n  default:\n    version: \"1\"\n    enabled: true\n    affinity:\n      enabled: true\n      binding: {ttl: 60}\n      failover: {enable: true, priority: sameprovider}\nproviders:\n  zen:\n    name: zen\n    url: https://x\n    key: sk-1\n    models: [{name: m, model: a, order: 1}]\n"
	cfg, err := ParseAndValidate(src)
	require.NoError(t, err)
	assert.Equal(t, "sameProvider", cfg.LLMGateway["default"].Affinity.Failover.Priority)
}

func TestLoadConfig_FromDB(t *testing.T) {
	resetGlobalState(t)
	dbReadOption = func(key string) (string, bool) {
		if key == OptGatewayRoutingConfigYaml {
			return validYAML, true
		}
		return "", false
	}
	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "zen", cfg.Providers["zen"].Name)
}

func TestLoadConfig_DBInvalidYAML(t *testing.T) {
	resetGlobalState(t)
	dbReadOption = func(key string) (string, bool) {
		if key == OptGatewayRoutingConfigYaml {
			return "llm_gateway:\n  bogus: stuff\n", true
		}
		return "", false
	}
	_, err := LoadConfig()
	assert.Error(t, err)
}

func TestLoadConfig_FallbackToFile(t *testing.T) {
	resetGlobalState(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "gw.yaml")
	require.NoError(t, os.WriteFile(path, []byte(validYAML), 0o600))
	SetConfigPath(path)
	dbReadOption = func(key string) (string, bool) { return "", false }

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "zen", cfg.Providers["zen"].Name)
}

func TestLoadConfig_DBPathOverride(t *testing.T) {
	resetGlobalState(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.yaml")
	require.NoError(t, os.WriteFile(path, []byte(validYAML), 0o600))
	dbReadOption = func(key string) (string, bool) {
		if key == OptGatewayRoutingConfigPath {
			return path, true
		}
		return "", false
	}

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, path, GetConfigPath())
}

func TestLoadConfig_BothEmpty(t *testing.T) {
	resetGlobalState(t)
	SetConfigPath(filepath.Join(t.TempDir(), "absent.yaml"))
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Nil(t, cfg)
}

func TestSaveConfig_FullFlow(t *testing.T) {
	resetGlobalState(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "gw.yaml")
	SetConfigPath(path)

	written := map[string]string{}
	dbWriteOption = func(key, value string) error {
		written[key] = value
		return nil
	}
	syncChannelsCalls := 0
	syncChannels = func(cfg *GatewayYaml) (*SyncResult, error) {
		syncChannelsCalls++
		return &SyncResult{Created: []string{"zen"}}, nil
	}
	syncCostCalls := 0
	syncCost = func(cfg *GatewayYaml) (*CostSyncResult, error) {
		syncCostCalls++
		return &CostSyncResult{Updated: 7}, nil
	}

	res, err := SaveConfig(validYAML)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.Applied)
	assert.Equal(t, []string{"zen"}, res.ChannelsCreated)
	assert.Equal(t, 7, res.PricingUpdated)
	assert.NotEmpty(t, res.EffectiveAt)
	assert.Empty(t, res.Errors)
	assert.Equal(t, validYAML, written[OptGatewayRoutingConfigYaml])

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, validYAML, string(data))

	cfg := GetConfig()
	require.NotNil(t, cfg)
	assert.Equal(t, "zen", cfg.Providers["zen"].Name)
	assert.Equal(t, 1, syncChannelsCalls)
	assert.Equal(t, 1, syncCostCalls)
}

func TestSaveConfig_ValidationFailureNoWrite(t *testing.T) {
	resetGlobalState(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "gw.yaml")
	SetConfigPath(path)

	dbCalled := false
	dbWriteOption = func(key, value string) error {
		dbCalled = true
		return nil
	}

	_, err := SaveConfig("llm_gateway:\n  notdefault: {version: x, enabled: true}\n")
	assert.Error(t, err)
	assert.False(t, dbCalled, "DB must not be written when validation fails")
	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "file must not be created when validation fails")
	assert.Nil(t, GetConfig())
}

func TestSaveConfig_SyncErrorsCollected(t *testing.T) {
	resetGlobalState(t)
	dir := t.TempDir()
	SetConfigPath(filepath.Join(dir, "gw.yaml"))

	syncChannels = func(cfg *GatewayYaml) (*SyncResult, error) {
		return nil, errors.New("channel sync failed")
	}
	syncCost = func(cfg *GatewayYaml) (*CostSyncResult, error) {
		return nil, errors.New("cost sync failed")
	}

	res, err := SaveConfig(validYAML)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.Applied)
	require.Len(t, res.Errors, 2)
	joined := strings.Join(res.Errors, "|")
	assert.Contains(t, joined, "channel sync")
	assert.Contains(t, joined, "cost sync")
}

func TestSwapAndGetConfig_Atomic(t *testing.T) {
	resetGlobalState(t)
	cfg, err := ParseAndValidate(validYAML)
	require.NoError(t, err)
	swapGlobalConfig(cfg)
	got := GetConfig()
	require.NotNil(t, got)
	assert.Same(t, cfg, got)
}

func TestGetConfig_ConcurrentReads(t *testing.T) {
	resetGlobalState(t)
	cfg, err := ParseAndValidate(validYAML)
	require.NoError(t, err)
	swapGlobalConfig(cfg)

	var wg sync.WaitGroup
	const readers = 32
	var mismatches atomic.Int64
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				got := GetConfig()
				if got == nil || got.Providers["zen"] == nil {
					mismatches.Add(1)
					return
				}
			}
		}()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				newCfg, _ := ParseAndValidate(validYAML)
				swapGlobalConfig(newCfg)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int64(0), mismatches.Load())
}

func TestHotReloadConfig_RereadsAndSwaps(t *testing.T) {
	resetGlobalState(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "gw.yaml")
	require.NoError(t, os.WriteFile(path, []byte(validYAML), 0o600))
	SetConfigPath(path)

	require.NoError(t, HotReloadConfig())
	first := GetConfig()
	require.NotNil(t, first)

	mutated := strings.Replace(validYAML, "name: zen", "name: zen2", 1)
	require.NoError(t, os.WriteFile(path, []byte(mutated), 0o600))

	require.NoError(t, HotReloadConfig())
	second := GetConfig()
	require.NotNil(t, second)
	assert.Equal(t, "zen2", second.Providers["zen"].Name)
	assert.NotSame(t, first, second)
}

func TestHotReloadConfig_NoSourceLeavesGlobalUntouched(t *testing.T) {
	resetGlobalState(t)
	cfg, err := ParseAndValidate(validYAML)
	require.NoError(t, err)
	swapGlobalConfig(cfg)

	SetConfigPath(filepath.Join(t.TempDir(), "absent.yaml"))
	require.NoError(t, HotReloadConfig())
	assert.Same(t, cfg, GetConfig())
}

func TestHandleGatewayOption(t *testing.T) {
	resetGlobalState(t)

	assert.True(t, HandleGatewayOption(OptGatewayRoutingEnabled, "true"))
	assert.True(t, HandleGatewayOption(OptGatewayAffinityEnabled, "true"))
	assert.True(t, HandleGatewayOption(OptGatewayChannelAutoSync, "false"))
	assert.True(t, HandleGatewayOption(OptGatewayCostSyncEnabled, "true"))

	dir := t.TempDir()
	path := filepath.Join(dir, "p.yaml")
	assert.True(t, HandleGatewayOption(OptGatewayRoutingConfigPath, path))
	assert.Equal(t, path, GetConfigPath())

	assert.True(t, HandleGatewayOption(OptGatewayRoutingConfigYaml, validYAML))
	assert.NotNil(t, GetConfig())

	assert.False(t, HandleGatewayOption("UnrelatedKey", "x"))
}

func TestHandleGatewayOption_InvalidYamlIgnored(t *testing.T) {
	resetGlobalState(t)
	cfg, err := ParseAndValidate(validYAML)
	require.NoError(t, err)
	swapGlobalConfig(cfg)

	handled := HandleGatewayOption(OptGatewayRoutingConfigYaml, "llm_gateway:\n  bogus: stuff\n")
	assert.True(t, handled)
	assert.Same(t, cfg, GetConfig(), "global config must remain unchanged when YAML is invalid")
}

func TestHandleGatewayOption_EmptyYamlNoop(t *testing.T) {
	resetGlobalState(t)
	cfg, err := ParseAndValidate(validYAML)
	require.NoError(t, err)
	swapGlobalConfig(cfg)

	assert.True(t, HandleGatewayOption(OptGatewayRoutingConfigYaml, ""))
	assert.Same(t, cfg, GetConfig())
}

func TestRegisterGatewayOptionHooks_NoPanic(t *testing.T) {
	resetGlobalState(t)
	assert.NotPanics(t, func() { RegisterGatewayOptionHooks() })
}

func TestParseAndValidate_DailyTokenTiers(t *testing.T) {
	valid := strings.Replace(validYAML, "providers:", "daily_token_tiers: [5000, 10000, 15000, 20000]\nproviders:", 1)
	cfg, err := ParseAndValidate(valid)
	require.NoError(t, err)
	assert.Equal(t, []float64{5000, 10000, 15000, 20000}, cfg.DailyTokenTiers)

	// provider-level override and explicit disable parse independently
	withProv := strings.Replace(valid, "  zen:", "  zen:\n    dailyTokenTiers: [1000]", 1)
	cfg, err = ParseAndValidate(withProv)
	require.NoError(t, err)
	assert.Equal(t, []float64{1000}, cfg.Providers["zen"].DailyTokenTiers)

	disabled := strings.Replace(valid, "  zen:", "  zen:\n    dailyTokenTiers: []", 1)
	cfg, err = ParseAndValidate(disabled)
	require.NoError(t, err)
	assert.Equal(t, []float64{}, cfg.Providers["zen"].DailyTokenTiers)

	for _, tc := range []struct {
		name   string
		tiers  string
		field  string
		inProv bool
	}{
		{"negative global", "daily_token_tiers: [5000, -1]", "daily_token_tiers", false},
		{"zero global", "daily_token_tiers: [0]", "daily_token_tiers", false},
		{"negative provider", "dailyTokenTiers: [-0.5]", "providers.zen.dailyTokenTiers", true},
		{"zero provider", "dailyTokenTiers: [1000, 0]", "providers.zen.dailyTokenTiers", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := validYAML
			if tc.inProv {
				src = strings.Replace(src, "  zen:", "  zen:\n    "+tc.tiers, 1)
			} else {
				src = strings.Replace(src, "providers:", tc.tiers+"\nproviders:", 1)
			}
			_, err := ParseAndValidate(src)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.field)
		})
	}
}
