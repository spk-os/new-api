package workbuddy

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyChatRules(t *testing.T) {
	a := &Adaptor{}

	t.Run("developer 角色改写为 system 并强制流式", func(t *testing.T) {
		req := &dto.GeneralOpenAIRequest{
			Messages: []dto.Message{
				{Role: "developer", Content: "sys"},
				{Role: "user", Content: "hi"},
			},
		}
		out := a.applyChatRules(req)
		assert.Equal(t, "system", out.Messages[0].Role)
		assert.Equal(t, "user", out.Messages[1].Role)
		require.NotNil(t, out.Stream)
		assert.True(t, *out.Stream)
	})

	t.Run("tool_choice 对象取函数名", func(t *testing.T) {
		req := &dto.GeneralOpenAIRequest{
			ToolChoice: map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": "get_weather",
				},
			},
		}
		out := a.applyChatRules(req)
		assert.Equal(t, "get_weather", out.ToolChoice)
	})

	t.Run("tool_choice 字符串保留", func(t *testing.T) {
		req := &dto.GeneralOpenAIRequest{ToolChoice: "auto"}
		out := a.applyChatRules(req)
		assert.Equal(t, "auto", out.ToolChoice)
	})

	t.Run("tool_choice 非法形态移除", func(t *testing.T) {
		for _, tc := range []any{
			map[string]any{"type": "function"},
			map[string]any{"function": map[string]any{}},
			true,
		} {
			req := &dto.GeneralOpenAIRequest{ToolChoice: tc}
			out := a.applyChatRules(req)
			assert.Nil(t, out.ToolChoice)
		}
	})

	t.Run("reasoning_effort 白名单", func(t *testing.T) {
		for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
			req := &dto.GeneralOpenAIRequest{ReasoningEffort: effort}
			out := a.applyChatRules(req)
			assert.Equal(t, effort, out.ReasoningEffort, "白名单内 %s 应保留", effort)
		}
		// off 被上游拒绝（code 11150），连同未知值一并移除
		for _, effort := range []string{"off", "none", "ultra"} {
			req := &dto.GeneralOpenAIRequest{ReasoningEffort: effort}
			out := a.applyChatRules(req)
			assert.Empty(t, out.ReasoningEffort, "白名单外 %s 应移除", effort)
		}
	})
}

func TestApplyChatRulesClaudeCodeSystemFingerprint(t *testing.T) {
	const fp = claudeCodeSystemFingerprint
	const wantReplaced = claudeCodeSystemReplacement

	t.Run("system 开头指纹句被改写", func(t *testing.T) {
		a := &Adaptor{}
		req := &dto.GeneralOpenAIRequest{Messages: []dto.Message{
			{Role: "system", Content: fp + " Follow the user's instructions."},
			{Role: "user", Content: "hi"},
		}}
		out := a.applyChatRules(req)
		sys, ok := out.Messages[0].Content.(string)
		require.True(t, ok)
		assert.Equal(t, wantReplaced+" Follow the user's instructions.", sys)
		assert.NotContains(t, sys, fp)
	})

	t.Run("system 数组形态所有 text 块被改写", func(t *testing.T) {
		a := &Adaptor{}
		req := &dto.GeneralOpenAIRequest{Messages: []dto.Message{
			{Role: "system", Content: []dto.MediaContent{
				{Type: "text", Text: fp},
				{Type: "text", Text: "env: linux " + fp},
			}},
			{Role: "user", Content: "hi"},
		}}
		out := a.applyChatRules(req)
		blocks, ok := out.Messages[0].Content.([]dto.MediaContent)
		require.True(t, ok)
		assert.Equal(t, wantReplaced, blocks[0].Text)
		assert.Equal(t, "env: linux "+wantReplaced, blocks[1].Text)
	})

	t.Run("非 system 角色不改写", func(t *testing.T) {
		a := &Adaptor{}
		req := &dto.GeneralOpenAIRequest{Messages: []dto.Message{
			{Role: "user", Content: fp},
		}}
		out := a.applyChatRules(req)
		assert.Equal(t, fp, out.Messages[0].Content)
	})

	t.Run("system 句中指纹句被移除（真实拼接形态，与遥测头同行）", func(t *testing.T) {
		a := &Adaptor{}
		// Claude 多块 system 经通用转换按块顺序拼接成单条 string，
		// 指纹句落在 billing-header 之后同一行——真实 CC 2.1.299.120
		// 请求正是此形态（v2.24.2 首版仅改写开头导致漏改，见 11128）
		content := "x-anthropic-billing-header: cc_version=2.1.299.120; cc_entrypoint=cli;" + fp + "\n\nYou are an agent working with the user."
		req := &dto.GeneralOpenAIRequest{Messages: []dto.Message{
			{Role: "system", Content: content},
			{Role: "user", Content: "hi"},
		}}
		out := a.applyChatRules(req)
		sys, ok := out.Messages[0].Content.(string)
		require.True(t, ok)
		assert.NotContains(t, sys, fp)
		assert.NotContains(t, sys, "x-anthropic-")
		assert.Contains(t, sys, "You are an agent working with the user.", "主体内容应保留")
	})
}

// TestClaudeCodeSystemFingerprintEndToEnd 用真实 Claude Code 2.1.299.120
// 入站 body 形态回放全链路：Claude /v1/messages 请求 → 通用转换 → applyChatRules
// → 出站 OpenAI body，断言指纹句已被清除（用户失败请求
// 202610141625207204568978268d9d6RZhlbTjS 的回放测试）。
func TestClaudeCodeSystemFingerprintEndToEnd(t *testing.T) {
	// 与 content log 一致的入站 system 数组：billing-header 块在前，
	// 指纹块带 cache_control ephemeral
	claudeBody := `{
		"model": "step-5-preview",
		"max_tokens": 1024,
		"stream": true,
		"system": [
			{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.299.120; cc_entrypoint=cli;"},
			{"type": "text", "text": "You are Claude Code, Anthropic's official CLI for Claude.", "cache_control": {"type": "ephemeral"}},
			{"type": "text", "text": "\n\nYou are an agent working with the user toward their goals."}
		],
		"messages": [
			{"role": "user", "content": "hi"}
		]
	}`
	var claudeReq dto.ClaudeRequest
	require.NoError(t, common.Unmarshal([]byte(claudeBody), &claudeReq))
	require.False(t, claudeReq.IsStringSystem())

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	a := &Adaptor{}
	result, err := a.ConvertClaudeRequest(c, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}, &claudeReq)
	require.NoError(t, err)

	outBody, err := common.Marshal(result)
	require.NoError(t, err)
	outStr := string(outBody)
	// 指纹句与遥测头同行，可能被整行连带移除；达标标准 = 两者均不外泄，
	// 且 system 主体内容保留
	assert.NotContains(t, outStr, claudeCodeSystemFingerprint, "出站 body 不得含 Claude Code 官方指纹句")
	assert.NotContains(t, outStr, "x-anthropic-", "出站 body 不得含 Anthropic 官方遥测头（billing-header 单独即触发 11128）")
	assert.Contains(t, outStr, "You are an agent working with the user toward their goals.", "system 主体应保留")
}

func TestSanitizeClaudeCodeSystemTelemetry(t *testing.T) {
	t.Run("数组形态遥测头块整体删除", func(t *testing.T) {
		a := &Adaptor{}
		req := &dto.GeneralOpenAIRequest{Messages: []dto.Message{
			{Role: "system", Content: []dto.MediaContent{
				{Type: "text", Text: "x-anthropic-billing-header: cc_version=2.1.299.120; cc_entrypoint=cli;"},
				{Type: "text", Text: "keep me"},
			}},
		}}
		out := a.applyChatRules(req)
		blocks := out.Messages[0].Content.([]dto.MediaContent)
		require.Len(t, blocks, 1)
		assert.Equal(t, "keep me", blocks[0].Text)
	})

	t.Run("字符串形态遥测头行删除", func(t *testing.T) {
		a := &Adaptor{}
		req := &dto.GeneralOpenAIRequest{Messages: []dto.Message{
			{Role: "system", Content: "x-anthropic-billing-header: cc_version=9.9.9;\nkeep me\nx-anthropic-trace-id: abc\nkeep too"},
		}}
		out := a.applyChatRules(req)
		sys := out.Messages[0].Content.(string)
		assert.Equal(t, "keep me\nkeep too", sys)
	})
}
