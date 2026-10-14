package opencode

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/relayconvert"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// Adaptor 面向 OpenCode Zen（https://opencode.ai/zen）免费层。
//
// zen 免费门禁要求上游请求必须流式且携带 opencode 客户端指纹（见 session.go
// 注释），因此本 adaptor 在 openai 之上：
//   - 强制上游 stream:true + stream_options.include_usage
//   - 注入四头 + UA + gate tools（bash/edit/read stub）
//   - 客户端要非流式时，本地聚合上游 SSE 为完整 completion JSON 返回
type Adaptor struct {
	openai.Adaptor
}

func (a *Adaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	if info.RelayMode == relayconstant.RelayModeChatCompletions {
		// 渠道 base_url 配 https://opencode.ai/zen
		return fmt.Sprintf("%s/v1/chat/completions", info.ChannelBaseUrl), nil
	}
	return a.Adaptor.GetRequestURL(info)
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, header *http.Header, info *relaycommon.RelayInfo) error {
	if err := a.Adaptor.SetupRequestHeader(c, header, info); err != nil {
		return err
	}
	header.Set("User-Agent", OPENCODE_CLIENT_UA)
	header.Set("x-opencode-client", opencodeClient)
	header.Set("x-opencode-project", opencodeProject)
	header.Set("x-opencode-session", GenerateOpencodeId("ses"))
	header.Set("x-opencode-request", GenerateOpencodeId("msg"))
	return nil
}

// enforceGate 应用免费门禁三要素：强制流式、usage 上报、gate tools 注入。
func (a *Adaptor) enforceGate(request *dto.GeneralOpenAIRequest) *dto.GeneralOpenAIRequest {
	streamTrue := true
	request.Stream = &streamTrue
	request.StreamOptions = &dto.StreamOptions{IncludeUsage: true}
	request.Tools = ApplyGateTools(request.Tools)
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
	// openai 通用转换会把非 OpenAI/Azure 渠道的 StreamOptions 清掉，
	// 门禁三要素必须在通用转换之后覆盖。
	req, ok := result.(*dto.GeneralOpenAIRequest)
	if !ok {
		return nil, fmt.Errorf("expected OpenAI chat completions request, got %T", result)
	}
	return a.enforceGate(req), nil
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
	return a.enforceGate(req), nil
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	switch info.RelayMode {
	case relayconstant.RelayModeChatCompletions:
		// 注意：对上游强制 stream:true 后，compatible_handler 会在 DoResponse 之前
		// 按上游 Content-Type 把 info.IsStream 覆写为 true，故这里必须用
		// GenRelayInfo 时存入 context 的客户端原始流式意图来判断分支。
		if common.GetContextKeyBool(c, constant.ContextKeyIsStream) {
			return openai.OaiStreamHandler(c, info, resp)
		}
		return aggregateStreamHandler(c, info, resp)
	default:
		return a.Adaptor.DoResponse(c, resp, info)
	}
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return ChannelName
}

// aggregateStreamHandler 读取上游 SSE 流并聚合为完整 chat completion 返回给
// 客户端（zen 免费门禁强制上游流式，但客户端可以请求非流式）。
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
			logger.LogError(c, "opencode aggregate: unmarshal stream chunk failed: "+err.Error())
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
