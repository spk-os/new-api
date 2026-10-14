package service

// WorkBuddy 渠道凭证过期告警：workbuddy 渠道的 accessToken 由 token.go 服务端
// 自动刷新，成功即静默；刷新端点明确拒绝（workbuddy.ErrTokenRejected）说明
// refreshToken 已失效（上游 401/code 12153 session_dead 同义），无法自动续期，
// 需人工在 Windows 端 WorkBuddy 重新登录并重新导出凭据后更新渠道 key。
// 由 relay 层分类后统一调到本函数：按渠道类型守卫 + 1h 去重后发飞书
// （复用 deveco_token_alarm.go 同款的 spk-os-alarm webhook 配置）。

import (
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/service/gateway"

	"github.com/gin-gonic/gin"
)

const (
	workbuddyTokenAlarmKind     = "workbuddy-token-expired"
	workbuddyTokenAlarmInterval = time.Hour // 需人工处理，1h 重提醒一次即可，不刷屏
)

// workbuddyAlarmDispatch 可在测试中替换以捕获告警内容（模式同 deveco_token_alarm 的 devEcoAlarmDispatch）。
var workbuddyAlarmDispatch = dispatchFeishuAlarm

// AlarmWorkBuddyTokenExpiry 在 workbuddy 渠道凭证无法自动续期时发飞书告警。
// 非 workbuddy 渠道调用是 no-op；同类告警 1h 内去重。
func AlarmWorkBuddyTokenExpiry(c *gin.Context, reason string) {
	if common.GetContextKeyInt(c, constant.ContextKeyChannelType) != constant.ChannelTypeWorkBuddy {
		return
	}
	if !shouldAlarm(workbuddyTokenAlarmKind, workbuddyTokenAlarmInterval) {
		return
	}
	webhook := ""
	if cfg := gateway.GetConfig(); cfg != nil && cfg.Alarm != nil {
		webhook = cfg.Alarm.FeishuWebhook
	}
	detail := fmt.Sprintf(
		"类型: workbuddy 凭证无法自动续期\n原因: %s\n渠道ID: %d\n渠道名: %s\n时间: %s\n处理: 在 Windows 端 WorkBuddy 重新登录后重新导出凭据 JSON 更新渠道 key；该渠道由其他渠道故障转移兜底",
		reason,
		common.GetContextKeyInt(c, constant.ContextKeyChannelId),
		common.GetContextKeyString(c, constant.ContextKeyChannelName),
		time.Now().Format("2006-01-02 15:04:05"),
	)
	workbuddyAlarmDispatch(webhook, "WorkBuddy 渠道 Token 过期告警", detail)
}
