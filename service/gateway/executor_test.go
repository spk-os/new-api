package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func newTestCtx() *gin.Context {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c
}

func resetExecutorState() {
	providerHealthMap = sync.Map{}
	rateLimitState = sync.Map{}
	nowFunc = time.Now
	sleepFunc = func(time.Duration) {}
	SetConfig(nil)
}

func newPlan(cands ...*Candidate) *GatewayPlan {
	return &GatewayPlan{
		Candidates: cands,
		Retry: &ProviderRetry{
			Enabled: true,
			Model: &ModelRetry{
				MaxRetries:              2,
				BackoffIntervals:        []int{0, 0, 0},
				RetryableStatusCodes:    []int{500, 503},
				NonRetryableStatusCodes: []int{401, 403},
				RetryOnTimeout:          true,
				RetryByKey:              true,
			},
			Provider: &ProviderSwitch{SwitchDelay: 0},
			Global:   &GlobalRetry{MaxTotalRetries: 30, MaxTotalTimeout: 1200},
		},
	}
}

func errAt(code int) *dto.OpenAIErrorWithStatusCode {
	return &dto.OpenAIErrorWithStatusCode{
		StatusCode: code,
		Error:      types.OpenAIError{Message: "boom", Type: "test"},
	}
}

func TestExecuteWithPlan_SingleCandidateSuccess(t *testing.T) {
	resetExecutorState()
	c := newTestCtx()
	cand := &Candidate{ProviderId: "zen", Keys: []string{"k1"}, ActualModel: "m1", ModelGroup: "g"}
	plan := newPlan(cand)
	calls := 0
	resp, err := ExecuteWithPlan(c, plan, func(_ *Candidate) (*http.Response, *dto.OpenAIErrorWithStatusCode) {
		calls++
		return &http.Response{StatusCode: 200}, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || resp.StatusCode != 200 {
		t.Fatalf("expected 200 response, got %+v", resp)
	}
	if calls != 1 {
		t.Fatalf("doOnce calls = %d, want 1", calls)
	}
}

func TestExecuteWithPlan_RetryOnRetryableStatus(t *testing.T) {
	resetExecutorState()
	c := newTestCtx()
	cand := &Candidate{ProviderId: "zen", Keys: []string{"k1"}, ActualModel: "m1"}
	plan := newPlan(cand)
	calls := 0
	resp, err := ExecuteWithPlan(c, plan, func(_ *Candidate) (*http.Response, *dto.OpenAIErrorWithStatusCode) {
		calls++
		if calls < 3 {
			return nil, errAt(500)
		}
		return &http.Response{StatusCode: 200}, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %+v", resp)
	}
	if calls != 3 {
		t.Fatalf("calls=%d, want 3", calls)
	}
}

func TestExecuteWithPlan_NonRetryableSwitchesKey(t *testing.T) {
	resetExecutorState()
	c := newTestCtx()
	cand := &Candidate{ProviderId: "zen", Keys: []string{"k1", "k2"}, ActualModel: "m1"}
	plan := newPlan(cand)
	seenKeys := []int{}
	calls := 0
	resp, err := ExecuteWithPlan(c, plan, func(c *Candidate) (*http.Response, *dto.OpenAIErrorWithStatusCode) {
		calls++
		seenKeys = append(seenKeys, c.KeyIndex)
		if c.KeyIndex == 0 {
			return nil, errAt(401)
		}
		return &http.Response{StatusCode: 200}, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2 (no retry on 401, switch to next key)", calls)
	}
	if seenKeys[0] != 0 || seenKeys[1] != 1 {
		t.Fatalf("seenKeys=%v, want [0,1]", seenKeys)
	}
}

func TestExecuteWithPlan_SwitchProviderAfterCandidateExhausted(t *testing.T) {
	resetExecutorState()
	c := newTestCtx()
	c1 := &Candidate{ProviderId: "zen", Keys: []string{"k1"}, ActualModel: "m1"}
	c2 := &Candidate{ProviderId: "ali", Keys: []string{"k2"}, ActualModel: "m1"}
	plan := newPlan(c1, c2)
	plan.Retry.Model.MaxRetries = 0

	resp, err := ExecuteWithPlan(c, plan, func(c *Candidate) (*http.Response, *dto.OpenAIErrorWithStatusCode) {
		if c.ProviderId == "zen" {
			return nil, errAt(500)
		}
		return &http.Response{StatusCode: 200}, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if GetProviderHealth("zen") == nil {
		t.Fatal("expected RecordProviderFailure for zen")
	}
}

func TestExecuteWithPlan_AffinityNoFailoverFails(t *testing.T) {
	resetExecutorState()
	c := newTestCtx()
	c1 := &Candidate{ProviderId: "zen", Keys: []string{"k1"}, ActualModel: "m1"}
	c2 := &Candidate{ProviderId: "ali", Keys: []string{"k2"}, ActualModel: "m1"}
	plan := newPlan(c1, c2)
	plan.AffinityHit = true
	plan.Affinity = &AffinityConfig{Failover: &AffinityFailover{Enable: false}}

	_, err := ExecuteWithPlan(c, plan, func(_ *Candidate) (*http.Response, *dto.OpenAIErrorWithStatusCode) {
		return nil, errAt(500)
	})
	if !errors.Is(err, ErrAffinityNoFailover) {
		t.Fatalf("expected ErrAffinityNoFailover, got %v", err)
	}
}

func TestExecuteWithPlan_AffinityFailoverSameModelReorder(t *testing.T) {
	resetExecutorState()
	c := newTestCtx()
	bound := &Candidate{ProviderId: "zen", Keys: []string{"k1"}, ActualModel: "qwen3.7-max"}
	other := &Candidate{ProviderId: "ali", Keys: []string{"k2"}, ActualModel: "deepseek-v3"}
	sameModel := &Candidate{ProviderId: "nv", Keys: []string{"k3"}, ActualModel: "qwen3.7-max"}
	series := &Candidate{ProviderId: "ms", Keys: []string{"k4"}, ActualModel: "qwen3.7-plus"}

	plan := newPlan(bound, other, sameModel, series)
	plan.AffinityHit = true
	plan.Affinity = &AffinityConfig{Failover: &AffinityFailover{
		Enable:       true,
		Priority:     "sameModel",
		NotifyClient: false,
	}}

	calls := []string{}
	_, err := ExecuteWithPlan(c, plan, func(cand *Candidate) (*http.Response, *dto.OpenAIErrorWithStatusCode) {
		calls = append(calls, cand.ProviderId)
		if cand.ProviderId == "nv" {
			return &http.Response{StatusCode: 200}, nil
		}
		return nil, errAt(500)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	hitNv := false
	for _, p := range calls {
		if p == "nv" {
			hitNv = true
			break
		}
	}
	if !hitNv {
		t.Fatalf("expected nv to be tried, calls=%v", calls)
	}
	idxNv := -1
	idxOther := -1
	for i, p := range calls {
		if p == "nv" && idxNv < 0 {
			idxNv = i
		}
		if p == "ali" && idxOther < 0 {
			idxOther = i
		}
	}
	if idxOther >= 0 && idxNv > idxOther {
		t.Fatalf("sameModel candidate (nv) should come before other (ali) after reorder, calls=%v", calls)
	}
}

func TestExecuteWithPlan_MaxTotalRetriesEnforced(t *testing.T) {
	resetExecutorState()
	c := newTestCtx()
	cand := &Candidate{ProviderId: "zen", Keys: []string{"k1"}, ActualModel: "m1"}
	plan := newPlan(cand)
	plan.Retry.Model.MaxRetries = 100
	plan.Retry.Global.MaxTotalRetries = 3

	calls := 0
	_, err := ExecuteWithPlan(c, plan, func(_ *Candidate) (*http.Response, *dto.OpenAIErrorWithStatusCode) {
		calls++
		return nil, errAt(500)
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrRetriesExhausted) {
		t.Fatalf("expected ErrRetriesExhausted, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls=%d, want 3 (MaxTotalRetries)", calls)
	}
}

func TestExecuteWithPlan_DeadlineEnforced(t *testing.T) {
	resetExecutorState()
	cur := time.Unix(1_700_000_000, 0)
	nowFunc = func() time.Time { return cur }
	t.Cleanup(func() { nowFunc = time.Now })

	c := newTestCtx()
	cand := &Candidate{ProviderId: "zen", Keys: []string{"k1"}, ActualModel: "m1"}
	plan := newPlan(cand)
	plan.Retry.Model.MaxRetries = 100
	plan.Retry.Global.MaxTotalTimeout = 1

	calls := 0
	_, err := ExecuteWithPlan(c, plan, func(_ *Candidate) (*http.Response, *dto.OpenAIErrorWithStatusCode) {
		calls++
		cur = cur.Add(2 * time.Second)
		return nil, errAt(500)
	})
	if !errors.Is(err, ErrRetriesExhausted) {
		t.Fatalf("expected ErrRetriesExhausted from deadline, got %v", err)
	}
	if calls > 2 {
		t.Fatalf("calls=%d, want <=2 (deadline enforced)", calls)
	}
}

func TestExecuteWithPlan_ResponseAlreadyWritten(t *testing.T) {
	resetExecutorState()
	c := newTestCtx()
	c.Set(ContextKeyGatewayResponseWritten, true)

	cand := &Candidate{ProviderId: "zen", Keys: []string{"k1"}, ActualModel: "m1"}
	plan := newPlan(cand)
	_, err := ExecuteWithPlan(c, plan, func(_ *Candidate) (*http.Response, *dto.OpenAIErrorWithStatusCode) {
		return nil, errAt(500)
	})
	if !errors.Is(err, ErrResponseAlreadyWritten) {
		t.Fatalf("expected ErrResponseAlreadyWritten, got %v", err)
	}
}

func TestReorderByFailoverPriority_SameProvider(t *testing.T) {
	failed := &Candidate{ProviderId: "zen", ActualModel: "m1"}
	a := &Candidate{ProviderId: "zen", ActualModel: "m2"}
	b := &Candidate{ProviderId: "ali", ActualModel: "m3"}
	plan := &GatewayPlan{
		Candidates: []*Candidate{failed, b, a},
		Affinity:   &AffinityConfig{Failover: &AffinityFailover{Priority: "sameProvider"}},
	}
	reorderByFailoverPriority(plan, failed)
	if len(plan.Candidates) != 2 {
		t.Fatalf("expected 2 remaining, got %d", len(plan.Candidates))
	}
	if plan.Candidates[0] != a {
		t.Fatalf("sameProvider should come first, got %+v", plan.Candidates[0])
	}
}

func TestModelSeriesPrefix(t *testing.T) {
	cases := map[string]string{
		"qwen3.7-max": "qwen3",
		"deepseek_v3": "deepseek",
		"plain":       "plain",
		"":            "",
		"-leading":    "-leading",
		"a-b":         "a",
	}
	for in, want := range cases {
		if got := modelSeriesPrefix(in); got != want {
			t.Errorf("modelSeriesPrefix(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestCalcRequestCost(t *testing.T) {
	resetExecutorState()
	cfg := &GatewayYaml{
		Cost: &CostConfig{
			Default: &ModelCost{InputPer1kTokens: 0.001, OutputPer1kTokens: 0.002},
			Models: map[string]*ModelCost{
				"premium": {InputPer1kTokens: 0.01, OutputPer1kTokens: 0.03},
			},
		},
	}
	SetConfig(cfg)
	t.Cleanup(func() { SetConfig(nil) })

	usage := &dto.Usage{PromptTokens: 1000, CompletionTokens: 500}
	got := CalcRequestCost("premium", usage)
	want := 1000.0/1000.0*0.01 + 500.0/1000.0*0.03
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}

	got = CalcRequestCost("unknown-model", usage)
	want = 1000.0/1000.0*0.001 + 500.0/1000.0*0.002
	if got != want {
		t.Errorf("default cost: got %v, want %v", got, want)
	}

	if got := CalcRequestCost("any", nil); got != 0 {
		t.Errorf("nil usage should be 0, got %v", got)
	}
}
