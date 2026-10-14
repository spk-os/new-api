package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestMatchPattern_PrefixAndExact(t *testing.T) {
	assert.True(t, matchPattern("Sonnet*", "Sonnet-4"), "trailing * is prefix match")
	assert.True(t, matchPattern("Sonnet*", "Sonnet"), "prefix matches bare prefix")
	assert.False(t, matchPattern("Sonnet*", "claude-sonnet-4"), "prefix match is case-sensitive, not contains")
	assert.False(t, matchPattern("Sonnet*", "Opus-4"))
	assert.True(t, matchPattern("gpt-4", "gpt-4"), "no wildcard is exact match")
	assert.False(t, matchPattern("gpt-4", "gpt-4-turbo"))
}

func TestSplitMappingRule(t *testing.T) {
	p, r := splitMappingRule("Sonnet*: glm-5.2")
	assert.Equal(t, "Sonnet*", p)
	assert.Equal(t, "glm-5.2", r)

	// inline no-space form (the user's example syntax)
	p, r = splitMappingRule("Sonnet*:auto-coding")
	assert.Equal(t, "Sonnet*", p)
	assert.Equal(t, "auto-coding", r)

	p, r = splitMappingRule("no-colon")
	assert.Equal(t, "no-colon", p)
	assert.Equal(t, "", r, "no colon -> empty replacement")
}

func TestMatchMapping_FirstWins(t *testing.T) {
	rules := []string{
		"Sonnet-4*: glm-5.3", // more specific first
		"Sonnet*: glm-5.2",
	}
	repl, ok := matchMapping(rules, "Sonnet-4.5")
	require.True(t, ok)
	assert.Equal(t, "glm-5.3", repl, "first matching rule wins")

	repl, ok = matchMapping(rules, "Sonnet-9")
	require.True(t, ok)
	assert.Equal(t, "glm-5.2", repl)

	_, ok = matchMapping(rules, "Opus-4")
	assert.False(t, ok)
}

func TestMatchMapping_SkipsMalformed(t *testing.T) {
	rules := []string{
		"badrule-no-colon", // skipped: empty replacement
		"Sonnet*: glm-5.2",
	}
	repl, ok := matchMapping(rules, "Sonnet-4")
	require.True(t, ok)
	assert.Equal(t, "glm-5.2", repl)
}

func TestEffectiveMappingRules_DefaultsWhenUnset(t *testing.T) {
	// cfg nil -> compiled-in defaults
	setTestConfig(t, nil)
	rules := effectiveMappingRules()
	assert.Contains(t, rules, "Opus*: glm-5.3")
	assert.Contains(t, rules, "Sonnet*: ali_glm-5.2")
	assert.Contains(t, rules, "Haiku*: auto-free-height")

	// cfg present but field absent (nil) -> still defaults
	cfg := twoProviderConfig() // has no ModelNameMapping
	setTestConfig(t, cfg)
	rules = effectiveMappingRules()
	assert.NotEmpty(t, rules)
	assert.Contains(t, rules, "Sonnet*: ali_glm-5.2")
}

func TestEffectiveMappingRules_OperatorOverride(t *testing.T) {
	cfg := twoProviderConfig()
	cfg.ModelNameMapping = []string{"Foo*: bar"}
	setTestConfig(t, cfg)
	rules := effectiveMappingRules()
	assert.Equal(t, []string{"Foo*: bar"}, rules, "operator rules override defaults")

	// explicit empty list -> disabled (returns empty, non-nil)
	cfg.ModelNameMapping = []string{}
	setTestConfig(t, cfg)
	rules = effectiveMappingRules()
	assert.Empty(t, rules)
}

func TestDefaultModelNameMapping_IsImmutable(t *testing.T) {
	a := DefaultModelNameMapping()
	a[0] = "MUTATED*: x"
	b := DefaultModelNameMapping()
	assert.NotEqual(t, "MUTATED*: x", b[0], "caller must not mutate the package-level default")
}

// newMappingContext builds a gin context with the given JSON body and body
// storage initialized, matching what the real middleware chain provides.
func newMappingContext(t *testing.T, body string) *gin.Context {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	storage, err := common.CreateBodyStorage([]byte(body))
	require.NoError(t, err, "CreateBodyStorage")
	c.Set(common.KeyBodyStorage, storage)
	c.Request.Body = io.NopCloser(storage)
	return c
}

func bodyModel(t *testing.T, c *gin.Context) string {
	t.Helper()
	storage, err := common.GetBodyStorage(c)
	require.NoError(t, err, "GetBodyStorage")
	bs, err := storage.Bytes()
	require.NoError(t, err, "Bytes")
	return gjson.GetBytes(bs, "model").String()
}

func TestModelNameMappingMiddleware_RewritesBody(t *testing.T) {
	// Field absent from config -> defaults apply (Sonnet* -> ali_glm-5.2).
	setTestConfig(t, twoProviderConfig())
	c := newMappingContext(t, `{"model":"Sonnet-4.5","messages":[{"role":"user","content":"hi"}]}`)

	ModelNameMappingMiddleware()(c)

	assert.Equal(t, "ali_glm-5.2", bodyModel(t, c), "Sonnet* prefix should rewrite to ali_glm-5.2")
	assert.Equal(t, "Sonnet-4.5", common.GetContextKeyString(c, constant.ContextKeyModelMappedFrom),
		"original name preserved in trace key")

	// messages preserved
	storage, _ := common.GetBodyStorage(c)
	bs, _ := storage.Bytes()
	assert.Equal(t, "hi", gjson.GetBytes(bs, "messages.0.content").String())
}

func TestModelNameMappingMiddleware_HaikuMapsToVirtual(t *testing.T) {
	setTestConfig(t, twoProviderConfig())
	c := newMappingContext(t, `{"model":"Haiku-flash","messages":[]}`)
	ModelNameMappingMiddleware()(c)
	assert.Equal(t, "auto-free-height", bodyModel(t, c))
}

func TestModelNameMappingMiddleware_NoMatchNoRewrite(t *testing.T) {
	setTestConfig(t, twoProviderConfig())
	original := `{"model":"deepseek-v4-pro","messages":[]}`
	c := newMappingContext(t, original)
	ModelNameMappingMiddleware()(c)
	assert.Equal(t, "deepseek-v4-pro", bodyModel(t, c))
	_, set := common.GetContextKey(c, constant.ContextKeyModelMappedFrom)
	assert.False(t, set, "trace key must not be set when no rule matched")
}

func TestModelNameMappingMiddleware_DisabledByEmptyList(t *testing.T) {
	cfg := twoProviderConfig()
	cfg.ModelNameMapping = []string{} // operator disabled
	setTestConfig(t, cfg)
	c := newMappingContext(t, `{"model":"Sonnet-4","messages":[]}`)
	ModelNameMappingMiddleware()(c)
	assert.Equal(t, "Sonnet-4", bodyModel(t, c), "empty list disables mapping")
}

func TestModelNameMappingMiddleware_NoModelFieldIsNoop(t *testing.T) {
	setTestConfig(t, twoProviderConfig())
	original := `{"messages":[{"role":"user","content":"hi"}]}`
	c := newMappingContext(t, original)
	ModelNameMappingMiddleware()(c)
	storage, _ := common.GetBodyStorage(c)
	bs, _ := storage.Bytes()
	assert.JSONEq(t, original, string(bs), "body unchanged without a model field")
}
