package gateway

import (
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
)

// defaultModelNameMapping is the compiled-in fallback applied when the
// operator has not declared model_name_mapping in the gateway YAML. These
// route common Claude family prefixes to gateway-routable models so clients
// that send Sonnet/Opus/Haiku names hit the local providers without per-
// channel model_mapping. Override via the YAML field; set to [] to disable.
//
// Patterns are case-sensitive prefix matches (trailing `*`); to catch
// lowercase variants like `claude-sonnet-*`, configure `claude-sonnet*`.
var defaultModelNameMapping = []string{
	"Opus*: glm-5.3",
	"Sonnet*: ali_glm-5.2",
	"Haiku*: auto-free-height",
}

// DefaultModelNameMapping returns a copy of the compiled-in default mapping
// rules. Exported for introspection (admin UI / tests).
func DefaultModelNameMapping() []string {
	out := make([]string, len(defaultModelNameMapping))
	copy(out, defaultModelNameMapping)
	return out
}

// effectiveMappingRules resolves the rules to apply for the current request:
// configured ModelNameMapping when the operator declared it (including an
// explicit empty list to disable), otherwise the compiled-in defaults.
func effectiveMappingRules() []string {
	cfg := GetConfig()
	if cfg != nil && cfg.ModelNameMapping != nil {
		return cfg.ModelNameMapping
	}
	return defaultModelNameMapping
}

// splitMappingRule splits a "pattern: replacement" rule on the first colon
// and trims both halves. A rule without a colon yields an empty replacement,
// which validate() rejects at config-save time; this function stays
// defensive so a runtime-loaded bad rule is simply skipped.
func splitMappingRule(rule string) (pattern, replacement string) {
	idx := strings.Index(rule, ":")
	if idx < 0 {
		return strings.TrimSpace(rule), ""
	}
	return strings.TrimSpace(rule[:idx]), strings.TrimSpace(rule[idx+1:])
}

// matchPattern reports whether model matches pattern. A trailing `*` is a
// case-sensitive prefix match; any other pattern is an exact equality match.
func matchPattern(pattern, model string) bool {
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(model, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == model
}

// matchMapping returns the first rule's replacement whose pattern matches
// model, preserving list order so operators can express precedence. Empty
// replacements are treated as no-match.
func matchMapping(rules []string, model string) (replacement string, ok bool) {
	for _, rule := range rules {
		pattern, repl := splitMappingRule(rule)
		if pattern == "" || repl == "" {
			continue
		}
		if matchPattern(pattern, model) {
			return repl, true
		}
	}
	return "", false
}

// ModelNameMappingMiddleware rewrites the request body `model` field per the
// configured mapping rules before gateway routing and channel selection. It
// is a new-api-global capability (not gated by GatewayRoutingEnabled) so
// that clients sending alias/prefix model names — e.g. Claude family
// Sonnet/Opus/Haiku — reach a routable model without per-channel
// model_mapping. The rewrite happens early so the downstream distributor
// parses, whitelists, and bills against the mapped name.
//
// Register this middleware on relay routes BEFORE GatewayStrategyMiddleware
// and Distribute, so the strategy layer and channel selection both see the
// mapped model. No-op when there is no body `model` field or no rule matches.
func ModelNameMappingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		rules := effectiveMappingRules()
		if len(rules) == 0 {
			c.Next()
			return
		}
		model := requestedModelFrom(c)
		if model == "" {
			c.Next()
			return
		}
		repl, ok := matchMapping(rules, model)
		if !ok || repl == "" || repl == model {
			c.Next()
			return
		}
		// Preserve the client-supplied source for traceability before
		// rewriting — downstream original_model will hold the mapped name.
		common.SetContextKey(c, constant.ContextKeyModelMappedFrom, model)
		common.SysLog(fmt.Sprintf("gateway: model_name_mapping %q -> %q", model, repl))
		rewriteRequestBodyModel(c, repl)
		c.Next()
	}
}
