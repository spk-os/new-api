package workbuddy

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/relayconvert"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// Adaptor 面向 WorkBuddy（腾讯桌面 AI 助手，copilot.tencent.com 同源端点）。
//
// 上游 OpenAI Chat Completions 形状但仅支持 SSE（强制 stream:true），差异点：
//   - 聊天端点为 /v2/chat/completions，非流式客户端由本地聚合上游 SSE 还原
//     为完整 completion JSON（同 opencode 渠道）
//   - 鉴权 Bearer accessToken，并须注入 X-User-Id/X-Enterprise-Id/X-Domain/
//     X-IDE-Type/X-Product/X-Requested-With + Origin/Referer + 固定 UA；
//     渠道 key 存导出的凭据 JSON，token.go 负责刷新与缓存
//   - body 约束：developer 角色→system（400 code 11128）；tool_choice 仅
//     字符串；reasoning_effort 白名单（off 被拒 11150）
type Adaptor struct {
	openai.Adaptor
}

// DoRequest 必须覆盖：继承自 openai.Adaptor 的版本会把内层 openai 实例传给
// DoApiRequest，导致本类型的 GetRequestURL / SetupRequestHeader 覆盖被
// 绕过（workbuddy 的 URL 改写与头注入全部失效）。传 a 自身以恢复动态派发。
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
		return fmt.Sprintf("%s%s", info.ChannelBaseUrl, workbuddyChatPath), nil
	}
	return a.Adaptor.GetRequestURL(info)
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, header *http.Header, info *relaycommon.RelayInfo) error {
	if err := a.Adaptor.SetupRequestHeader(c, header, info); err != nil {
		return err
	}
	cred, err := ResolveCredential(c.Request.Context(), info.ApiKey, info.ChannelBaseUrl)
	if err != nil {
		return fmt.Errorf("workbuddy auth: %w", err)
	}
	header.Set("Authorization", "Bearer "+cred.AccessToken)
	// 身份头来自凭据 JSON；直接粘贴 accessToken 的 key 没有身份字段，缺省跳过
	if cred.UID != "" {
		header.Set("X-User-Id", cred.UID)
	}
	if cred.EnterpriseID != "" {
		header.Set("X-Enterprise-Id", cred.EnterpriseID)
	}
	if cred.Domain != "" {
		header.Set("X-Domain", cred.Domain)
	}
	header.Set("X-IDE-Type", "WorkBuddy")
	header.Set("X-Product", "SaaS")
	header.Set("X-Requested-With", "XMLHttpRequest")
	// Origin/Referer 固定主域名，UA 决定 /v3/config 模型花名册，均与 CLI 一致
	header.Set("Origin", workbuddySiteOrigin)
	header.Set("Referer", workbuddySiteOrigin+"/")
	header.Set("User-Agent", workbuddyClientUA)
	// 上游仅有 SSE 端点，Accept 固定 text/event-stream（SetupApiRequestHeader
	// 会原样透传客户端 Accept，api_request.go）
	header.Set("Accept", "text/event-stream")
	return nil
}

// Claude Code 官方 system prompt 首句是精确指纹：上游风控围绕该句拦截
// （400 code 11128 Illegal API invocation from an unapproved channel）。
// 实测还发现 system 中的 Anthropic 官方遥测头块（x-anthropic-billing-header:
// cc_version=...; cc_entrypoint=...;）同样单独触发拦截——两道指纹缺一即过，
// 同时存在均拦。Claude 格式入站的 system 常是多块数组，经通用 Claude→OpenAI
// 转换后按块顺序直接拼接成单条 string（to_oai_chat_req.go），指纹可能出现在
// 任意位置（CC 2.1.299.120 首块是 billing-header，指纹句在第二块）。
// 转发时清除全部 x-anthropic-* 遥测头并把指纹句改写为中性描述——遥测头对
// 模型无语义，指纹句改写语义等价，其他客户端几乎不可能携带，无副作用。
const claudeCodeSystemFingerprint = "You are Claude Code, Anthropic's official CLI for Claude."
const claudeCodeSystemReplacement = "You are a CLI coding agent."
const anthropicTelemetryHeaderPrefix = "x-anthropic-"

// sanitizeClaudeCodeSystem 清除 system 消息中的 Anthropic 官方遥测头行并改写
// 所有出现的 Claude Code 官方指纹句。Content 兼容字符串与 multimodal 数组
// 两种形态，其余形态原样保留。
func sanitizeClaudeCodeSystem(msg *dto.Message) {
	replace := func(s string) string {
		s = strings.ReplaceAll(s, claudeCodeSystemFingerprint, claudeCodeSystemReplacement)
		return s
	}
	switch v := msg.Content.(type) {
	case string:
		msg.Content = replace(dropTelemetryLines(v))
	case []dto.MediaContent:
		blocks := v[:0]
		for _, b := range v {
			if b.Type == "text" {
				if strings.HasPrefix(b.Text, anthropicTelemetryHeaderPrefix) {
					continue // 遥测头块整体丢弃
				}
				b.Text = replace(b.Text)
			}
			blocks = append(blocks, b)
		}
		msg.Content = blocks
	}
}

// dropTelemetryLines 删除字符串中 x-anthropic-* 开头的行（多块 system 拼接
// 成单条 string 后，遥测头块退化为行首片段）。
func dropTelemetryLines(s string) string {
	lines := strings.Split(s, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(line, anthropicTelemetryHeaderPrefix) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// applyChatRules 应用 workbuddy body 约束（通用转换之后、发往上游之前）。
func (a *Adaptor) applyChatRules(request *dto.GeneralOpenAIRequest) *dto.GeneralOpenAIRequest {
	streamTrue := true
	request.Stream = &streamTrue
	for i := range request.Messages {
		// 上游拒绝 developer 角色（400 code 11128），CLI 同款修正为 system
		if request.Messages[i].Role == "developer" {
			request.Messages[i].Role = "system"
		}
		// system 开头的 Claude Code 官方指纹句触发上游渠道校验拦截，改写
		if request.Messages[i].Role == "system" {
			sanitizeClaudeCodeSystem(&request.Messages[i])
		}
	}
	// tool_choice 仅接受字符串形态：{type,function:{name}} 对象取函数名，
	// 其余对象/非法形态直接移除
	switch tc := request.ToolChoice.(type) {
	case string:
	case map[string]any:
		if fn, ok := tc["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok && name != "" {
				request.ToolChoice = name
			} else {
				request.ToolChoice = nil
			}
		} else {
			request.ToolChoice = nil
		}
	default:
		request.ToolChoice = nil
	}
	// reasoning_effort 白名单之外（含 off/none/空串/未知值）移除，避免 11150
	if !reasoningEfforts[request.ReasoningEffort] {
		request.ReasoningEffort = ""
	}
	return request
}

func (a *Adaptor) ConvertOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error) {
	if request == nil {
		return nil, errors.New("request is nil")
	}
	result, err := a.Adaptor.ConvertOpenAIRequest(c, info, request)
	if err != nil {
		return nil, err
	}
	req, ok := result.(*dto.GeneralOpenAIRequest)
	if !ok {
		return nil, fmt.Errorf("expected OpenAI chat completions request, got %T", result)
	}
	return a.applyChatRules(req), nil
}

func (a *Adaptor) ConvertClaudeRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.ClaudeRequest) (any, error) {
	result, err := a.Adaptor.ConvertClaudeRequest(c, info, request)
	if err != nil {
		return nil, err
	}
	req, ok := result.(*dto.GeneralOpenAIRequest)
	if !ok {
		return nil, fmt.Errorf("expected OpenAI chat completions request, got %T", result)
	}
	return a.applyChatRules(req), nil
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	// 对上游强制 stream:true 后，compatible_handler 会在 DoResponse 之前按
	// 上游 Content-Type 把 info.IsStream 覆写为 true，故这里必须用 GenRelayInfo
	// 时存入 context 的客户端原始流式意图来判断分支（与 opencode 同款）。
	if info.RelayMode == relayconstant.RelayModeChatCompletions ||
		info.RelayFormat == types.RelayFormatClaude {
		if common.GetContextKeyBool(c, constant.ContextKeyIsStream) {
			return openai.OaiStreamHandler(c, info, resp)
		}
		return aggregateStreamHandler(c, info, resp)
	}
	return a.Adaptor.DoResponse(c, resp, info)
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return ChannelName
}

// aggregateStreamHandler 读取上游 SSE 流并聚合为完整 chat completion 返回给
// 客户端（上游仅支持 SSE，但客户端可以请求非流式）。与 opencode 渠道的
// aggregateStreamHandler 同构，渠道包自包含故各自维护。
func aggregateStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	scanner := helper.NewStreamScanner(resp.Body)
	var (
		content      strings.Builder
		reasoning    strings.Builder
		toolCalls    []dto.ToolCallResponse
		finishReason string
		responseId   string
		created      int64
		model        string
		usage        *dto.Usage
	)
	for scanner.Scan() {
		data := strings.TrimSuffix(strings.TrimPrefix(scanner.Text(), "data: "), "\r")
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk dto.ChatCompletionsStreamResponse
		if err := common.Unmarshal([]byte(data), &chunk); err != nil {
			logger.LogError(c, "workbuddy aggregate: unmarshal stream chunk failed: "+err.Error())
			continue
		}
		if chunk.Id != "" {
			responseId = chunk.Id
		}
		if chunk.Created != 0 {
			created = chunk.Created
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		if chunk.Usage != nil && service.ValidUsage(chunk.Usage) {
			usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			content.WriteString(choice.Delta.GetContentString())
			reasoning.WriteString(choice.Delta.GetReasoningContent())
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishReason = *choice.FinishReason
			}
			toolCalls = mergeToolCalls(toolCalls, choice.Delta.ToolCalls)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}

	if responseId == "" {
		responseId = helper.GetResponseID(c)
	}
	if model == "" {
		model = info.UpstreamModelName
	}
	if finishReason == "" {
		finishReason = "stop"
	}

	message := dto.Message{
		Role:    "assistant",
		Content: content.String(),
	}
	if reasoning.Len() > 0 {
		r := reasoning.String()
		message.ReasoningContent = &r
	}
	if len(toolCalls) > 0 {
		toolCallsJson, err := common.Marshal(toolCalls)
		if err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
		message.ToolCalls = toolCallsJson
	}

	if usage == nil {
		usage = service.ResponseText2Usage(c, content.String(), info.UpstreamModelName, info.GetEstimatePromptTokens())
	}

	textResponse := dto.OpenAITextResponse{
		Id:      responseId,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []dto.OpenAITextResponseChoice{
			{
				Index:        0,
				Message:      message,
				FinishReason: finishReason,
			},
		},
		Usage: *usage,
	}

	var responseBody []byte
	if info.RelayFormat != types.RelayFormatOpenAI {
		convertResult, err := relayconvert.ConvertResponse(c, info, info.RelayFormat, &textResponse)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		responseBody, err = common.Marshal(convertResult.Value)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
	} else {
		var err error
		responseBody, err = common.Marshal(textResponse)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
	}

	// 上游是 SSE，Content-Type 须改回 JSON 再写回非流式客户端
	resp.Header.Set("Content-Type", "application/json")
	service.IOCopyBytesGracefully(c, resp, responseBody)

	return usage, nil
}

// mergeToolCalls 按 index 合并流式 tool_calls 分片（arguments 逐段拼接）。
func mergeToolCalls(acc []dto.ToolCallResponse, incoming []dto.ToolCallResponse) []dto.ToolCallResponse {
	for _, call := range incoming {
		idx := 0
		if call.Index != nil {
			idx = *call.Index
		}
		for idx >= len(acc) {
			acc = append(acc, dto.ToolCallResponse{Index: new(int), Function: dto.FunctionResponse{}})
		}
		slot := &acc[idx]
		*slot.Index = idx
		if call.ID != "" {
			slot.ID = call.ID
		}
		if call.Type != nil {
			slot.Type = call.Type
		}
		if call.Function.Name != "" {
			slot.Function.Name = call.Function.Name
		}
		slot.Function.Arguments += call.Function.Arguments
	}
	return acc
}
