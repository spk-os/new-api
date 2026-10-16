package service

// Token usage alarm: fires when a single request exceeds a token threshold
// or the rolling per-minute token total exceeds a threshold. Notifications go
// to the spk-os-alarm Feishu webhook (direct POST, no HMAC signature — the
// spk-os-alarm webhook is not configured with a secret).
//
// Two alarm kinds dedupe independently: after a kind fires it is suppressed
// for `Alarm.Interval` (default 10 min). State is in-memory only (no DB
// writes), per the user's "不用db" requirement — minute bucket + dedupe map.

import (
	"bytes"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/gateway"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

// DefaultFeishuAlarmWebhook is the compiled-in spk-os-alarm Feishu webhook,
// used when gateway config alarm.feishu-webhook is empty. Keeping the URL here
// lets us rotate it without a code change only when the YAML override is set;
// when unset we fall back to this constant so alarms work out of the box.
const DefaultFeishuAlarmWebhook = "https://open.feishu.cn/open-apis/bot/v2/hook/de3f09c4-e7da-472d-b668-151ba44aa8ee"

var (
	// Per-minute token bucket. bucket = unix-seconds aligned to the minute
	// (ts - ts%60). When a new minute rolls in, sum resets to the incoming
	// request's tokens. Guarded by alarmMinuteMu.
	alarmMinuteBucket int64
	alarmMinuteSum     int64
	alarmMinuteMu      sync.Mutex

	// Dedupe map: kind ("one-req" / "per-minute") → next time the alarm of
	// that kind is allowed to fire. Guarded by alarmDedupeMu so the
	// check-and-set is race-free.
	alarmDedupeMap sync.Map
	alarmDedupeMu  sync.Mutex

	// alarmNowFunc is overridable in tests; production uses time.Now.
	alarmNowFunc = time.Now
)

// recordMinuteTokens accumulates tokens into the current-minute bucket and
// returns the running sum for that minute. A new minute boundary resets the
// sum.
func recordMinuteTokens(ts int64, tokens int64) int64 {
	bucket := ts - ts%60
	alarmMinuteMu.Lock()
	defer alarmMinuteMu.Unlock()
	if bucket != alarmMinuteBucket {
		alarmMinuteBucket = bucket
		alarmMinuteSum = 0
	}
	alarmMinuteSum += tokens
	return alarmMinuteSum
}

// shouldAlarm reports whether an alarm of the given kind may fire now, and if
// so records the next-allowed time (now+interval) so the same kind is
// suppressed for interval. Returns false (without updating) while suppressed.
func shouldAlarm(kind string, interval time.Duration) bool {
	now := alarmNowFunc()
	alarmDedupeMu.Lock()
	defer alarmDedupeMu.Unlock()
	if v, ok := alarmDedupeMap.Load(kind); ok {
		if next, ok := v.(time.Time); ok && now.Before(next) {
			return false
		}
	}
	alarmDedupeMap.Store(kind, now.Add(interval))
	return true
}

// checkTokenAlarm is the AfterRecordConsumeLogHook implementation. It runs on
// every recorded consume log (once per request — new-api now writes a single
// log per request regardless of retries, with retry detail in Content Log).
// Both checks are cheap; Feishu dispatch is async via gopool so the request
// path is never blocked.
func checkTokenAlarm(c *gin.Context, userId int, createdAt int64, params model.RecordConsumeLogParams) {
	cfg := gateway.GetConfig()
	if cfg == nil || cfg.Alarm == nil {
		return
	}
	alarm := cfg.Alarm
	interval := time.Duration(alarm.Interval) * time.Millisecond
	if interval <= 0 {
		interval = 10 * time.Minute
	}

	totalTokens := int64(params.PromptTokens) + int64(params.CompletionTokens) + int64(params.CacheTokens)

	// 1) single-request threshold
	if alarm.OneReqTokens > 0 && totalTokens >= alarm.OneReqTokens {
		if shouldAlarm(alarmKindOneReq, interval) {
			requestId := ""
			if c != nil {
				requestId = c.GetString(common.RequestIdKey)
			}
			detail := fmt.Sprintf(
				"类型: 单请求 token 超阈值\n阈值: %d tokens\n实际: %d tokens\n用户ID: %d\n模型: %s\n渠道ID: %d\nToken: %s\n请求ID: %s",
				alarm.OneReqTokens, totalTokens, userId, params.ModelName, params.ChannelId, params.TokenName, requestId,
			)
			dispatchFeishuAlarm(alarm.FeishuWebhook, "Token 用量告警 - 单请求超阈值", detail)
		}
	}

	// 2) per-minute rolling total threshold
	if alarm.TokensPerMinute > 0 {
		sum := recordMinuteTokens(createdAt, totalTokens)
		if sum >= alarm.TokensPerMinute {
			if shouldAlarm(alarmKindPerMinute, interval) {
				requestId := ""
				if c != nil {
					requestId = c.GetString(common.RequestIdKey)
				}
				detail := fmt.Sprintf(
					"类型: 分钟 token 总量超阈值\n阈值: %d tokens/分钟\n实际: %d tokens/分钟\n用户ID: %d\n模型: %s\n渠道ID: %d\nToken: %s\n请求ID: %s",
					alarm.TokensPerMinute, sum, userId, params.ModelName, params.ChannelId, params.TokenName, requestId,
				)
				dispatchFeishuAlarm(alarm.FeishuWebhook, "Token 用量告警 - 分钟总量超阈值", detail)
			}
		}
	}

	// 3) loop-call detection (learning + block/sleep). Shares this hook so no
	// second model→service hook is needed. The sleep in the block phase is
	// synchronous and will delay this request's response — that is the
	// intended penalty (see loop_detect.go).
	checkLoopCall(c, userId, createdAt, params)
}

const (
	alarmKindOneReq    = "one-req"
	alarmKindPerMinute = "per-minute"
)

// dispatchFeishuAlarm sends the alarm async so it never blocks the request.
func dispatchFeishuAlarm(webhookURL, title, detail string) {
	if webhookURL == "" {
		webhookURL = DefaultFeishuAlarmWebhook
	}
	// Logged on the request goroutine (before async dispatch) so alarm events
	// are visible in container logs even when the Feishu POST itself succeeds
	// silently — operators can correlate "token alarm dispatched" with the
	// request that triggered it.
	common.SysLog("token alarm dispatched: " + title)
	gopool.Go(func() {
		if err := sendFeishuAlarm(webhookURL, title, detail); err != nil {
			common.SysError(fmt.Sprintf("token alarm feishu send failed: %v", err))
		}
	})
}

// sendFeishuAlarm posts a text message to a Feishu webhook. It reuses the
// SSRF-protected HTTP client and URL validation used by SendWebhookNotify.
// No HMAC signature — the spk-os-alarm webhook has no secret configured.
func sendFeishuAlarm(webhookURL, title, detail string) error {
	if err := ValidateSSRFProtectedFetchURL(webhookURL); err != nil {
		return fmt.Errorf("alarm webhook url rejected: %w", err)
	}

	body := map[string]interface{}{
		"msg_type": "text",
		"content": map[string]string{
			"text": title + "\n" + detail,
		},
	}
	payload, err := common.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal feishu body: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, webhookURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create feishu request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := GetSSRFProtectedHTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("send feishu request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("feishu webhook status %d", resp.StatusCode)
	}
	return nil
}

// init registers checkTokenAlarm as a post-consume-log hook. Defined in the
// model package as vars (model cannot import service without a cycle), set
// here where service→model is the natural direction.
func init() {
	model.RegisterAfterRecordConsumeLogHook(checkTokenAlarm)
}
