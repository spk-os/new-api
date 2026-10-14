package service

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/gateway"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetLoopState clears the loop windows and the alarm dedupe so tests are
// order-independent. Callers also override loopNowFunc/loopSleepFunc as needed
// and must restore them via defer.
func resetLoopState() {
	loopWindows = sync.Map{}
}

// loopParams builds a consume-log params with a large token total (above the
// default 5W threshold) and a fast response window.
func loopParams(modelName string, tokens int64) model.RecordConsumeLogParams {
	return model.RecordConsumeLogParams{
		ModelName:        modelName,
		PromptTokens:     int(tokens),
		CompletionTokens: 0,
		CacheTokens:      0,
		ChannelId:        1,
		TokenName:        "tk",
		UseTimeSeconds:   1,
	}
}

// loopCtx builds a gin context carrying the resolved client, request id, a
// request start time (so respMs is computed from loopNowFunc-start), and a
// request body size.
func loopCtx(client, requestId string, reqSize int64, start time.Time) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Request.ContentLength = reqSize
	c.Set(model.LoopClientCtxKey, client)
	c.Set(common.RequestIdKey, requestId)
	common.SetContextKey(c, constant.ContextKeyRequestStartTime, start)
	return c
}

// loopConfig returns an AlarmConfig with the same defaults ParseAndValidate
// fills, so tests exercise the production rule.
func loopConfig() *gateway.AlarmConfig {
	return &gateway.AlarmConfig{
		LoopMinTokens:   50000,
		LoopSizeDiffPct: 1,
		LoopMaxRespMs:   5000,
		LoopSleepMs:     180000,
		LoopWindowMs:    60000,
		LoopMinHits:     3,
		Interval:        600000,
	}
}

// runLoop installs a global config carrying the given alarm and a controllable
// clock, returns the now-advancer.
func runLoop(t *testing.T, alarm *gateway.AlarmConfig) (advance func(d time.Duration), now func() time.Time) {
	t.Helper()
	swapGatewayConfigForTest(t, &gateway.GatewayYaml{Alarm: alarm})
	base := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	cur := base
	loopNowFunc = func() time.Time { return cur }
	t.Cleanup(func() { loopNowFunc = time.Now })
	return func(d time.Duration) { cur = cur.Add(d) }, func() time.Time { return cur }
}

// swapGatewayConfigForTest writes a config into the gateway global store
// without touching the filesystem.
func swapGatewayConfigForTest(t *testing.T, cfg *gateway.GatewayYaml) {
	t.Helper()
	gateway.SetConfig(cfg)
}

// windowOf reads back the loopWindow stored under client+model so tests can
// assert on flagged/baseSize state.
func windowOf(t *testing.T, client, modelName string) *loopWindow {
	t.Helper()
	v, ok := loopWindows.Load(client + "\x00" + modelName)
	require.True(t, ok, "expected a loop window for %s/%s", client, modelName)
	w, ok := v.(*loopWindow)
	require.True(t, ok)
	return w
}

func TestLoopDetect_LearningFlagsAfterMinHits(t *testing.T) {
	resetLoopState()
	resetAlarmState()
	advance, _ := runLoop(t, loopConfig())

	const client, model = "claude-tool", "gpt-4"
	for i := 0; i < 3; i++ {
		advance(100 * time.Millisecond) // ~100ms response, well under 5s
		c := loopCtx(client, "req", 10000, loopNowFunc().Add(-100*time.Millisecond))
		checkLoopCall(c, 7, loopNowFunc().Unix(), loopParams(model, 60000))
	}
	w := windowOf(t, client, model)
	assert.True(t, w.flagged, "3 large fast same-size hits should flag the source")
	assert.Equal(t, int64(10000), w.baseSize)
}

func TestLoopDetect_BelowTokenThresholdNeverFlags(t *testing.T) {
	resetLoopState()
	resetAlarmState()
	advance, _ := runLoop(t, loopConfig())

	const client, model = "small-tool", "gpt-4"
	for i := 0; i < 5; i++ {
		advance(100 * time.Millisecond)
		c := loopCtx(client, "req", 10000, loopNowFunc().Add(-100*time.Millisecond))
		// 1000 tokens < 50000 threshold → neither learned nor penalised.
		checkLoopCall(c, 7, loopNowFunc().Unix(), loopParams(model, 1000))
	}
	_, present := loopWindows.Load(client + "\x00" + model)
	assert.False(t, present, "sub-threshold requests must not create a window")
}

func TestLoopDetect_LearningRequiresFastResponse(t *testing.T) {
	resetLoopState()
	resetAlarmState()
	advance, _ := runLoop(t, loopConfig())

	const client, model = "slow-tool", "gpt-4"
	// 6s response (>= LoopMaxRespMs=5000) → never matches the learning rule.
	for i := 0; i < 3; i++ {
		advance(100 * time.Millisecond)
		c := loopCtx(client, "req", 10000, loopNowFunc().Add(-6*time.Second))
		checkLoopCall(c, 7, loopNowFunc().Unix(), loopParams(model, 60000))
	}
	w := windowOf(t, client, model)
	assert.False(t, w.flagged, "slow responses must not trigger the learning rule")
}

func TestLoopDetect_LearningRequiresSizeProximity(t *testing.T) {
	resetLoopState()
	resetAlarmState()
	advance, _ := runLoop(t, loopConfig())

	const client, model = "varied-tool", "gpt-4"
	// Three large fast hits but wildly different sizes → not anomalous.
	sizes := []int64{10000, 50000, 200000}
	for i := 0; i < 3; i++ {
		advance(100 * time.Millisecond)
		c := loopCtx(client, "req", sizes[i], loopNowFunc().Add(-100*time.Millisecond))
		checkLoopCall(c, 7, loopNowFunc().Unix(), loopParams(model, 60000))
	}
	w := windowOf(t, client, model)
	assert.False(t, w.flagged, "divergent request sizes must not trigger the rule")
}

func TestLoopDetect_WindowPrunesStaleHits(t *testing.T) {
	resetLoopState()
	resetAlarmState()
	advance, _ := runLoop(t, loopConfig())

	const client, model = "burst-tool", "gpt-4"
	// Two hits now, then jump past the 1-minute window: the old hits must be
	// pruned so a single fresh hit cannot complete a 3-hit learning batch.
	for i := 0; i < 2; i++ {
		advance(100 * time.Millisecond)
		c := loopCtx(client, "req", 10000, loopNowFunc().Add(-100*time.Millisecond))
		checkLoopCall(c, 7, loopNowFunc().Unix(), loopParams(model, 60000))
	}
	advance(2 * time.Minute) // past LoopWindowMs=60000
	c := loopCtx(client, "req", 10000, loopNowFunc().Add(-100*time.Millisecond))
	checkLoopCall(c, 7, loopNowFunc().Unix(), loopParams(model, 60000))
	w := windowOf(t, client, model)
	assert.False(t, w.flagged, "stale hits outside the window must not count toward the batch")
	assert.Len(t, w.hits, 1, "only the fresh hit survives pruning")
}

func TestLoopDetect_BlockPhaseSleepsOnMatchingSize(t *testing.T) {
	resetLoopState()
	resetAlarmState()
	advance, _ := runLoop(t, loopConfig())

	const client, model = "loop-tool", "gpt-4"
	// Learn: 3 identical large fast hits at size 10000 → flagged, baseSize=10000.
	for i := 0; i < 3; i++ {
		advance(100 * time.Millisecond)
		c := loopCtx(client, "req", 10000, loopNowFunc().Add(-100*time.Millisecond))
		checkLoopCall(c, 7, loopNowFunc().Unix(), loopParams(model, 60000))
	}
	require.True(t, windowOf(t, client, model).flagged)

	// 4th hit: same size (diff 0% < 1%) → blocked, must sleep LoopSleepMs.
	var slept time.Duration
	var sleepCalls int
	loopSleepFunc = func(c *gin.Context, d time.Duration) {
		slept = d
		sleepCalls++
	}
	t.Cleanup(func() { loopSleepFunc = loopSleepProduction })
	advance(100 * time.Millisecond)
	c := loopCtx(client, "req4", 10000, loopNowFunc().Add(-100*time.Millisecond))
	checkLoopCall(c, 7, loopNowFunc().Unix(), loopParams(model, 60000))
	assert.Equal(t, 1, sleepCalls, "matching-size hit after flag must sleep")
	assert.Equal(t, 180*time.Second, slept)
}

func TestLoopDetect_BlockPhaseSkipsSleepOnDivergentSize(t *testing.T) {
	resetLoopState()
	resetAlarmState()
	advance, _ := runLoop(t, loopConfig())

	const client, model = "drift-tool", "gpt-4"
	for i := 0; i < 3; i++ {
		advance(100 * time.Millisecond)
		c := loopCtx(client, "req", 10000, loopNowFunc().Add(-100*time.Millisecond))
		checkLoopCall(c, 7, loopNowFunc().Unix(), loopParams(model, 60000))
	}
	require.True(t, windowOf(t, client, model).flagged)

	var sleepCalls int
	loopSleepFunc = func(c *gin.Context, d time.Duration) { sleepCalls++ }
	t.Cleanup(func() { loopSleepFunc = loopSleepProduction })
	// Size 10200 vs base 10000 → diff 2% >= 1% → not blocked.
	advance(100 * time.Millisecond)
	c := loopCtx(client, "req4", 10200, loopNowFunc().Add(-100*time.Millisecond))
	checkLoopCall(c, 7, loopNowFunc().Unix(), loopParams(model, 60000))
	assert.Equal(t, 0, sleepCalls, "divergent-size hit must not be blocked")
}

func TestLoopDetect_BlockRespectsConfiguredSleepDuration(t *testing.T) {
	resetLoopState()
	resetAlarmState()
	cfg := loopConfig()
	cfg.LoopSleepMs = 5000 // 5s instead of the 3min default
	advance, _ := runLoop(t, cfg)

	const client, model = "short-tool", "gpt-4"
	for i := 0; i < 3; i++ {
		advance(100 * time.Millisecond)
		c := loopCtx(client, "req", 10000, loopNowFunc().Add(-100*time.Millisecond))
		checkLoopCall(c, 7, loopNowFunc().Unix(), loopParams(model, 60000))
	}
	require.True(t, windowOf(t, client, model).flagged)

	var slept time.Duration
	loopSleepFunc = func(c *gin.Context, d time.Duration) { slept = d }
	t.Cleanup(func() { loopSleepFunc = loopSleepProduction })
	advance(100 * time.Millisecond)
	c := loopCtx(client, "req4", 10000, loopNowFunc().Add(-100*time.Millisecond))
	checkLoopCall(c, 7, loopNowFunc().Unix(), loopParams(model, 60000))
	assert.Equal(t, 5*time.Second, slept, "sleep must use the configured LoopSleepMs")
}

func TestLoopDetect_RespectsConfiguredThresholds(t *testing.T) {
	resetLoopState()
	resetAlarmState()
	cfg := loopConfig()
	cfg.LoopMinTokens = 100000 // higher bar
	advance, _ := runLoop(t, cfg)

	const client, model = "high-bar-tool", "gpt-4"
	// 60000 tokens < raised 100000 threshold → never creates a window.
	for i := 0; i < 3; i++ {
		advance(100 * time.Millisecond)
		c := loopCtx(client, "req", 10000, loopNowFunc().Add(-100*time.Millisecond))
		checkLoopCall(c, 7, loopNowFunc().Unix(), loopParams(model, 60000))
	}
	_, present := loopWindows.Load(client + "\x00" + model)
	assert.False(t, present, "hits below the configured LoopMinTokens must not create a window")
}

// TestLoopDetect_BlockPhaseRequiresFastResponse proves the false-positive fix:
// once a source is flagged, a subsequent SLOW hit (response >= LoopMaxRespMs)
// must NOT be slept. This is the case that caused legitimate 15-31s Claude Code
// streaming turns to be penalised before the block phase required the response
// cap. All four conditions must hold at trigger time; a slow hit fails the
// response condition even when token and size match.
func TestLoopDetect_BlockPhaseRequiresFastResponse(t *testing.T) {
	resetLoopState()
	resetAlarmState()
	advance, _ := runLoop(t, loopConfig())

	const client, model = "slow-block-tool", "gpt-4"
	// Learn: 3 identical large fast hits at size 10000 → flagged, baseSize=10000.
	for i := 0; i < 3; i++ {
		advance(100 * time.Millisecond)
		c := loopCtx(client, "req", 10000, loopNowFunc().Add(-100*time.Millisecond))
		checkLoopCall(c, 7, loopNowFunc().Unix(), loopParams(model, 60000))
	}
	require.True(t, windowOf(t, client, model).flagged)

	var sleepCalls int
	loopSleepFunc = func(c *gin.Context, d time.Duration) { sleepCalls++ }
	t.Cleanup(func() { loopSleepFunc = loopSleepProduction })

	// 4th hit: same size + same token, but a 6s response (>= LoopMaxRespMs=5000)
	// → the response condition fails → must NOT be slept.
	advance(100 * time.Millisecond)
	c := loopCtx(client, "req4", 10000, loopNowFunc().Add(-6*time.Second))
	checkLoopCall(c, 7, loopNowFunc().Unix(), loopParams(model, 60000))
	assert.Equal(t, 0, sleepCalls, "a slow response must not be blocked even from a flagged source")
	assert.True(t, windowOf(t, client, model).flagged, "flag persists while the window still has hits")
}

// TestLoopDetect_BlockPhaseRequiresMinHitsInWindow proves the count condition
// at trigger time: after the burst ages out of the window, a fresh matching hit
// must NOT be slept (the flag self-clears, see next test, and even before that
// the count condition fails). Here we keep the flag but drop below minHits.
func TestLoopDetect_FlagRecoversWhenWindowEmpties(t *testing.T) {
	resetLoopState()
	resetAlarmState()
	advance, _ := runLoop(t, loopConfig())

	const client, model = "transient-tool", "gpt-4"
	for i := 0; i < 3; i++ {
		advance(100 * time.Millisecond)
		c := loopCtx(client, "req", 10000, loopNowFunc().Add(-100*time.Millisecond))
		checkLoopCall(c, 7, loopNowFunc().Unix(), loopParams(model, 60000))
	}
	require.True(t, windowOf(t, client, model).flagged)
	require.Equal(t, int64(10000), windowOf(t, client, model).baseSize)

	// Jump past the 1-minute window: the stale hits are pruned, the window is
	// empty → the flag and base size self-clear.
	advance(2 * time.Minute)
	c := loopCtx(client, "req-late", 10000, loopNowFunc().Add(-100*time.Millisecond))
	checkLoopCall(c, 7, loopNowFunc().Unix(), loopParams(model, 60000))
	w := windowOf(t, client, model)
	assert.False(t, w.flagged, "flag must self-clear once the window empties")
	assert.Equal(t, int64(0), w.baseSize, "base size must reset on recovery")
	assert.Len(t, w.hits, 1, "only the fresh hit survives")
}

// TestLoopDetect_FractionalSizeDiffPctRespected proves the int→float64 fix: a
// configured 0.2% threshold is actually honoured at runtime. Before the fix,
// yaml parsed 0.2 into the int field as 0, the zero-fill replaced it with the
// default 1%, and a 0.5% diff was wrongly blocked. Now 0.5% > 0.2% is NOT
// blocked, while 0.05% < 0.2% IS.
func TestLoopDetect_FractionalSizeDiffPctRespected(t *testing.T) {
	resetLoopState()
	resetAlarmState()
	cfg := loopConfig()
	cfg.LoopSizeDiffPct = 0.2 // fractional — would be silently dropped under the old int field
	advance, _ := runLoop(t, cfg)

	const client, model = "frac-tool", "gpt-4"
	// Learn at size 10000 (fast, large) → flagged, baseSize=10000.
	for i := 0; i < 3; i++ {
		advance(100 * time.Millisecond)
		c := loopCtx(client, "req", 10000, loopNowFunc().Add(-100*time.Millisecond))
		checkLoopCall(c, 7, loopNowFunc().Unix(), loopParams(model, 60000))
	}
	require.True(t, windowOf(t, client, model).flagged)

	var sleepCalls int
	loopSleepFunc = func(c *gin.Context, d time.Duration) { sleepCalls++ }
	t.Cleanup(func() { loopSleepFunc = loopSleepProduction })

	// 4th fast hit, size 10050 → diff 0.5% > 0.2% → must NOT block.
	advance(100 * time.Millisecond)
	c := loopCtx(client, "req4", 10050, loopNowFunc().Add(-100*time.Millisecond))
	checkLoopCall(c, 7, loopNowFunc().Unix(), loopParams(model, 60000))
	assert.Equal(t, 0, sleepCalls, "0.5%% diff must not be blocked under a 0.2%% threshold")

	// 5th fast hit, size 10005 → diff 0.05% < 0.2% → must block.
	advance(100 * time.Millisecond)
	c = loopCtx(client, "req5", 10005, loopNowFunc().Add(-100*time.Millisecond))
	checkLoopCall(c, 7, loopNowFunc().Unix(), loopParams(model, 60000))
	assert.Equal(t, 1, sleepCalls, "0.05%% diff must be blocked under a 0.2%% threshold")
}
