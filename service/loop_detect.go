package service

// Loop-call detection — guards against a misbehaving client (e.g. a tool
// stuck in a retry loop) hammering the same model with near-identical large
// requests that burn tokens.
//
// Rule (all four thresholds operator-tunable via gateway alarm config):
//   - Learning phase: within LoopWindowMs, LoopMinHits hits from the same
//     client+model where each hit has tokens >= LoopMinTokens, response time
//     < LoopMaxRespMs, and mutual request-size difference < LoopSizeDiffPct%
//     → mark the (client, model) source as anomalous and fire a detect alarm.
//   - Block phase: once flagged, a subsequent hit is penalised (sleep
//     LoopSleepMs + alarm) only when all four conditions hold simultaneously
//     right now — at least LoopMinHits hits still in the window, this hit
//     meets the token threshold, this hit is faster than the response cap,
//     and this hit's size matches the learned base within LoopSizeDiffPct%.
//     Requiring the full signature at trigger time (not just size + tokens)
//     means a single transient flagging batch cannot punish legitimate slow
//     traffic afterwards; a normal long streaming turn fails the response cap
//     and is never slept. The flag also self-clears when the window empties.
//
// State is in-memory only (no DB), keyed by client+model. Alarms dedupe per
// window via the shared Alarm.Interval window so a sustained loop does not
// flood Feishu. loopNowFunc/loopSleepFunc are overridable in tests.

import (
	"fmt"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/gateway"

	"github.com/gin-gonic/gin"
)

// loopClientCtxKey mirrors model.LoopClientCtxKey, the context key set in
// model.RecordConsumeLog once the client identity is resolved (X-Agent-Name /
// User-Agent + ClientIDResolver), so this package reuses the exact same value
// as the consume log without re-deriving it.
const loopClientCtxKey = model.LoopClientCtxKey

type loopHit struct {
	ts      int64 // unix milliseconds
	tokens  int64
	respMs  int64
	reqSize int64
}

type loopWindow struct {
	mu   sync.Mutex
	hits []loopHit
	// flagged sources are in the block phase: subsequent matching hits sleep.
	flagged   bool
	baseSize  int64 // learned average reqSize of the anomalous batch
	flaggedAt time.Time
	// per-window alarm dedupe timestamps (reuse Alarm.Interval window).
	lastDetectAlarm time.Time
	lastBlockAlarm  time.Time
}

var (
	loopWindows   sync.Map // key = client + "\x00" + model -> *loopWindow
	loopNowFunc   = time.Now
	loopSleepFunc = loopSleepProduction
)

// loopSleepProduction sleeps for d but returns early when the request context
// is cancelled, so a client disconnect does not leave a goroutine blocked for
// the full penalty duration.
func loopSleepProduction(c *gin.Context, d time.Duration) {
	if c != nil && c.Request != nil {
		ctx := c.Request.Context()
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
		return
	}
	time.Sleep(d)
}

// checkLoopCall runs the loop-call detection for a single recorded request. It
// is called from checkTokenAlarm (same AfterRecordConsumeLogHook trigger) so
// loop detection shares the one-request-one-log hook without adding a second
// model→service hook.
func checkLoopCall(c *gin.Context, userId int, createdAt int64, params model.RecordConsumeLogParams) {
	cfg := gateway.GetConfig()
	if cfg == nil || cfg.Alarm == nil {
		return
	}
	alarm := cfg.Alarm
	// LoopMinTokens==0 lets operators disable loop detection entirely; a zero
	// MinHits would make the learning rule fire on the first request.
	if alarm.LoopMinTokens <= 0 || alarm.LoopMinHits <= 0 {
		return
	}

	totalTokens := int64(params.PromptTokens) + int64(params.CompletionTokens) + int64(params.CacheTokens)
	// Below the token threshold: neither learn from nor penalise. A source
	// only matters once it starts burning large per-request token counts.
	if totalTokens < alarm.LoopMinTokens {
		return
	}

	client := ""
	if c != nil {
		client = c.GetString(loopClientCtxKey)
	}
	if client == "" {
		client = "unknown"
	}
	modelName := params.ModelName
	if modelName == "" {
		return
	}

	reqSize := int64(0)
	if c != nil && c.Request != nil {
		reqSize = c.Request.ContentLength
	}

	respMs := int64(0)
	if c != nil {
		start := common.GetContextKeyTime(c, constant.ContextKeyRequestStartTime)
		if !start.IsZero() {
			respMs = loopNowFunc().Sub(start).Milliseconds()
		}
	}
	if respMs <= 0 && params.UseTimeSeconds > 0 {
		respMs = int64(params.UseTimeSeconds) * 1000
	}

	now := loopNowFunc()
	interval := time.Duration(alarm.Interval) * time.Millisecond
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	windowMs := alarm.LoopWindowMs
	if windowMs <= 0 {
		windowMs = 60000
	}

	key := client + "\x00" + modelName
	wAny, _ := loopWindows.LoadOrStore(key, &loopWindow{})
	w, _ := wAny.(*loopWindow)

	w.mu.Lock()
	defer w.mu.Unlock()

	// Prune hits outside the sliding window. Reusing the backing array keeps
	// allocations down under sustained load.
	cutoff := now.Add(-time.Duration(windowMs) * time.Millisecond).UnixMilli()
	kept := w.hits[:0]
	for _, h := range w.hits {
		if h.ts >= cutoff {
			kept = append(kept, h)
		}
	}
	w.hits = kept

	// Recovery: if no hits survive the sliding window, the anomalous burst
	// has ended — clear the learned flag and base size so a single transient
	// batch cannot penalise the same source forever. The flag persists only
	// as long as the source keeps hitting within the window.
	if w.flagged && len(w.hits) == 0 {
		w.flagged = false
		w.baseSize = 0
	}

	hit := loopHit{ts: now.UnixMilli(), tokens: totalTokens, respMs: respMs, reqSize: reqSize}
	requestId := ""
	if c != nil {
		requestId = c.GetString(common.RequestIdKey)
	}

	if !w.flagged {
		// Learning phase: record the hit, then test the most recent
		// LoopMinHits against the anomaly rule.
		w.hits = append(w.hits, hit)
		minHits := alarm.LoopMinHits
		if len(w.hits) >= minHits {
			recent := w.hits[len(w.hits)-minHits:]
			if loopBatchLooksAnomalous(recent, alarm) {
				w.flagged = true
				w.flaggedAt = now
				var sum int64
				var n int
				for _, h := range recent {
					if h.reqSize > 0 {
						sum += h.reqSize
						n++
					}
				}
				if n > 0 {
					w.baseSize = sum / int64(n)
				}
				if now.After(w.lastDetectAlarm.Add(interval)) {
					w.lastDetectAlarm = now
					detail := fmt.Sprintf(
						"类型: 检测到异常循环调用模式\n来源: %s\n模型: %s\n规则: %d分钟内 >=%d 次, 单请求 token >=%d, 响应 <%dms, 请求大小差异 <%v%%\n学习批次请求大小基准: %d bytes\n用户ID: %d\n渠道ID: %d\nToken: %s\n请求ID: %s",
						client, modelName,
						windowMs/60000, minHits, alarm.LoopMinTokens, alarm.LoopMaxRespMs, alarm.LoopSizeDiffPct,
						w.baseSize,
						userId, params.ChannelId, params.TokenName, requestId,
					)
					dispatchFeishuAlarm(alarm.FeishuWebhook, "循环调用异常检测 - 检测到异常模式", detail)
				}
			}
		}
		return
	}

	// Block phase: the source is already flagged (an anomalous batch was
	// learned earlier). Penalise this hit only when the full four-condition
	// signature is still present right now — at least LoopMinHits hits in the
	// window, this hit meets the token threshold, this hit is faster than the
	// response cap, and this hit's size still matches the learned base within
	// the diff threshold. Requiring all four at trigger time means a stale
	// flag (or a single transient burst) cannot punish legitimate slow traffic
	// from the same source — a normal 15-31s turn fails the response cap and
	// is never slept.
	w.hits = append(w.hits, hit)
	blocked := false
	diffPct := float64(0)
	if len(w.hits) >= alarm.LoopMinHits &&
		totalTokens >= alarm.LoopMinTokens &&
		respMs > 0 && respMs < alarm.LoopMaxRespMs &&
		w.baseSize > 0 && reqSize > 0 {
		diff := reqSize - w.baseSize
		if diff < 0 {
			diff = -diff
		}
		diffPct = float64(diff) / float64(w.baseSize) * 100
		if diffPct < alarm.LoopSizeDiffPct {
			blocked = true
		}
	}
	if !blocked {
		return
	}

	if now.After(w.lastBlockAlarm.Add(interval)) {
		w.lastBlockAlarm = now
		sleepMs := alarm.LoopSleepMs
		detail := fmt.Sprintf(
			"类型: 循环调用拦截 - sleep %dms\n来源: %s\n模型: %s\n本次 token: %d\n请求大小: %d bytes (基准 %d, 差异 %.2f%%)\n用户ID: %d\n渠道ID: %d\nToken: %s\n请求ID: %s",
			sleepMs, client, modelName, totalTokens, reqSize, w.baseSize, diffPct,
			userId, params.ChannelId, params.TokenName, requestId,
		)
		dispatchFeishuAlarm(alarm.FeishuWebhook, "循环调用异常检测 - 拦截并 sleep", detail)
	}
	if alarm.LoopSleepMs > 0 {
		loopSleepFunc(c, time.Duration(alarm.LoopSleepMs)*time.Millisecond)
	}
}

// loopBatchLooksAnomalous reports whether a batch of hits jointly matches the
// learning rule: every hit exceeds the token threshold, every hit is faster
// than the response cap, and the spread of request sizes is within the diff
// percent. A zero/unknown reqSize means the size-diff criterion cannot be
// confirmed, so the batch is not flagged (conservative — avoids false
// positives when ContentLength is unavailable).
func loopBatchLooksAnomalous(batch []loopHit, alarm *gateway.AlarmConfig) bool {
	if len(batch) == 0 {
		return false
	}
	var minSize, maxSize int64
	for i, h := range batch {
		if h.tokens < alarm.LoopMinTokens {
			return false
		}
		if h.respMs <= 0 || h.respMs >= alarm.LoopMaxRespMs {
			return false
		}
		if h.reqSize <= 0 {
			return false
		}
		if i == 0 {
			minSize, maxSize = h.reqSize, h.reqSize
			continue
		}
		if h.reqSize < minSize {
			minSize = h.reqSize
		}
		if h.reqSize > maxSize {
			maxSize = h.reqSize
		}
	}
	if maxSize <= 0 {
		return false
	}
	diff := maxSize - minSize
	return float64(diff)/float64(maxSize)*100 < alarm.LoopSizeDiffPct
}
