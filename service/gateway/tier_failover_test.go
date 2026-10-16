package gateway

import (
	"io"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tierPlan builds a plan with TierRequest set and the given candidates, the
// shape the strategy middleware produces for a tier request.
func tierPlan(cands ...*Candidate) *GatewayPlan {
	return &GatewayPlan{
		TierRequest: "auto_high",
		Candidates:  cands,
	}
}

func tierCand(providerId string, channelId int, model string) *Candidate {
	return &Candidate{
		ProviderId:  providerId,
		ChannelId:   channelId,
		ActualModel: model,
		TierIndex:   0,
	}
}

// TestIsTierRequest protects the failover gate: only plans built for a
// configured tier get cross-model failover; every other plan (including
// affinity hits without a tier and nil) keeps the existing retry semantics.
func TestIsTierRequest(t *testing.T) {
	assert.True(t, IsTierRequest(tierPlan()))
	assert.False(t, IsTierRequest(&GatewayPlan{}))
	assert.False(t, IsTierRequest(nil))
}

// TestNextTierCandidate_OrderAndSkips protects the failover target choice:
// the first candidate with a different channel wins; the just-failed
// channel, cooldown providers and unresolvable (no-channel) candidates are
// skipped; exhaustion reports ok=false.
func TestNextTierCandidate_OrderAndSkips(t *testing.T) {
	c := &gin.Context{}
	plan := tierPlan(
		tierCand("zen", 2, "kimiB"),  // cooldown provider
		tierCand("mimo", 0, "hy3"),   // unresolvable channel
		tierCand("dev", 4, "step-5"), // expected pick
	)

	ForceProviderCooldown("zen")
	t.Cleanup(func() { ResetProviderHealth("zen") })

	// Failed channel 1 is not in the plan: the cooldown and unresolvable
	// candidates must still be skipped, dev is the first usable pick.
	got, ok := NextTierCandidate(c, plan, 1)
	require.True(t, ok)
	assert.Equal(t, "dev", got.ProviderId)
	assert.Equal(t, 4, got.ChannelId)

	// Dev's own channel fails → tier exhausted (zen cooling down, mimo has
	// no channel).
	_, ok3 := NextTierCandidate(c, plan, 4)
	assert.False(t, ok3, "tier with every other candidate unusable must report exhaustion")
}

// TestNextTierCandidate_NeverReturnsTriedChannel protects the anti-ping-pong
// contract: once a channel has been attempted in this request (recorded in
// the context's use_channel list) it must never be offered again, so a
// A-fails -> B chain cannot bounce back to A when B fails too.
func TestNextTierCandidate_NeverReturnsTriedChannel(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	plan := tierPlan(
		tierCand("ali", 1, "glmA"),
		tierCand("zen", 2, "kimiB"),
	)
	// Channel 1 (ali) already attempted and failed earlier in this request.
	c.Set("use_channel", []string{"1"})

	got, ok := NextTierCandidate(c, plan, 1)
	require.True(t, ok)
	assert.Equal(t, "zen", got.ProviderId)
	assert.Equal(t, 2, got.ChannelId, "the untried channel is the failover target")

	// Channel 2 (zen) fails now too: with both channels attempted the tier is
	// exhausted — ali must not come back for a second round.
	c.Set("use_channel", []string{"1", "2"})
	_, ok = NextTierCandidate(c, plan, 2)
	assert.False(t, ok, "a channel already tried for this request must never be re-offered")
}

// TestNextTierCandidate_NonTierPlanIsNoop protects the non-tier regression
// surface: a plan without TierRequest never yields a failover candidate,
// even when it carries candidates.
func TestNextTierCandidate_NonTierPlanIsNoop(t *testing.T) {
	c := &gin.Context{}
	plan := &GatewayPlan{Candidates: []*Candidate{tierCand("ali", 1, "glmA")}}
	_, ok := NextTierCandidate(c, plan, 99)
	assert.False(t, ok)
}

// TestRewriteRequestBodyModel_SwitchesModel protects the body rewrite
// contract the relay retry loop depends on: after a tier failover the
// request body's model field must carry the new model so the downstream
// relay and the upstream call use it.
func TestRewriteRequestBodyModel_SwitchesModel(t *testing.T) {
	body := `{"model":"auto_high","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	c.Request.Header.Set("Content-Type", "application/json")

	storage, err := common.CreateBodyStorage([]byte(body))
	require.NoError(t, err)
	c.Set(common.KeyBodyStorage, storage)
	c.Request.Body = io.NopCloser(storage)

	RewriteRequestBodyModel(c, "kimiB")

	newStorage, err := common.GetBodyStorage(c)
	require.NoError(t, err)
	bs, err := newStorage.Bytes()
	require.NoError(t, err)
	assert.Contains(t, string(bs), `"model":"kimiB"`)
	assert.NotContains(t, string(bs), "auto_high")
	assert.Contains(t, string(bs), `"max_tokens":100`, "rewrite must not touch other fields")
}
