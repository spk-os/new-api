package gateway

import (
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Session-balanced provider selection.
//
// Requirement: when several providers share the same effective priority and a
// request carries a session identifier, distinct sessions should spread across
// the equal-priority providers instead of always landing on the first one,
// while each session stays sticky to its assigned provider.
//
// Implementation: a sessionId -> providerId assignment map. On each request we
// look at the first equal-priority block of the (already sorted) candidate
// list; a known session gets its assigned provider moved to the front (the
// strategy middleware picks the first resolvable candidate), a new or
// reassigned session gets the least-loaded provider in the block with a
// random tie-break. Candidates outside the first block are untouched, so
// priority semantics and failover order are preserved.

const (
	// sessionBalanceTTL is how long a session->provider assignment survives
	// without being seen again. Expired entries are deleted lazily during
	// load counting (new sessions) and binding refresh, which keeps the map
	// bounded by the number of recently active sessions.
	sessionBalanceTTL = 6 * time.Hour
)

type sessionBalanceEntry struct {
	providerId string
	lastSeen   time.Time
}

var (
	sessionBindings sync.Map // sessionId(string) -> *sessionBalanceEntry
	sessionNowFunc  = time.Now
)

// ExtractSessionId reads the client session identifier from request headers.
// X-Claude-Code-Session-Id is the primary source (Claude Code streams); the
// aliases give other agents the same balancing behavior.
func ExtractSessionId(c *gin.Context) string {
	for _, h := range []string{
		"X-Claude-Code-Session-Id",
		"X-Session-Id",
		"Session-Id",
	} {
		if v := strings.TrimSpace(c.GetHeader(h)); v != "" {
			return v
		}
	}
	return ""
}

// SessionBalanceCandidates reorders the first equal-priority block of cands so
// the provider assigned to sessionId comes first, assigning one when needed
// (least-loaded among the block, random tie-break). No-op when cands is empty,
// the first block has a single candidate, or sessionId is empty.
func SessionBalanceCandidates(cands []*Candidate, sessionId string) {
	if len(cands) <= 1 || sessionId == "" {
		return
	}

	// Block boundary is the (daily tier, provider order) pair: daily tier
	// scheduling sorts candidates by today's usage tier first (stable, so the
	// order sequence inside a tier is preserved), and sessions must not be
	// balanced across different tiers or orders. When tiers are off every
	// candidate has DailyTier 0, which reduces to the historical
	// same-order-only boundary.
	blockTier := cands[0].DailyTier
	blockOrder := effectiveProviderOrder(cands[0].ProviderId, nil)
	blockEnd := 1
	for blockEnd < len(cands) && cands[blockEnd].DailyTier == blockTier &&
		effectiveProviderOrder(cands[blockEnd].ProviderId, nil) == blockOrder {
		blockEnd++
	}
	if blockEnd <= 1 {
		return
	}

	inBlock := make(map[string]bool, blockEnd)
	for _, cand := range cands[:blockEnd] {
		inBlock[cand.ProviderId] = true
	}

	now := sessionNowFunc()
	providerId := assignedSessionProvider(sessionId, inBlock, now)
	if providerId == "" {
		providerId = pickLeastLoadedBlockProvider(inBlock, now)
		bindSessionToProvider(sessionId, providerId, now)
	}

	// Move the assigned provider's candidate(s) to the front of the block,
	// keeping the relative order of the rest stable.
	for i := 0; i < blockEnd; i++ {
		if cands[i].ProviderId == providerId {
			if i == 0 {
				return
			}
			chosen := cands[i]
			copy(cands[1:i+1], cands[0:i])
			cands[0] = chosen
			return
		}
	}
}

// assignedSessionProvider returns the bound provider if it is still a member
// of the block and its binding is fresh; otherwise clears the stale binding
// and returns "".
func assignedSessionProvider(sessionId string, inBlock map[string]bool, now time.Time) string {
	v, ok := sessionBindings.Load(sessionId)
	if !ok {
		return ""
	}
	e, _ := v.(*sessionBalanceEntry)
	if e == nil {
		sessionBindings.Delete(sessionId)
		return ""
	}
	if now.Sub(e.lastSeen) > sessionBalanceTTL || !inBlock[e.providerId] {
		sessionBindings.Delete(sessionId)
		return ""
	}
	// Re-store instead of mutating e.lastSeen in place: the entry pointer is
	// shared across concurrent requests for the same session.
	sessionBindings.Store(sessionId, &sessionBalanceEntry{providerId: e.providerId, lastSeen: now})
	return e.providerId
}

// pickLeastLoadedBlockProvider counts fresh bindings per block member and
// returns one with the minimum count; ties are broken randomly so brand-new
// sessions spread instead of all landing on the same provider.
func pickLeastLoadedBlockProvider(inBlock map[string]bool, now time.Time) string {
	load := make(map[string]int, len(inBlock))
	sessionBindings.Range(func(k, v interface{}) bool {
		e, _ := v.(*sessionBalanceEntry)
		if e == nil || now.Sub(e.lastSeen) > sessionBalanceTTL || !inBlock[e.providerId] {
			if e == nil || now.Sub(e.lastSeen) > sessionBalanceTTL {
				sessionBindings.Delete(k)
			}
			return true
		}
		load[e.providerId]++
		return true
	})
	minLoad := -1
	var mins []string
	for pid := range inBlock {
		n := load[pid]
		if minLoad < 0 || n < minLoad {
			minLoad = n
			mins = []string{pid}
		} else if n == minLoad {
			mins = append(mins, pid)
		}
	}
	return mins[rand.Intn(len(mins))]
}

func bindSessionToProvider(sessionId, providerId string, now time.Time) {
	sessionBindings.Store(sessionId, &sessionBalanceEntry{providerId: providerId, lastSeen: now})
}
