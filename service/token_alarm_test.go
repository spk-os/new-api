package service

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// resetAlarmState clears the minute bucket and dedupe map so tests are
// independent of execution order.
func resetAlarmState() {
	alarmMinuteMu.Lock()
	alarmMinuteBucket = 0
	alarmMinuteSum = 0
	alarmMinuteMu.Unlock()
	alarmDedupeMap = sync.Map{} // zero-value sync.Map is valid
}

func TestRecordMinuteTokensAccumulatesWithinMinute(t *testing.T) {
	resetAlarmState()

	// ts=60 and ts=90 fall in the same minute bucket [60,120).
	assert.Equal(t, int64(100), recordMinuteTokens(60, 100))
	assert.Equal(t, int64(300), recordMinuteTokens(90, 200))
}

func TestRecordMinuteTokensResetsOnNewMinute(t *testing.T) {
	resetAlarmState()

	assert.Equal(t, int64(100), recordMinuteTokens(60, 100))
	// ts=120 starts a new minute bucket → sum resets, then adds the incoming tokens.
	assert.Equal(t, int64(50), recordMinuteTokens(120, 50))
}

func TestShouldAlarmDedupesWithinWindow(t *testing.T) {
	resetAlarmState()
	base := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	alarmNowFunc = func() time.Time { return base }
	defer func() { alarmNowFunc = time.Now }()

	// First fire of a kind is allowed and records the next-allowed time.
	assert.True(t, shouldAlarm("one-req", 10*time.Minute))
	// Same kind within the window is suppressed.
	assert.False(t, shouldAlarm("one-req", 10*time.Minute))

	// Different kind is independent of the first.
	assert.True(t, shouldAlarm("per-minute", 10*time.Minute))
	assert.False(t, shouldAlarm("per-minute", 10*time.Minute))
}

func TestShouldAlarmAllowsAfterWindowExpires(t *testing.T) {
	resetAlarmState()
	base := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	now := base
	alarmNowFunc = func() time.Time { return now }
	defer func() { alarmNowFunc = time.Now }()

	assert.True(t, shouldAlarm("one-req", 10*time.Minute))
	assert.False(t, shouldAlarm("one-req", 10*time.Minute))

	// Advance past the dedupe window → allowed again.
	now = base.Add(11 * time.Minute)
	assert.True(t, shouldAlarm("one-req", 10*time.Minute))
}
