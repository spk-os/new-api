package gateway

import (
	"sync"
	"time"
)

type affinityStore struct {
	mu      sync.RWMutex
	entries map[string]*AffinityBindingRecord
}

var affinity = &affinityStore{entries: map[string]*AffinityBindingRecord{}}

func affinityKey(clientId, taskId string) string {
	return "gateway:affinity:" + clientId + ":" + taskId
}

// GetAffinityBinding fetches the active binding for (clientId, taskId), or nil if absent/expired.
func GetAffinityBinding(clientId, taskId string) *AffinityBindingRecord {
	if clientId == "" || taskId == "" {
		return nil
	}
	affinity.mu.RLock()
	rec := affinity.entries[affinityKey(clientId, taskId)]
	affinity.mu.RUnlock()
	if rec == nil {
		return nil
	}
	if !rec.ExpiresAt.IsZero() && time.Now().After(rec.ExpiresAt) {
		affinity.mu.Lock()
		delete(affinity.entries, affinityKey(clientId, taskId))
		affinity.mu.Unlock()
		return nil
	}
	return rec
}

// SetAffinityBinding stores or refreshes a binding.
func SetAffinityBinding(rec *AffinityBindingRecord, ttl time.Duration) {
	if rec == nil || rec.ClientId == "" || rec.TaskId == "" {
		return
	}
	if rec.BoundAt.IsZero() {
		rec.BoundAt = time.Now()
	}
	if ttl > 0 {
		rec.ExpiresAt = time.Now().Add(ttl)
	}
	affinity.mu.Lock()
	affinity.entries[affinityKey(rec.ClientId, rec.TaskId)] = rec
	affinity.mu.Unlock()
}

// ExtendAffinityBinding bumps ExpiresAt if extendOnAccess and within maxTTL.
func ExtendAffinityBinding(rec *AffinityBindingRecord, ttl, maxTTL time.Duration) {
	if rec == nil {
		return
	}
	now := time.Now()
	maxExpiry := rec.BoundAt.Add(maxTTL)
	newExpiry := now.Add(ttl)
	if maxTTL > 0 && newExpiry.After(maxExpiry) {
		newExpiry = maxExpiry
	}
	affinity.mu.Lock()
	rec.ExpiresAt = newExpiry
	affinity.entries[affinityKey(rec.ClientId, rec.TaskId)] = rec
	affinity.mu.Unlock()
}

// BreakAffinityBinding marks a binding broken (still readable for failover history).
func BreakAffinityBinding(clientId, taskId string) {
	affinity.mu.Lock()
	if rec, ok := affinity.entries[affinityKey(clientId, taskId)]; ok && rec != nil {
		rec.Broken = true
	}
	affinity.mu.Unlock()
}

// ListAffinityBindings returns a snapshot for inspection endpoints.
func ListAffinityBindings() []*AffinityBindingRecord {
	affinity.mu.RLock()
	defer affinity.mu.RUnlock()
	out := make([]*AffinityBindingRecord, 0, len(affinity.entries))
	for _, r := range affinity.entries {
		c := *r
		out = append(out, &c)
	}
	return out
}

// ClearAffinityBindings is intended for tests.
func ClearAffinityBindings() {
	affinity.mu.Lock()
	affinity.entries = map[string]*AffinityBindingRecord{}
	affinity.mu.Unlock()
}
