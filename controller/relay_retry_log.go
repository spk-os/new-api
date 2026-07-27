package controller

import (
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	gateway "github.com/QuantumNous/new-api/service/gateway"

	"github.com/gin-gonic/gin"
)

// RetryAttempt records a single failed relay attempt within the retry loop.
// Stored in the gin context as a slice of maps so that model.RecordConsumeLog
// can read them without importing the controller package.
type RetryAttempt struct {
	ChannelId      int    `json:"channel_id"`
	ChannelName    string `json:"channel_name"`
	ModelName      string `json:"model_name"`
	StatusCode     int    `json:"status_code"`
	ErrorReason    string `json:"error_reason"`
	UseTimeSeconds int    `json:"use_time_seconds"`
}

const (
	// contextKeyRetryLogSuppressed marks the gin context as being inside the
	// retry loop, so processChannelError should skip RecordErrorLog.
	contextKeyRetryLogSuppressed = "_retry_log_suppressed"
	// contextKeyRetryAttempts stores []map[string]interface{} of failed attempts.
	contextKeyRetryAttempts = "_retry_attempts"
)

func setRetryLogSuppression(c *gin.Context, suppressed bool) {
	c.Set(contextKeyRetryLogSuppressed, suppressed)
}

func isRetryLogSuppressed(c *gin.Context) bool {
	return c.GetBool(contextKeyRetryLogSuppressed)
}

// appendRetryAttempt records a failed attempt's details into the gin context.
// It replaces recordFailedConsumeLog so that no per-attempt consume log is
// created during the retry loop. The collected history is later attached to
// the single consolidated log (success or failure) by RecordConsumeLog.
func appendRetryAttempt(c *gin.Context, relayInfo *relaycommon.RelayInfo, channel *model.Channel, apiErr *types.NewAPIError) {
	if relayInfo == nil || channel == nil || apiErr == nil {
		return
	}

	startTime := common.GetContextKeyTime(c, constant.ContextKeyRequestStartTime)
	useTimeSeconds := 0
	if !startTime.IsZero() {
		useTimeSeconds = int(time.Since(startTime).Seconds())
	}

	attempt := map[string]interface{}{
		"channel_id":      channel.Id,
		"channel_name":    channel.Name,
		"model_name":      relayInfo.OriginModelName,
		"status_code":     apiErr.StatusCode,
		"error_reason":    apiErr.MaskSensitiveErrorWithStatusCode(),
		"use_time_seconds": useTimeSeconds,
	}

	var attempts []map[string]interface{}
	if existing, ok := c.Get(contextKeyRetryAttempts); ok {
		if existingAttempts, ok := existing.([]map[string]interface{}); ok {
			attempts = existingAttempts
		}
	}
	attempts = append(attempts, attempt)
	c.Set(contextKeyRetryAttempts, attempts)
}

// recordConsolidatedFailedLog creates a single consume log entry for a request
// that exhausted all retry attempts. It replaces the per-attempt error logs
// and failed consume logs with one consolidated record containing the full
// retry history in the Other field.
//
// The retry_attempts and retry_count fields are auto-attached by
// model.RecordConsumeLog from the gin context, so this function only needs to
// provide the error-specific fields.
func recordConsolidatedFailedLog(c *gin.Context, relayInfo *relaycommon.RelayInfo) {
	if relayInfo == nil || relayInfo.LastError == nil {
		return
	}
	if !common.LogConsumeEnabled {
		return
	}

	apiErr := relayInfo.LastError

	startTime := common.GetContextKeyTime(c, constant.ContextKeyRequestStartTime)
	useTimeSeconds := 0
	if !startTime.IsZero() {
		useTimeSeconds = int(time.Since(startTime).Seconds())
	}

	tokenName := c.GetString("token_name")
	group := relayInfo.UsingGroup
	if group == "" {
		group = c.GetString("group")
	}

	other := map[string]interface{}{
		"error_type":   apiErr.GetErrorType(),
		"error_code":   apiErr.GetErrorCode(),
		"status_code":  apiErr.StatusCode,
		"error_reason": apiErr.MaskSensitiveErrorWithStatusCode(),
		"use_channel":  c.GetStringSlice("use_channel"),
	}

	// Include gateway plan info if available (same as recordFailedConsumeLog).
	if gw, ok := c.Get("_gateway_plan"); ok {
		if plan, ok := gw.(*gateway.GatewayPlan); ok && plan != nil {
			other["gateway_group"] = common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
			if len(plan.Candidates) > 0 {
				other["gateway_provider"] = plan.Candidates[0].ProviderId
				other["gateway_actual_model"] = plan.Candidates[0].ActualModel
			}
			other["gateway_affinity_status"] = "disabled"
			if plan.Profile != nil && plan.Profile.Affinity != nil && plan.Profile.Affinity.Enabled {
				if plan.AffinityHit {
					other["gateway_affinity_status"] = "hit"
				} else {
					other["gateway_affinity_status"] = "miss"
				}
			}
		}
	}

	logger.LogInfo(c, "[consolidated] recording single failed log for request that exhausted retries")

	model.RecordConsumeLog(c, relayInfo.UserId, model.RecordConsumeLogParams{
		ChannelId:        c.GetInt("channel_id"),
		PromptTokens:     0,
		CompletionTokens: 0,
		ModelName:        relayInfo.OriginModelName,
		TokenName:        tokenName,
		Quota:            0,
		Content:          "请求失败: " + apiErr.MaskSensitiveErrorWithStatusCode(),
		TokenId:          relayInfo.TokenId,
		UseTimeSeconds:   useTimeSeconds,
		IsStream:         relayInfo.IsStream,
		Group:            group,
		Other:            other,
	})
}
