package deveco

// DevEco Code 渠道（华为 CodeGenie MaaS，deveco-code CLI 同源端点）常量。
//
// 请求形状依据 deveco-code CLI 源码（huawei-endpoints.json、auth-plugin.ts、
// transform.ts）与 2026-09-23 实测 curl 验证：
//   - 端点 https://cn.devecostudio.huawei.com/sse/codeGenie/maas/v2
//   - 流式 /chat/completions，非流式 /no-stream/chat/completions
//   - 鉴权 Bearer access token（约 30 分钟过期），长效凭证为 jwtToken，
//     经 GET <origin>/authrouter/auth/api/jwToken/check (refresh:true) 刷新
//   - 推理模型 body 须带 "thinking":{"type":"enabled"}，经渠道 param_override 注入

const ChannelName = "DevEco Code"

// devecoTokenCheckPath 为 access token 刷新端点路径（挂在渠道 base_url 同源下）。
const devecoTokenCheckPath = "/authrouter/auth/api/jwToken/check"

// 上游模型经云端 modelConfig 动态下发，此处为已实测可用的最小集，
// 渠道配置里可手动补充。
var ModelList = []string{
	"GLM-5.3",
}
