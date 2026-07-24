package gateway

import (
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

// compiledClientPattern is a pre-compiled client_id mapping entry.
type compiledClientPattern struct {
	name    string
	pattern *regexp.Regexp
}

var (
	compiledPatterns    []compiledClientPattern
	compiledPatternsMu  sync.RWMutex
	compiledPatternsCfg atomic.Pointer[GatewayYaml] // detects config changes
)

// patternToRegex converts a wildcard pattern to a compiled regexp.
// The pattern supports `*` (matches any sequence of characters) and is
// matched as a substring (contains). For example "Claude/*Chrome" becomes
// the regex `Claude\/.*Chrome` which matches any string containing
// "Claude/<anything>Chrome".
func patternToRegex(pattern string) (*regexp.Regexp, error) {
	parts := strings.Split(pattern, "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	regexStr := strings.Join(parts, ".*")
	return regexp.Compile(regexStr)
}

// compileClientIDPatterns extracts and compiles all client_id patterns from
// the config. Iterates every strategy group so patterns can be scoped per
// group, though in practice they are usually defined in the "default" group.
func compileClientIDPatterns(cfg *GatewayYaml) []compiledClientPattern {
	if cfg == nil {
		return nil
	}
	var patterns []compiledClientPattern
	for _, group := range cfg.LLMGateway {
		if group == nil || group.Log == nil || len(group.Log.ClientIDMap) == 0 {
			continue
		}
		for name, pattern := range group.Log.ClientIDMap {
			pattern = strings.TrimSpace(pattern)
			if pattern == "" {
				continue
			}
			re, err := patternToRegex(pattern)
			if err != nil {
				continue
			}
			patterns = append(patterns, compiledClientPattern{name: name, pattern: re})
		}
	}
	return patterns
}

// ResolveClientID maps a User-Agent / X-Agent-Name string to a friendly
// client ID using the gateway routing config's log.client_id patterns.
// Returns the original string if no pattern matches or no config is loaded.
func ResolveClientID(userAgent string) string {
	if userAgent == "" {
		return userAgent
	}

	cfg := GetConfig()
	if cfg == nil {
		return userAgent
	}

	// Fast path: patterns already compiled for this config pointer.
	compiledPatternsMu.RLock()
	patterns := compiledPatterns
	cachedCfg := compiledPatternsCfg.Load()
	compiledPatternsMu.RUnlock()

	if cachedCfg != cfg {
		// Config changed — recompile.
		patterns = compileClientIDPatterns(cfg)
		compiledPatternsMu.Lock()
		compiledPatterns = patterns
		compiledPatternsCfg.Store(cfg)
		compiledPatternsMu.Unlock()
	}

	if len(patterns) == 0 {
		return userAgent
	}

	for _, p := range patterns {
		if p.pattern.MatchString(userAgent) {
			return p.name
		}
	}
	return userAgent
}
