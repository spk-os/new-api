package workbuddy

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"

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

	t.Run("system 数组形态 text 块被改写", func(t *testing.T) {
		a := &Adaptor{}
		req := &dto.GeneralOpenAIRequest{Messages: []dto.Message{
			{Role: "system", Content: []dto.MediaContent{
				{Type: "text", Text: fp},
				{Type: "text", Text: "env: linux"},
			}},
			{Role: "user", Content: "hi"},
		}}
		out := a.applyChatRules(req)
		blocks, ok := out.Messages[0].Content.([]dto.MediaContent)
		require.True(t, ok)
		assert.Equal(t, wantReplaced, blocks[0].Text)
		assert.Equal(t, "env: linux", blocks[1].Text)
	})

	t.Run("非 system 角色不改写", func(t *testing.T) {
		a := &Adaptor{}
		req := &dto.GeneralOpenAIRequest{Messages: []dto.Message{
			{Role: "user", Content: fp},
		}}
		out := a.applyChatRules(req)
		assert.Equal(t, fp, out.Messages[0].Content)
	})

	t.Run("system 不含指纹句原样保留", func(t *testing.T) {
		a := &Adaptor{}
		// 句中位置与前导换行均不触发上游风控，也不应被改写
		for _, content := range []string{
			"Prefix text. " + fp,
			"\n" + fp,
			"You are Claude Code, Anthropic's official CLI",
		} {
			req := &dto.GeneralOpenAIRequest{Messages: []dto.Message{
				{Role: "system", Content: content},
			}}
			out := a.applyChatRules(req)
			assert.Equal(t, content, out.Messages[0].Content)
		}
	})
}
