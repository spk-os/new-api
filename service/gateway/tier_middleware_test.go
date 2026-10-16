package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tierMiddlewareEnv installs the flags GatewayStrategyMiddleware gates on,
// restoring them on cleanup.
func tierMiddlewareEnv(t *testing.T) {
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

// tierMiddlewareConfig builds a two-provider config whose default group
// declares auto_high = [glmA (p2), kimiB (p1)] — note the tier array order
// deliberately disagrees with the provider order so tests can tell them
// apart.
func tierMiddlewareConfig(t *testing.T) {
	t.Helper()
	prevCfg := GetConfig()
	t.Cleanup(func() { SetConfig(prevCfg) })
	SetConfig(&GatewayYaml{
		LLMGateway: map[string]*StrategyGroup{
			"default": {
				Version:  "1.0",
				Enabled:  dailyTierTestBoolPtr(true),
				Affinity: &AffinityConfig{Enabled: true},
				Routing: &RoutingConfig{
					ModelSelection: &ModelSelection{Strategy: "priority"},
					Tiers: map[string]*TierConfig{
						"auto_high": {Models: []string{"glmA", "kimiB"}},
					},
				},
			},
		},
		Providers: map[string]*Provider{
			"p1": {Name: "tier-p1", Enabled: true, Order: 0, Key: "k1",
				Models: []*ModelGroup{{Name: "default", Model: "kimiB", Enabled: true}}},
			"p2": {Name: "tier-p2", Enabled: true, Order: 5, Key: "k2",
				Models: []*ModelGroup{{Name: "default", Model: "glmA", Enabled: true}}},
		},
	})
}

func seedTierChannel(t *testing.T, name string) int {
	t.Helper()
	ch := model.Channel{Name: name, Status: common.ChannelStatusEnabled}
	require.NoError(t, model.DB.Create(&ch).Error)
	return ch.Id
}

// tierMiddlewareRequest runs the strategy middleware for a tier request and
// returns the response, whether the handler was reached, the plan left in the
// context, and the handler context (for asserting downstream-visible side
// effects like the rewritten body model).
func tierMiddlewareRequest(t *testing.T, body string, headers map[string]string) (*httptest.ResponseRecorder, bool, *GatewayPlan, *gin.Context) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	reached := false
	var plan *GatewayPlan
	var handlerCtx *gin.Context
	r.POST("/v1/chat/completions", GatewayStrategyMiddleware(), func(c *gin.Context) {
		reached = true
		plan = GetGatewayPlanFromContext(c)
		handlerCtx = c
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w, reached, plan, handlerCtx
}

// TestGatewayStrategyMiddleware_TierRequestPlan protects the tier plan shape
// the retry layer consumes: a request for a configured tier gets a plan with
// TierRequest set and the full candidate chain in tier order (glmA's
// provider first even though p1 has the lower provider order), the body
// rewritten to the picked model, and the requested model preserved in its
// own context key.
func TestGatewayStrategyMiddleware_TierRequestPlan(t *testing.T) {
	setupDailyTierTestDB(t)
	tierMiddlewareEnv(t)
	tierMiddlewareConfig(t)
	id2 := seedTierChannel(t, "tier-p2")

	w, reached, plan, handlerCtx := tierMiddlewareRequest(t, `{"model":"auto_high","messages":[]}`, nil)
	require.True(t, reached)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, plan)
	assert.Equal(t, "auto_high", plan.TierRequest)
	require.Len(t, plan.Candidates, 2)
	assert.Equal(t, "p2", plan.Candidates[0].ProviderId, "tier model order outranks provider order")
	assert.Equal(t, "glmA", plan.Candidates[0].ActualModel)
	assert.Equal(t, id2, plan.Candidates[0].ChannelId)

	// Body rewritten to the picked concrete model and the requested tier name
	// preserved in its own context slot.
	bs := readRequestBodyBytes(handlerCtx)
	assert.Contains(t, string(bs), `"model":"glmA"`)
	reqModel, _ := common.GetContextKey(handlerCtx, constant.ContextKeyGatewayRequestedModel)
	assert.Equal(t, "auto_high", reqModel)
}

// TestGatewayStrategyMiddleware_TierAffinityHitKeepsFallback protects the
// affinity-hit tier path: a sticky binding on a tier request must still
// carry the tier's other candidates behind the sticky one, so the retry
// layer can fail over across models when the bound provider fails.
func TestGatewayStrategyMiddleware_TierAffinityHitKeepsFallback(t *testing.T) {
	setupDailyTierTestDB(t)
	tierMiddlewareEnv(t)
	tierMiddlewareConfig(t)
	id1 := seedTierChannel(t, "tier-p1")
	seedTierChannel(t, "tier-p2")
	ClearAffinityBindings()
	t.Cleanup(ClearAffinityBindings)

	SetAffinityBinding(&AffinityBindingRecord{
		ClientId: "cli1", TaskId: "task1", ProviderId: "p1", ProviderName: "tier-p1",
		ChannelId: id1, ModelGroup: "default", ActualModel: "kimiB",
	}, 0)

	headers := map[string]string{"X-Client-ID": "cli1", "X-Task-ID": "task1"}
	w, reached, plan, _ := tierMiddlewareRequest(t, `{"model":"auto_high","messages":[]}`, headers)
	require.True(t, reached)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, plan)
	assert.True(t, plan.AffinityHit, "fresh binding must be honored")
	assert.Equal(t, "auto_high", plan.TierRequest, "affinity hit on a tier keeps TierRequest for failover")
	require.Len(t, plan.Candidates, 2, "sticky candidate plus the tier fallback chain")
	assert.Equal(t, "p1", plan.Candidates[0].ProviderId, "sticky candidate stays first")
	assert.Equal(t, "kimiB", plan.Candidates[0].ActualModel)
	assert.Equal(t, "p2", plan.Candidates[1].ProviderId, "tier fallback candidate appended")
	assert.Equal(t, "glmA", plan.Candidates[1].ActualModel)
}

// TestGatewayStrategyMiddleware_NonTierPlanUntouched protects the non-tier
// regression surface: an ordinary model request through the same middleware
// must produce a plan with TierRequest empty.
func TestGatewayStrategyMiddleware_NonTierPlanUntouched(t *testing.T) {
	setupDailyTierTestDB(t)
	tierMiddlewareEnv(t)
	tierMiddlewareConfig(t)
	seedTierChannel(t, "tier-p1")

	w, reached, plan, _ := tierMiddlewareRequest(t, `{"model":"kimiB","messages":[]}`, nil)
	require.True(t, reached)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, plan)
	assert.Equal(t, "", plan.TierRequest)
	require.NotEmpty(t, plan.Candidates)
	assert.Equal(t, "p1", plan.Candidates[0].ProviderId)
}
