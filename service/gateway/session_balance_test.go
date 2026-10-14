package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetSessionBalanceState(t *testing.T) {
	t.Helper()
	sessionBindings.Range(func(k, v interface{}) bool {
		sessionBindings.Delete(k)
		return true
	})
	sessionNowFunc = time.Now
	t.Cleanup(func() {
		sessionBindings.Range(func(k, v interface{}) bool {
			sessionBindings.Delete(k)
			return true
		})
		sessionNowFunc = time.Now
	})
}

// twoProvidersSameOrder builds a config with two order-0 providers plus one
// order-5 fallback, mirroring the deveco6506/deveco9227 setup.
func twoProvidersSameOrder(t *testing.T) *GatewayYaml {
	t.Helper()
	mg := func() *ModelGroup {
		return &ModelGroup{Name: "default", Model: "hw_glm-5.3", Enabled: true}
	}
	cfg := &GatewayYaml{
		Providers: map[string]*Provider{
			"deveco6506": {Name: "DevEco Code_6506", Enabled: true, Order: 0, Key: "k6506", Models: []*ModelGroup{mg()}},
			"deveco9227": {Name: "DevEco Code_9227", Enabled: true, Order: 0, Key: "k9227", Models: []*ModelGroup{mg()}},
			"mimo":       {Name: "mimo", Enabled: true, Order: 5, Key: "kmimo", Models: []*ModelGroup{mg()}},
		},
	}
	SetConfig(cfg)
	return cfg
}

func balanceCandidates(t *testing.T) []*Candidate {
	t.Helper()
	prof := &EffectiveProfile{}
	cands := BuildCandidates(prof, "hw_glm-5.3", nil)
	require.Len(t, cands, 3)
	return cands
}

func TestSessionBalance_SameSessionStickyToSameProvider(t *testing.T) {
	resetSessionBalanceState(t)
	twoProvidersSameOrder(t)
	cands := balanceCandidates(t)

	SessionBalanceCandidates(cands, "sess-a")
	first := cands[0].ProviderId
	require.Contains(t, []string{"deveco6506", "deveco9227"}, first)

	// Repeated requests for the same session keep the same provider.
	for i := 0; i < 5; i++ {
		fresh := balanceCandidates(t)
		SessionBalanceCandidates(fresh, "sess-a")
		require.Equal(t, first, fresh[0].ProviderId, "session must stay on its assigned provider")
	}
}

func TestSessionBalance_DistinctSessionsSpreadAcrossProviders(t *testing.T) {
	resetSessionBalanceState(t)
	twoProvidersSameOrder(t)

	seen := map[string]int{}
	for i := 0; i < 20; i++ {
		cands := balanceCandidates(t)
		SessionBalanceCandidates(cands, fmt.Sprintf("sess-%d", i))
		seen[cands[0].ProviderId]++
	}
	// Random tie-break should use both equal-priority providers, never the
	// lower-priority fallback for a fresh session.
	assert.Greater(t, seen["deveco6506"], 0, "both equal-priority providers must receive sessions")
	assert.Greater(t, seen["deveco9227"], 0, "both equal-priority providers must receive sessions")
	assert.Equal(t, 0, seen["mimo"], "priority fallback must not be picked while higher-priority providers exist")
}

func TestSessionBalance_LeastLoadedPreferredOnAssignment(t *testing.T) {
	resetSessionBalanceState(t)
	twoProvidersSameOrder(t)

	// Bind 10 sessions to deveco6506 by pre-seeding the map.
	now := sessionNowFunc()
	for i := 0; i < 10; i++ {
		bindSessionToProvider(fmt.Sprintf("seed-%d", i), "deveco6506", now)
	}

	// A new session must be assigned the empty provider deterministically
	// (load difference beats the random tie-break).
	cands := balanceCandidates(t)
	SessionBalanceCandidates(cands, "sess-new")
	assert.Equal(t, "deveco9227", cands[0].ProviderId)
}

func TestSessionBalance_ReassignsWhenAssignedProviderNotInBlock(t *testing.T) {
	resetSessionBalanceState(t)
	twoProvidersSameOrder(t)

	// Session previously assigned to the order-5 provider (e.g. its peers
	// were cooling down at assignment time).
	bindSessionToProvider("sess-b", "mimo", sessionNowFunc())

	cands := balanceCandidates(t)
	SessionBalanceCandidates(cands, "sess-b")
	assert.Contains(t, []string{"deveco6506", "deveco9227"}, cands[0].ProviderId,
		"stale binding outside the first block must be reassigned within the block")
}

func TestSessionBalance_SingleCandidateIsNoop(t *testing.T) {
	resetSessionBalanceState(t)
	twoProvidersSameOrder(t)

	cands := []*Candidate{{ProviderId: "only"}}
	SessionBalanceCandidates(cands, "sess-c")
	assert.Equal(t, "only", cands[0].ProviderId, "single candidate must be untouched")

	SessionBalanceCandidates(nil, "sess-c") // must not panic
}

func TestSessionBalance_ExpiredBindingReassigned(t *testing.T) {
	resetSessionBalanceState(t)
	twoProvidersSameOrder(t)

	SessionBalanceCandidates(balanceCandidates(t), "sess-ttl")

	// Advance beyond the TTL: the binding must expire and the session gets a
	// fresh assignment (either equal-priority provider is valid).
	sessionNowFunc = func() time.Time { return time.Now().Add(sessionBalanceTTL + time.Minute) }
	cands := balanceCandidates(t)
	SessionBalanceCandidates(cands, "sess-ttl")
	assert.Contains(t, []string{"deveco6506", "deveco9227"}, cands[0].ProviderId)
}

func TestExtractSessionId(t *testing.T) {
	makeCtx := func(headers map[string]string) *gin.Context {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		for k, v := range headers {
			c.Request.Header.Set(k, v)
		}
		return c
	}

	assert.Equal(t, "", ExtractSessionId(makeCtx(nil)))

	c := makeCtx(map[string]string{"X-Claude-Code-Session-Id": "81a04b1c-c8f6-4d98-a748-037bd5235875"})
	assert.Equal(t, "81a04b1c-c8f6-4d98-a748-037bd5235875", ExtractSessionId(c))

	// Alias headers are honored when the primary is absent.
	c = makeCtx(map[string]string{"X-Session-Id": "s2"})
	assert.Equal(t, "s2", ExtractSessionId(c))
	c = makeCtx(map[string]string{"Session-Id": "s3"})
	assert.Equal(t, "s3", ExtractSessionId(c))

	// Primary wins over aliases.
	c = makeCtx(map[string]string{"X-Claude-Code-Session-Id": "primary", "X-Session-Id": "alias"})
	assert.Equal(t, "primary", ExtractSessionId(c))
}
