package gateway

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type RateLimitEntry struct {
	WindowStart        time.Time
	RequestCount       int64
	CurrentConcurrency int32
}

var rateLimitState sync.Map

var rateLimitMu sync.Mutex

// GetEffectiveRateLimiting returns the first non-nil rate limiting config in
// the order: provider's modelGroup → provider → profile.RateLimiting (llmCommon).
func GetEffectiveRateLimiting(profile *EffectiveProfile, providerId, modelGroupName string) *RateLimiting {
	cfg := GetConfig()
	if cfg != nil && providerId != "" {
		if p, ok := cfg.Providers[providerId]; ok && p != nil {
			if modelGroupName != "" {
				for _, mg := range p.Models {
					if mg != nil && mg.Name == modelGroupName && mg.RateLimiting != nil {
						return mg.RateLimiting
					}
				}
			}
			if p.RateLimiting != nil {
				return p.RateLimiting
			}
		}
	}
	if profile != nil && profile.RateLimiting != nil {
		return profile.RateLimiting
	}
	return nil
}

// applyRateLimit filters candidates based on effective rate limiting.
// Reject strategy removes over-limit candidates; queue/downgrade keep them.
func applyRateLimit(candidates []*Candidate, profile *EffectiveProfile) []*Candidate {
	if len(candidates) == 0 {
		return candidates
	}
	out := make([]*Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c == nil {
			continue
		}
		rl := GetEffectiveRateLimiting(profile, c.ProviderId, c.ModelGroup)
		if rl == nil || !rl.Enabled {
			out = append(out, c)
			continue
		}
		key := rateLimitKey(c.ProviderId, c.ModelGroup)
		allowed, strategy := CheckRateLimit(key, rl)
		if allowed {
			out = append(out, c)
			continue
		}
		switch strategy {
		case "reject":
			continue
		case "queue", "downgrade":
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}

// CheckRateLimit reports whether a request is allowed for key under rl.
// When not allowed, the second return value is the configured overLimitStrategy.
func CheckRateLimit(key string, rl *RateLimiting) (bool, string) {
	if rl == nil || !rl.Enabled {
		return true, ""
	}

	rateLimitMu.Lock()
	defer rateLimitMu.Unlock()

	var entry *RateLimitEntry
	if v, ok := rateLimitState.Load(key); ok {
		entry, _ = v.(*RateLimitEntry)
	}
	if entry == nil {
		entry = &RateLimitEntry{}
		rateLimitState.Store(key, entry)
	}

	now := nowFunc()

	if rl.WindowRate != "" {
		windowDur, maxReq, err := parseWindowRate(rl.WindowRate)
		if err == nil && maxReq > 0 {
			if entry.WindowStart.IsZero() || now.Sub(entry.WindowStart) >= windowDur {
				entry.WindowStart = now
				atomic.StoreInt64(&entry.RequestCount, 0)
			}
			if atomic.LoadInt64(&entry.RequestCount) >= maxReq {
				return false, rl.OverLimitStrategy
			}
		}
	}

	if rl.Concurrency > 0 {
		if int(atomic.LoadInt32(&entry.CurrentConcurrency)) >= rl.Concurrency {
			return false, rl.OverLimitStrategy
		}
	}

	return true, ""
}

// IncrementRateLimitCount records a new request against the window count and
// in-flight concurrency for key.
func IncrementRateLimitCount(key string) {
	if key == "" {
		return
	}
	rateLimitMu.Lock()
	defer rateLimitMu.Unlock()

	var entry *RateLimitEntry
	if v, ok := rateLimitState.Load(key); ok {
		entry, _ = v.(*RateLimitEntry)
	}
	if entry == nil {
		entry = &RateLimitEntry{WindowStart: nowFunc()}
		rateLimitState.Store(key, entry)
	}
	atomic.AddInt64(&entry.RequestCount, 1)
	atomic.AddInt32(&entry.CurrentConcurrency, 1)
}

// DecrementConcurrency releases an in-flight slot when a request finishes.
func DecrementConcurrency(key string) {
	if key == "" {
		return
	}
	v, ok := rateLimitState.Load(key)
	if !ok {
		return
	}
	entry, _ := v.(*RateLimitEntry)
	if entry == nil {
		return
	}
	if atomic.LoadInt32(&entry.CurrentConcurrency) > 0 {
		atomic.AddInt32(&entry.CurrentConcurrency, -1)
	}
}

// parseWindowRate parses values like "1h-100", "30m-50", "10s-5", "1d-1000"
// into (window duration, max requests).
func parseWindowRate(windowRate string) (time.Duration, int64, error) {
	s := strings.TrimSpace(windowRate)
	if s == "" {
		return 0, 0, fmt.Errorf("empty windowRate")
	}
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid windowRate %q: expected <window>-<count>", windowRate)
	}
	winStr := strings.TrimSpace(parts[0])
	countStr := strings.TrimSpace(parts[1])
	if len(winStr) < 2 {
		return 0, 0, fmt.Errorf("invalid window %q", winStr)
	}
	unit := winStr[len(winStr)-1]
	numStr := winStr[:len(winStr)-1]
	num, err := strconv.Atoi(numStr)
	if err != nil || num <= 0 {
		return 0, 0, fmt.Errorf("invalid window number %q", numStr)
	}
	var dur time.Duration
	switch unit {
	case 's':
		dur = time.Duration(num) * time.Second
	case 'm':
		dur = time.Duration(num) * time.Minute
	case 'h':
		dur = time.Duration(num) * time.Hour
	case 'd':
		dur = time.Duration(num) * 24 * time.Hour
	default:
		return 0, 0, fmt.Errorf("invalid window unit %q (want s/m/h/d)", string(unit))
	}
	count, err := strconv.ParseInt(countStr, 10, 64)
	if err != nil || count <= 0 {
		return 0, 0, fmt.Errorf("invalid request count %q", countStr)
	}
	return dur, count, nil
}

func rateLimitKey(providerId, modelGroup string) string {
	return providerId + ":" + modelGroup
}
