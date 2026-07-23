package service

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetCooldownState() {
	channelCooldownMap = sync.Map{}
	channelCooldownMu = sync.Mutex{}
}

func setNow(t time.Time) {
	channelNowFunc = func() time.Time { return t }
}

func TestRecordChannelFailure_429TriggersImmediateCooldown(t *testing.T) {
	resetCooldownState()
	now := time.Now()
	setNow(now)

	RecordChannelFailure(15, 429)

	assert.True(t, IsChannelInCooldown(15), "429 should trigger immediate cooldown")
	ids := GetCooldownChannelIds()
	assert.Contains(t, ids, 15)
}

func TestRecordChannelFailure_402TriggersImmediateCooldown(t *testing.T) {
	resetCooldownState()
	now := time.Now()
	setNow(now)

	RecordChannelFailure(10, 402)

	assert.True(t, IsChannelInCooldown(10), "402 should trigger immediate cooldown")
}

func TestRecordChannelFailure_500NeedsThreeFailures(t *testing.T) {
	resetCooldownState()
	now := time.Now()
	setNow(now)

	RecordChannelFailure(1, 500)
	assert.False(t, IsChannelInCooldown(1), "first 500 should not trigger cooldown")

	RecordChannelFailure(1, 500)
	assert.False(t, IsChannelInCooldown(1), "second 500 should not trigger cooldown")

	RecordChannelFailure(1, 500)
	assert.True(t, IsChannelInCooldown(1), "third 500 should trigger cooldown")
}

func TestRecordChannelSuccessClearsCooldown(t *testing.T) {
	resetCooldownState()
	now := time.Now()
	setNow(now)

	RecordChannelFailure(15, 429)
	require.True(t, IsChannelInCooldown(15))

	RecordChannelSuccess(15)
	assert.False(t, IsChannelInCooldown(15), "success should clear cooldown")
}

func TestCooldownExpiresAfterDuration(t *testing.T) {
	resetCooldownState()
	now := time.Now()
	setNow(now)

	RecordChannelFailure(15, 429)
	require.True(t, IsChannelInCooldown(15))

	setNow(now.Add(time.Duration(CooldownDurationSec+1) * time.Second))
	assert.False(t, IsChannelInCooldown(15), "cooldown should expire after duration")
}

func TestCooldownFallback_GetCooldownChannelIds(t *testing.T) {
	resetCooldownState()
	now := time.Now()
	setNow(now)

	RecordChannelFailure(15, 429)
	RecordChannelFailure(1, 429)

	ids := GetCooldownChannelIds()
	assert.Len(t, ids, 2)
	assert.Contains(t, ids, 15)
	assert.Contains(t, ids, 1)
}

func TestFailureWindowResets(t *testing.T) {
	resetCooldownState()
	now := time.Now()
	setNow(now)

	RecordChannelFailure(1, 500)
	setNow(now.Add(time.Duration(FailureWindowSec+1) * time.Second))

	RecordChannelFailure(1, 500)
	assert.False(t, IsChannelInCooldown(1), "failure count should reset after window expires")
}
