package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func dailyTierTestBoolPtr(b bool) *bool { return &b }

// resetDailyTierState clears all daily-tier global state (counters, channel-id
// cache, injected clock) and restores it after the test.
func resetDailyTierState(t *testing.T) {
	t.Helper()
	dailyTierUsageMap.Range(func(k, v interface{}) bool {
		dailyTierUsageMap.Delete(k)
		return true
	})
	dailyTierChanIdCache.Range(func(k, v interface{}) bool {
		dailyTierChanIdCache.Delete(k)
		return true
	})
	dailyTierNowFunc = time.Now
	t.Cleanup(func() {
		dailyTierUsageMap.Range(func(k, v interface{}) bool {
			dailyTierUsageMap.Delete(k)
			return true
		})
		dailyTierChanIdCache.Range(func(k, v interface{}) bool {
			dailyTierChanIdCache.Delete(k)
			return true
		})
		dailyTierNowFunc = time.Now
	})
}

// setupDailyTierTestDB installs an in-memory sqlite DB with quota_data and
// channels tables, restoring the previous DB and DataExportEnabled afterwards.
func setupDailyTierTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	previousDB := model.DB
	previousExport := common.DataExportEnabled
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.QuotaData{}, &model.Channel{}))
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		common.DataExportEnabled = previousExport
		_ = sqlDB.Close()
	})
	return db
}

func TestTierOfUsage_Boundaries(t *testing.T) {
	// tiers: 5000W, 10000W, 15000W, 20000W (in tokens)
	tiers := normalizeTierTokens([]float64{20000, 15000, 10000, 5000})
	require.Equal(t, []float64{5e7, 1e8, 1.5e8, 2e8}, tiers)

	assert.Equal(t, 0, tierOfUsage(0, tiers))
	assert.Equal(t, 0, tierOfUsage(5e7-1, tiers), "just below first tier")
	assert.Equal(t, 1, tierOfUsage(5e7, tiers), "usage == Ti counts as reached")
	assert.Equal(t, 2, tierOfUsage(1e8, tiers))
	assert.Equal(t, 4, tierOfUsage(2e8, tiers))
	assert.Equal(t, 4, tierOfUsage(9e9, tiers))
	assert.Equal(t, 0, tierOfUsage(1e6, nil))
}

func TestNormalizeTierTokens(t *testing.T) {
	assert.Nil(t, normalizeTierTokens(nil))
	assert.Nil(t, normalizeTierTokens([]float64{}))
	assert.Nil(t, normalizeTierTokens([]float64{0, -1}), "all non-positive = feature off")
	assert.Equal(t, []float64{5000, 10000}, normalizeTierTokens([]float64{1, 0.5}), "fractional W allowed, W→tokens")
	assert.Equal(t, []float64{5e7, 1e8}, normalizeTierTokens([]float64{10000, 5000}), "input order irrelevant")
	assert.Equal(t, []float64{5e7}, normalizeTierTokens([]float64{5000, 5000}), "duplicates removed")
}

func TestEffectiveTierTokens_OverrideSemantics(t *testing.T) {
	resetDailyTierState(t)
	prevCfg := GetConfig()
	t.Cleanup(func() { SetConfig(prevCfg) })
	SetConfig(&GatewayYaml{
		Providers: map[string]*Provider{
			"inherits": {Name: "ch-inherits", Enabled: true},
			"override": {Name: "ch-override", Enabled: true, DailyTokenTiers: []float64{1000}},
			"disabled": {Name: "ch-disabled", Enabled: true, DailyTokenTiers: []float64{}},
		},
		DailyTokenTiers: []float64{5000, 10000},
	})

	assert.Equal(t, []float64{5e7, 1e8}, effectiveTierTokens("inherits"), "nil provider field inherits global")
	assert.Equal(t, []float64{1e7}, effectiveTierTokens("override"), "provider list replaces global")
	assert.Nil(t, effectiveTierTokens("disabled"), "empty provider list disables")
	assert.Equal(t, []float64{5e7, 1e8}, effectiveTierTokens("missing"), "unknown provider inherits global")

	SetConfig(&GatewayYaml{
		Providers: map[string]*Provider{
			"inherits": {Name: "ch-inherits", Enabled: true},
		},
	})
	assert.Nil(t, effectiveTierTokens("inherits"), "no config at all = feature off")
}

// dailyTierConfig builds a two-provider config with the global ladder.
func dailyTierConfig(t *testing.T) {
	t.Helper()
	prevCfg := GetConfig()
	t.Cleanup(func() { SetConfig(prevCfg) })
	mg := func() *ModelGroup {
		return &ModelGroup{Name: "default", Model: "m1", Enabled: true}
	}
	SetConfig(&GatewayYaml{
		LLMGateway: map[string]*StrategyGroup{
			"default": {
				Enabled:  dailyTierTestBoolPtr(true),
				Affinity: &AffinityConfig{Enabled: true},
			},
		},
		Providers: map[string]*Provider{
			"p1": {Name: "ch-p1", Enabled: true, Order: 0, Key: "k1", Models: []*ModelGroup{mg()}},
			"p2": {Name: "ch-p2", Enabled: true, Order: 5, Key: "k2", Models: []*ModelGroup{mg()}},
		},
		DailyTokenTiers: []float64{5000, 10000},
	})
}

func seedDailyTierChannel(t *testing.T, name string, used int64) int {
	t.Helper()
	ch := model.Channel{Name: name, Status: common.ChannelStatusEnabled}
	require.NoError(t, model.DB.Create(&ch).Error)
	if used > 0 {
		RecordChannelDailyUsage(ch.Id, used)
	}
	return ch.Id
}

func TestApplyDailyTierScheduling_ReorderByTier(t *testing.T) {
	resetDailyTierState(t)
	setupDailyTierTestDB(t)
	dailyTierConfig(t)
	id1 := seedDailyTierChannel(t, "ch-p1", 5000*10000) // tier 1
	seedDailyTierChannel(t, "ch-p2", 0)
	require.Equal(t, id1, providerChannelId("ch-p1"))

	cands := BuildCandidates(&EffectiveProfile{}, "m1", nil)
	require.Len(t, cands, 2)
	require.Equal(t, "p1", cands[0].ProviderId, "sanity: order puts p1 first")

	out, allBroken := ApplyDailyTierScheduling(cands)
	require.False(t, allBroken)
	require.Len(t, out, 2)
	assert.Equal(t, "p2", out[0].ProviderId, "tier scheduling outranks static order")
	assert.Equal(t, "p1", out[1].ProviderId)
	assert.Equal(t, 0, out[0].DailyTier)
	assert.Equal(t, 1, out[1].DailyTier)

	// Stability inside a tier: equal-tier candidates keep their input order
	// (two candidates of the same tier-1 provider p1).
	sameTier := []*Candidate{
		{ProviderId: "p1"},
		{ProviderId: "p1"},
	}
	out, allBroken = ApplyDailyTierScheduling(sameTier)
	require.False(t, allBroken)
	require.Len(t, out, 2)
	assert.Equal(t, "p1", out[0].ProviderId)
	assert.Equal(t, "p1", out[1].ProviderId)
}

func TestApplyDailyTierScheduling_AllBroken(t *testing.T) {
	resetDailyTierState(t)
	setupDailyTierTestDB(t)
	dailyTierConfig(t)
	id1 := seedDailyTierChannel(t, "ch-p1", 10000*10000) // tier 2 == len(tiers) → broken
	seedDailyTierChannel(t, "ch-p2", 20000*10000) // broken

	cands := BuildCandidates(&EffectiveProfile{}, "m1", nil)
	require.Len(t, cands, 2)
	out, allBroken := ApplyDailyTierScheduling(cands)
	assert.True(t, allBroken)
	assert.Empty(t, out)

	// One broken, one healthy → only the broken one is filtered. Channels stay
	// as they are; only the in-memory counters are rewritten.
	resetDailyTierState(t)
	RecordChannelDailyUsage(id1, 10000*10000)
	cands = BuildCandidates(&EffectiveProfile{}, "m1", nil)
	out, allBroken = ApplyDailyTierScheduling(cands)
	require.False(t, allBroken)
	require.Len(t, out, 1)
	assert.Equal(t, "p2", out[0].ProviderId)
}

func TestApplyDailyTierScheduling_FeatureOff(t *testing.T) {
	resetDailyTierState(t)
	setupDailyTierTestDB(t)
	prevCfg := GetConfig()
	t.Cleanup(func() { SetConfig(prevCfg) })
	SetConfig(&GatewayYaml{
		Providers: map[string]*Provider{
			"p1": {Name: "ch-p1", Enabled: true, Order: 0, Key: "k1",
				Models: []*ModelGroup{{Name: "default", Model: "m1", Enabled: true}}},
			"p2": {Name: "ch-p2", Enabled: true, Order: 5, Key: "k2",
				Models: []*ModelGroup{{Name: "default", Model: "m1", Enabled: true}}},
		},
		// no DailyTokenTiers anywhere
	})

	cands := BuildCandidates(&EffectiveProfile{}, "m1", nil)
	require.Len(t, cands, 2)
	out, allBroken := ApplyDailyTierScheduling(cands)
	assert.False(t, allBroken)
	assert.Equal(t, "p1", out[0].ProviderId, "feature off = order untouched")
	assert.Equal(t, "p2", out[1].ProviderId)
	assert.Equal(t, 0, out[0].DailyTier)
}

func TestShouldBreakAffinityByTier(t *testing.T) {
	// sticky broken → always break (caller guarantees alternatives were checked)
	assert.True(t, shouldBreakAffinityByTier(true, 2, 2))
	assert.True(t, shouldBreakAffinityByTier(true, 1, 2))
	// healthy sticky: tolerance is exactly one tier gap
	assert.False(t, shouldBreakAffinityByTier(false, 0, 0))
	assert.False(t, shouldBreakAffinityByTier(false, 1, 0), "one-tier gap keeps the session sticky")
	assert.True(t, shouldBreakAffinityByTier(false, 2, 0), "two-tier gap breaks")
}

func TestChannelDailyUsedTokens_DaySwitchReseed(t *testing.T) {
	resetDailyTierState(t)
	db := setupDailyTierTestDB(t)
	common.DataExportEnabled = true

	day1 := time.Date(2026, 10, 16, 12, 0, 0, 0, time.Local)
	day2 := time.Date(2026, 10, 17, 12, 0, 0, 0, time.Local)
	now := day1
	dailyTierNowFunc = func() time.Time { return now }

	require.NoError(t, db.Create(&model.QuotaData{ChannelID: 11, TokenUsed: 100, CreatedAt: day1.Unix()}).Error)

	assert.Equal(t, int64(100), channelDailyUsedTokens(11), "seeded from quota_data for the current day")
	RecordChannelDailyUsage(11, 5)
	assert.Equal(t, int64(105), channelDailyUsedTokens(11), "in-memory increment on top of the seed")

	// Day 2 arrives and the flusher writes that day's row.
	now = day2
	require.NoError(t, db.Create(&model.QuotaData{ChannelID: 11, TokenUsed: 200, CreatedAt: day2.Unix()}).Error)
	assert.Equal(t, int64(200), channelDailyUsedTokens(11), "day switch re-seeds from the new day's rows")
	RecordChannelDailyUsage(11, 7)
	assert.Equal(t, int64(207), channelDailyUsedTokens(11))
}

func TestSeedChannelDailyUsedTokens_ExcludesOtherDaysAndChannels(t *testing.T) {
	resetDailyTierState(t)
	db := setupDailyTierTestDB(t)
	common.DataExportEnabled = true

	dayStart := time.Date(2026, 10, 17, 0, 0, 0, 0, time.Local)
	now := time.Date(2026, 10, 17, 10, 0, 0, 0, time.Local)
	yesterday := dayStart.Add(-time.Hour)

	require.NoError(t, db.Create(&model.QuotaData{ChannelID: 21, TokenUsed: 500, CreatedAt: now.Unix()}).Error)
	require.NoError(t, db.Create(&model.QuotaData{ChannelID: 21, TokenUsed: 999, CreatedAt: yesterday.Unix()}).Error)
	require.NoError(t, db.Create(&model.QuotaData{ChannelID: 22, TokenUsed: 123, CreatedAt: now.Unix()}).Error)

	assert.Equal(t, int64(500), seedChannelDailyUsedTokens(21, now), "only today's rows of this channel")
	assert.Equal(t, int64(123), seedChannelDailyUsedTokens(22, now))

	common.DataExportEnabled = false
	assert.Equal(t, int64(0), seedChannelDailyUsedTokens(21, now), "no export = no baseline")
}

func TestRecordChannelDailyUsage_Concurrent(t *testing.T) {
	resetDailyTierState(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				RecordChannelDailyUsage(77, 3)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int64(16*100*3), channelDailyUsedTokens(77))
}

// dailyTierMiddlewareRequest runs the gateway strategy middleware against a
// request and reports whether the terminal handler was reached.
func dailyTierMiddlewareRequest(t *testing.T, body string, headers map[string]string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	reached := false
	r.POST("/v1/chat/completions", GatewayStrategyMiddleware(), func(c *gin.Context) {
		reached = true
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w, reached
}

func setDailyTierMiddlewareEnv(t *testing.T) {
	t.Helper()
	prevRouting := common.GatewayRoutingEnabled
	prevAffinity := common.GatewayAffinityEnabled
	common.GatewayRoutingEnabled = true
	common.GatewayAffinityEnabled = true
	t.Cleanup(func() {
		common.GatewayRoutingEnabled = prevRouting
		common.GatewayAffinityEnabled = prevAffinity
	})
}

func TestGatewayStrategyMiddleware_AllBroken403(t *testing.T) {
	resetDailyTierState(t)
	setupDailyTierTestDB(t)
	setDailyTierMiddlewareEnv(t)
	dailyTierConfig(t)
	seedDailyTierChannel(t, "ch-p1", 10000*10000) // broken
	seedDailyTierChannel(t, "ch-p2", 10000*10000) // broken

	w, reached := dailyTierMiddlewareRequest(t, `{"model":"m1","messages":[]}`, nil)
	require.False(t, reached, "request must be aborted before the handler")
	require.Equal(t, http.StatusForbidden, w.Code)

	body := w.Body.String()
	assert.Contains(t, body, dailyTierCircuitBrokenCode)
	assert.Contains(t, body, "new_api_error")
}

func TestGatewayStrategyMiddleware_AffinityBreakOnTierGap(t *testing.T) {
	resetDailyTierState(t)
	setupDailyTierTestDB(t)
	setDailyTierMiddlewareEnv(t)
	dailyTierConfig(t)
	// Channels are created once; later phases only rewrite the usage counters.
	id1 := seedDailyTierChannel(t, "ch-p1", 0)
	seedDailyTierChannel(t, "ch-p2", 0)
	ClearAffinityBindings()

	headers := map[string]string{"X-Client-ID": "cli1", "X-Task-ID": "task1"}
	bind := func() {
		SetAffinityBinding(&AffinityBindingRecord{
			ClientId: "cli1", TaskId: "task1", ProviderId: "p1", ProviderName: "ch-p1",
			ChannelId: id1, ModelGroup: "default", ActualModel: "m1",
		}, 0)
	}

	// One-tier gap: sticky p1 (tier 1) vs p2 (tier 0) → keep affinity.
	RecordChannelDailyUsage(id1, 5000*10000)
	bind()

	w, reached := dailyTierMiddlewareRequest(t, `{"model":"m1","messages":[]}`, headers)
	require.True(t, reached)
	require.Equal(t, http.StatusOK, w.Code)
	rec := GetAffinityBinding("cli1", "task1")
	require.NotNil(t, rec)
	assert.Equal(t, "p1", rec.ProviderId, "one-tier gap keeps the session sticky")

	// Sticky provider reaches its highest tier → affinity must break and the
	// request must land on the healthy provider.
	resetDailyTierState(t)
	RecordChannelDailyUsage(id1, 10000*10000) // tier 2 == len(tiers) → broken
	bind()

	w, reached = dailyTierMiddlewareRequest(t, `{"model":"m1","messages":[]}`, headers)
	require.True(t, reached, "healthy alternative exists, so no 403")
	require.Equal(t, http.StatusOK, w.Code)
	rec = GetAffinityBinding("cli1", "task1")
	require.NotNil(t, rec)
	assert.Equal(t, "p2", rec.ProviderId, "binding re-created on the healthy provider")

	// Two-tier gap without exhaustion: sticky p1 tier 2 vs p2 tier 0 → break.
	resetDailyTierState(t)
	setConfigWithExtraTier(t) // 3-tier ladder so p1 can be tier 2 but not broken
	RecordChannelDailyUsage(id1, 10000*10000)
	bind()

	w, reached = dailyTierMiddlewareRequest(t, `{"model":"m1","messages":[]}`, headers)
	require.True(t, reached)
	require.Equal(t, http.StatusOK, w.Code)
	rec = GetAffinityBinding("cli1", "task1")
	require.NotNil(t, rec)
	assert.Equal(t, "p2", rec.ProviderId, "two-tier gap breaks affinity")
}

// setConfigWithExtraTier swaps in a 3-tier ladder (5000W/10000W/15000W) for
// the two-tier-gap affinity case; the config cleanup registered by
// dailyTierConfig restores the previous config afterwards.
func setConfigWithExtraTier(t *testing.T) {
	t.Helper()
	cfg := GetConfig()
	cfg.DailyTokenTiers = []float64{5000, 10000, 15000}
	SetConfig(cfg)
}
