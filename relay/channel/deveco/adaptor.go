package deveco

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// Adaptor 面向 DevEco Code（deveco-code CLI 同源的华为 CodeGenie MaaS）。
//
// 上游 OpenAI Chat Completions 兼容（vLLM 后端），差异点：
//   - 非流式请求 URL 为 /no-stream/chat/completions
//   - 须注入 lang / Chat-Id / X-DevEco-Improvement-Enabled 头
//   - Authorization 用华为账号 OAuth access token（约 30 分钟过期），渠道
//     key 存长效 jwtToken，token.go 负责换取与缓存
//   - 推理模型 body 须带 "thinking":{"type":"enabled"}，经渠道
//     param_override 配置注入（compatible_handler 通用合并，对 Claude
//     格式客户端同样生效）
type Adaptor struct {
	openai.Adaptor
}

// DoRequest 必须覆盖：继承自 openai.Adaptor 的版本会把内层 openai 实例传给
// DoApiRequest，导致本类型的 GetRequestURL / SetupRequestHeader 覆盖被
// 绕过（deveco 的 URL 改写与头注入全部失效）。传 a 自身以恢复动态派发。
func (a *Adaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	return channel.DoApiRequest(a, c, info, requestBody)
}

func (a *Adaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	// Claude 格式（/v1/messages）的 RelayMode 不是 ChatCompletions（为
	// Unknown），body 已由通用转换机制变为 OpenAI chat completions，同样
	// 打到本上游，故按 RelayFormat 一并映射，否则落入内嵌 openai 分支
	// 拼出 {base}/v1/chat/completions 导致上游 404。
	if info.RelayMode == relayconstant.RelayModeChatCompletions ||
		info.RelayFormat == types.RelayFormatClaude {
		// 此处 info.IsStream 仍是客户端意图：上游 Content-Type 覆写发生在
		// DoResponse 之前、本方法之后（compatible_handler.go）。
		if info.IsStream {
			return fmt.Sprintf("%s/chat/completions", info.ChannelBaseUrl), nil
		}
		return fmt.Sprintf("%s/no-stream/chat/completions", info.ChannelBaseUrl), nil
	}
	return a.Adaptor.GetRequestURL(info)
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, header *http.Header, info *relaycommon.RelayInfo) error {
	if err := a.Adaptor.SetupRequestHeader(c, header, info); err != nil {
		return err
	}
	access, err := getAccessToken(c.Request.Context(), info.ApiKey, info.ChannelBaseUrl)
	if err != nil {
		return fmt.Errorf("deveco auth: %w", err)
	}
	header.Set("Authorization", "Bearer "+access)
	header.Set("lang", "en")
	header.Set("Chat-Id", newChatID())
	header.Set("X-DevEco-Improvement-Enabled", "false")
	// 上游流式端点拒绝 Accept: application/json（406 Not Acceptable），而
	// SetupApiRequestHeader 会原样透传客户端 Accept（api_request.go），故按
	// 客户端流式意图覆写——此处仍在上游响应之前，info.IsStream 即客户端意图。
	if info.IsStream {
		header.Set("Accept", "text/event-stream")
	} else {
		header.Set("Accept", "application/json")
	}
	return nil
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return ChannelName
}

// newChatID 生成 32 位 hex Chat-Id。CLI 按 session 维持映射，实测任意 hex 可用。
func newChatID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto 失败时退化为时间戳填充，保证 Chat-Id 永不缺失
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
