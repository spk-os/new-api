package opencode

// OpenCode Zen 渠道（免费层）常量。
//
// zen 自 2026-09 起对匿名免费请求有门禁（403 "OpenCode's free tier can only
// be used from within OpenCode"）。完整门禁规则与 ~60 次对照实验定稿见
// SPK-OS 知识库 docs/soft/opensource/freellmapi.mdx §5.3 及 freellmapi
// server/src/lib/opencode-session.ts（Go 移植蓝本）。

const ChannelName = "OpenCode"

// 免费层 starter 模型名单。zen 免费名单上游会轮换，最终以渠道配置为准。
var ModelList = []string{
	"mimo-v2.5-free",
	"deepseek-v4-flash-free",
	"ling-3.0-flash-fin-free",
	"nemotron-3.5-lightning-free",
	"big-pickle",
}
