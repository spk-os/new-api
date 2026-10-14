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
