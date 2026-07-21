package service

import (
	"bytes"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	gwpackage "github.com/QuantumNous/new-api/service/gateway"
	"github.com/gin-gonic/gin"
)

func ContentCaptureMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !common.ContentLogEnabled {
			c.Next()
			return
		}

		// Wrap response writer to capture gateway response body
		wrapped := NewContentLogResponseWriter(c.Writer)
		c.Writer = wrapped

		// Capture gateway request
		gatewayReq := CaptureGatewayRequest(c)

		c.Next()

		// Capture streaming body buffer (filled by streamTrackingBody during consumption)
		// Only use buffer if handler didn't already set a reconstructed body
		if bufPtr, ok := c.Get("_content_upstream_body_buf"); ok {
			if buf, ok := bufPtr.(*bytes.Buffer); ok && buf.Len() > 0 {
				if upstreamResp, exists := c.Get("_content_upstream_resp"); exists {
					if ur, ok := upstreamResp.(*HttpMessage); ok && ur.Body == "" {
						ur.Body = buf.String()
					}
				}
			}
		}

		// Only capture for relay requests (non-zero channel_id)
		channelID := c.GetInt("channel_id")
		if channelID == 0 {
			return
		}

		// Build gateway response from the wrapped writer
		gatewayResp := &HttpMessage{
			Status:  wrapped.StatusCode(),
			Headers: wrapped.ResponseHeaders(),
			Body:    wrapped.BodyString(),
		}

		// For streaming responses, replace raw SSE chunks with the
		// reconstructed upstream response body (a single aggregated JSON)
		if _, isStream := c.Get("_content_upstream_body_buf"); isStream {
			if resp, exists := c.Get("_content_upstream_resp"); exists {
				if ur, ok := resp.(*HttpMessage); ok && ur.Body != "" {
					gatewayResp.Body = ur.Body
				}
			}
		}

		// Get upstream data stored by doRequest()
		upstreamReq, _ := c.Get("_content_upstream_req")
		upstreamResp, _ := c.Get("_content_upstream_resp")

		requestID := c.GetString(common.RequestIdKey)
		if requestID == "" {
			requestID = "unknown"
		}

		modelName := c.GetString("original_model")
		if modelName == "" {
			modelName = c.GetString("model")
		}

		channelName := c.GetString("channel_name")

		entry := &ContentLogEntry{
			RequestID:   requestID,
			UserID:      c.GetInt("id"),
			ChannelID:   channelID,
			ChannelName: channelName,
			ModelName:   modelName,
			CreatedAt:   time.Now().UnixMilli(),
		}

		// Check upstream model mapping
		upstreamModelName, _ := c.Get("upstream_model_name")
		if upstreamModelName != nil {
			if s, ok := upstreamModelName.(string); ok && s != "" {
				entry.UpstreamModelName = s
			}
		}

		if gatewayReq != nil {
			entry.GatewayRequest = gatewayReq
		}
		if gatewayResp != nil {
			entry.GatewayResponse = gatewayResp
		}
		if upstreamReq != nil {
			if ur, ok := upstreamReq.(*HttpMessage); ok {
				entry.UpstreamRequest = ur
			}
		}
		if upstreamResp != nil {
			if ur, ok := upstreamResp.(*HttpMessage); ok {
				if ur.Headers != nil {
					ur.Headers = sanitizeHeaders(ur.Headers)
				}
				entry.UpstreamResponse = ur
			}
		}

		if gwVal, ok := c.Get("_gateway_plan"); ok {
			if plan, ok := gwVal.(*gwpackage.GatewayPlan); ok && plan != nil {
				gl := &GatewayLog{
					StickyEnabled:  c.GetString("X-Sticky-Route") != "false",
				}
				if cid, ok := c.Get("_gateway_client_id"); ok {
					gl.ClientId, _ = cid.(string)
				}
				if tid, ok := c.Get("_gateway_task_id"); ok {
					gl.TaskId, _ = tid.(string)
				}
				gl.StrategyGroup = common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
				if plan.Profile != nil && plan.Profile.Affinity != nil && plan.Profile.Affinity.Enabled {
					gl.AffinityStatus = "miss"
					if plan.AffinityHit {
						gl.AffinityStatus = "hit"
					}
				} else {
					gl.AffinityStatus = "disabled"
				}
				if len(plan.Candidates) > 0 {
					first := plan.Candidates[0]
					gl.ProviderId = first.ProviderId
					gl.ModelGroup = first.ModelGroup
					gl.ActualModel = first.ActualModel
					gl.KeyIndex = first.KeyIndex
					for _, c := range plan.Candidates {
						gl.CandidateChain = append(gl.CandidateChain, c.ProviderId+"/"+c.ModelGroup+"/"+c.ActualModel+"#k"+strconv.Itoa(c.KeyIndex))
					}
				}
				entry.Gateway = gl
			}
		}

		RecordContentLog(entry)
		common.SysLog("content_logger: recorded content for request_id=" + requestID)
	}
}

// InitContentCaptureRoutes sets up the content capture middleware on relay routes.
// The middleware is registered unconditionally so it can be toggled at runtime
// via the ContentLogEnabled setting. The middleware itself checks the setting
// per-request.
func InitContentCaptureRoutes(r *gin.Engine) {
	common.SysLog("content_logger: content capture middleware registered")
	// Apply to all /v1/* and /provider/* routes
	r.Use(func() gin.HandlerFunc {
		return ContentCaptureMiddleware()
	}())
}
