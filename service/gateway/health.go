package gateway

import (
	"sync"
	"time"
)

const (
	defaultFailureThreshold = 3
	defaultFailureWindowSec = 1200
	defaultCooldownSec      = 300
)

var providerHealthMap sync.Map

var healthMu sync.Mutex

func healthDefaults() (int, int, int) {
	threshold := defaultFailureThreshold
	window := defaultFailureWindowSec
	cooldown := defaultCooldownSec

	cfg := GetConfig()
	if cfg == nil {
		return threshold, window, cooldown
	}
	for _, group := range cfg.LLMGateway {
		if group == nil || group.LLMCommon == nil || group.LLMCommon.ProviderRetry == nil {
			continue
		}
		ps := group.LLMCommon.ProviderRetry.Provider
		if ps == nil {
			continue
		}
		if ps.FailureThreshold > 0 {
			threshold = ps.FailureThreshold
		}
		if ps.FailureWindow > 0 {
			window = ps.FailureWindow
		}
		if ps.CooldownPeriod > 0 {
			cooldown = ps.CooldownPeriod
		}
		break
	}
	return threshold, window, cooldown
}

// RecordProviderFailure increments failure count for a provider; when the
// count reaches the configured threshold within the failure window the
// provider is placed in cooldown.
func RecordProviderFailure(providerId string) {
	if providerId == "" {
		return
	}
	threshold, windowSec, cooldownSec := healthDefaults()
	now := nowFunc()

	healthMu.Lock()
	defer healthMu.Unlock()

	var h *ProviderHealth
	if v, ok := providerHealthMap.Load(providerId); ok {
		h, _ = v.(*ProviderHealth)
	}
	if h == nil {
		h = &ProviderHealth{}
	}

	if h.FailureWindowStart.IsZero() ||
		now.Sub(h.FailureWindowStart) > time.Duration(windowSec)*time.Second {
		h.FailureWindowStart = now
		h.FailureCount = 0
	}

	h.FailureCount++

	if h.FailureCount >= threshold {
		h.CooldownUntil = now.Add(time.Duration(cooldownSec) * time.Second)
	}
	providerHealthMap.Store(providerId, h)
}

// IsProviderInCooldown reports whether the provider is currently cooling down.
func IsProviderInCooldown(providerId string) bool {
	if providerId == "" {
		return false
	}
	v, ok := providerHealthMap.Load(providerId)
	if !ok {
		return false
	}
	h, _ := v.(*ProviderHealth)
	if h == nil {
		return false
	}
	return h.CooldownUntil.After(nowFunc())
}

// GetProviderHealth returns a snapshot of the current health entry, or nil
// when nothing is recorded for the provider.
func GetProviderHealth(providerId string) *ProviderHealth {
	if providerId == "" {
		return nil
	}
	v, ok := providerHealthMap.Load(providerId)
	if !ok {
		return nil
	}
	h, _ := v.(*ProviderHealth)
	if h == nil {
		return nil
	}
	cp := *h
	return &cp
}

// ResetProviderHealth clears failure count and cooldown for a provider.
func ResetProviderHealth(providerId string) {
	if providerId == "" {
		return
	}
	healthMu.Lock()
	defer healthMu.Unlock()
	providerHealthMap.Delete(providerId)
}

// GetCooldownProviders returns providerIds currently in cooldown.
func GetCooldownProviders() []string {
	now := nowFunc()
	var out []string
	providerHealthMap.Range(func(k, v interface{}) bool {
		pid, _ := k.(string)
		h, _ := v.(*ProviderHealth)
		if h != nil && h.CooldownUntil.After(now) {
			out = append(out, pid)
		}
		return true
	})
	return out
}

var nowFunc = time.Now
