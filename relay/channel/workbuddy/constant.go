package workbuddy

// WorkBuddy 渠道（腾讯桌面 AI 助手，copilot.tencent.com 同源端点）常量。
//
// 请求形状依据 design/info/workbuddy-auth-llm-mechanism.md（2026-10-14 实测取证）：
//   - 端点三域名：www.workbuddy.cn（主）/ copilot.tencent.com（备）/
//     wb.tencentbuddy.com（兜底），渠道 base_url 任选其一
//   - 聊天 POST /v2/chat/completions，仅支持 SSE（强制 stream:true，
//     非流式客户端由本地聚合 SSE 还原为 JSON）
//   - 鉴权 Bearer accessToken + X-User-Id/X-Enterprise-Id/X-Domain 等头，
//     accessToken 过期经 POST /v2/plugin/auth/token/refresh（头 X-Refresh-Token）刷新
//   - body 约束：developer 角色被拒（400 code 11128，须改 system）；
//     tool_choice 仅接受字符串；reasoning_effort 白名单 low/medium/high/
//     xhigh/max（off 被拒 11150）
//   - UA 决定 /v3/config 下发的模型花名册，固定 CLI/2.63.2 形态

const ChannelName = "WorkBuddy"

// workbuddyChatPath 为聊天端点路径（挂在渠道 base_url 同源下）。
const workbuddyChatPath = "/v2/chat/completions"

// workbuddyTokenRefreshPath 为 accessToken 刷新端点路径（挂在渠道 base_url 同源下）。
const workbuddyTokenRefreshPath = "/v2/plugin/auth/token/refresh"

// workbuddyClientUA 上游按 UA 决定模型花名册，固定 CLI/CodeBuddy 形态。
const workbuddyClientUA = "CLI/2.63.2 CodeBuddy/2.63.2"

// workbuddySiteOrigin 请求 Origin/Referer 统一为主域名（与 CLI 一致，
// 不随 base_url 选用的备用域名变化）。
const workbuddySiteOrigin = "https://www.workbuddy.cn"

// reasoningEfforts 白名单：上游拒绝 off（code 11150）与未知值，遇到白名单
// 之外的取值一律移除字段而不是透传。
var reasoningEfforts = map[string]bool{
	"low":    true,
	"medium": true,
	"high":   true,
	"xhigh":  true,
	"max":    true,
}

// 上游模型目录经 /v3/config 动态下发，此处为已实测存在的内置模型集，
// 渠道配置里可手动补充。
var ModelList = []string{
	"hy4-preview",
	"deepseek-v4.1-flash",
	"step-5-preview",
	"qwen3.8-max",
	"GLM_5_3_w8a8c8",
	"hy3",
	"kimi-k3",
	"deepseek-v4-pro",
}
