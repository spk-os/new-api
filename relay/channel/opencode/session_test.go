package opencode

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/QuantumNous/new-api/dto"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var opencodeIdPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

func TestGenerateOpencodeIdFormat(t *testing.T) {
	id := GenerateOpencodeId("ses")
	require.Len(t, id, 1+len("ses")+idLength, "ses_ 前缀 + 26 字符主体")
	assert.True(t, opencodeIdPattern.MatchString(id),
		"id 须为 ses_ + 12 位 hex + 14 位 base62，got %s", id)

	id = GenerateOpencodeId("msg")
	assert.True(t, regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`).MatchString(id),
		"msg_ 前缀同算法，got %s", id)
}

// 注：(ms*0x1000 + counter) 取低 48 位在当前纪元下高位会被截断
//（ms*0x1000 ≈ 2^53 > 2^48），12 位 hex 部分无法解码回真实时间——
// 与 freellmapi 参考实现及 opencode id.ts 行为一致，门禁只校验格式，
// 故这里只断言格式与唯一性，不做时间回解。

func TestGenerateOpencodeIdUniqueWithinSameMillisecond(t *testing.T) {
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		id := GenerateOpencodeId("ses")
		_, dup := seen[id]
		require.False(t, dup, "同一毫秒内 id 不得重复: %s", id)
		seen[id] = struct{}{}
	}
}

func stubToolNames(tools []dto.ToolCallRequest) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Function.Name)
	}
	return names
}

func TestApplyGateToolsInjectsMissingStubs(t *testing.T) {
	result := ApplyGateTools(nil)
	assert.ElementsMatch(t, []string{"bash", "edit", "read"}, stubToolNames(result))
	for _, tool := range result {
		assert.Equal(t, "function", tool.Type)
		assert.Equal(t, gateToolDescription, tool.Function.Description,
			"stub description 须为中性值，反调用文案会被门禁拒绝")
		assert.Equal(t, map[string]any{"type": "object", "properties": map[string]any{}},
			tool.Function.Parameters)
	}
}

func TestApplyGateToolsKeepsUserTools(t *testing.T) {
	user := []dto.ToolCallRequest{
		{Type: "function", Function: dto.FunctionRequest{Name: "web_search", Description: "search"}},
	}
	result := ApplyGateTools(user)
	names := stubToolNames(result)
	assert.Len(t, result, 4)
	assert.Equal(t, "web_search", result[0].Function.Name, "用户 tools 须原样保留在原位")
	assert.Equal(t, "search", result[0].Function.Description)
	assert.ElementsMatch(t, append([]string{"web_search"}, gateToolNames...), names)
}

func TestApplyGateToolsNoDuplicateInjection(t *testing.T) {
	// 客户端（Claude Code / opencode 等真实 agent）自带全部三件时不注入
	user := make([]dto.ToolCallRequest, 0, len(gateToolNames))
	for _, n := range gateToolNames {
		user = append(user, dto.ToolCallRequest{
			Type:     "function",
			Function: dto.FunctionRequest{Name: n, Description: fmt.Sprintf("%s tool", n)},
		})
	}
	result := ApplyGateTools(user)
	assert.Equal(t, user, result, "bash/edit/read 齐备时不得注入任何 stub")
}
