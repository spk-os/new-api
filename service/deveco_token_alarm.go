package service

// DevEco 渠道凭证过期告警：deveco 渠道的 token 有两类过期机制，语义按用户要求
// 收敛为"能自动续就不告警，续不了需要人工介入才告警"：
//   - jwtToken（渠道 23/24 老登录）：token.go 服务端自动续期，成功即静默；
//     刷新端点明确拒绝（deveco.ErrTokenRejected）说明续不了，需人工重新登录。
//   - accessToken（渠道 25/26 新登录）：由外部 cron 每 12h 经 Windows 换新，
//     new-api 无感知；上游返回 401（或非额度/非超限类 403）说明 cron 未覆盖到。
// 两种情况由 relay 层分类后统一调到本函数：按渠道类型守卫 + 1h 去重后发飞书
// （复用 token_alarm.go 的 spk-os-alarm webhook 与 yaml alarm.feishu-webhook 配置）。

import (
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/service/gateway"

	"github.com/gin-gonic/gin"
)

const (
	devecoTokenAlarmKind     = "deveco-token-expired"
	devecoTokenAlarmInterval = time.Hour // 需人工处理，1h 重提醒一次即可，不刷屏
)

// devEcoAlarmDispatch 可在测试中替换以捕获告警内容（模式同 token_alarm 的 alarmNowFunc）。
var devEcoAlarmDispatch = dispatchFeishuAlarm

// AlarmDevEcoTokenExpiry 在 deveco 渠道凭证无法自动续期时发飞书告警。
// 非 deveco 渠道调用是 no-op；同类告警 1h 内去重。
func AlarmDevEcoTokenExpiry(c *gin.Context, reason string) {
	if common.GetContextKeyInt(c, constant.ContextKeyChannelType) != constant.ChannelTypeDevEco {
		return
	}
	if !shouldAlarm(devecoTokenAlarmKind, devecoTokenAlarmInterval) {
		return
	}
	webhook := ""
	if cfg := gateway.GetConfig(); cfg != nil && cfg.Alarm != nil {
		webhook = cfg.Alarm.FeishuWebhook
	}
	detail := fmt.Sprintf(
		"类型: deveco 凭证无法自动续期\n原因: %s\n渠道ID: %d\n渠道名: %s\n时间: %s\n处理: 在 Windows 端重新登录对应账号后执行 ha/bin/deveco-refresh-<账号>.sh；重置周期 00:00 前该渠道由其他渠道故障转移兜底",
		reason,
		common.GetContextKeyInt(c, constant.ContextKeyChannelId),
		common.GetContextKeyString(c, constant.ContextKeyChannelName),
		time.Now().Format("2006-01-02 15:04:05"),
	)
	devEcoAlarmDispatch(webhook, "DevEco 渠道 Token 过期告警", detail)
}
