package service

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func devecoAlarmTestContext(channelType int) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
	common.SetContextKey(c, constant.ContextKeyChannelId, 26)
	common.SetContextKey(c, constant.ContextKeyChannelName, "DevEco Code_9227")
	return c
}

func TestAlarmDevEcoTokenExpiryGuardAndDedupe(t *testing.T) {
	alarmDedupeMap.Delete(devecoTokenAlarmKind)
	t.Cleanup(func() { alarmDedupeMap.Delete(devecoTokenAlarmKind) })

	var dispatched []string
	prev := devEcoAlarmDispatch
	devEcoAlarmDispatch = func(webhookURL, title, detail string) {
		dispatched = append(dispatched, title)
	}
	t.Cleanup(func() { devEcoAlarmDispatch = prev })

	// 非 deveco 渠道：no-op，不消费去重配额
	AlarmDevEcoTokenExpiry(devecoAlarmTestContext(1), "reason-a")
	require.Empty(t, dispatched)

	// deveco 渠道：首次发告警，1h 内同类去重
	AlarmDevEcoTokenExpiry(devecoAlarmTestContext(constant.ChannelTypeDevEco), "jwtToken 已失效")
	require.Len(t, dispatched, 1)
	AlarmDevEcoTokenExpiry(devecoAlarmTestContext(constant.ChannelTypeDevEco), "上游 401 拒绝凭证")
	require.Len(t, dispatched, 1, "1h 去重窗口内同类告警不得重复发送")
}

func TestAlarmDevEcoTokenExpiryDetailContent(t *testing.T) {
	alarmDedupeMap.Delete(devecoTokenAlarmKind)
	t.Cleanup(func() { alarmDedupeMap.Delete(devecoTokenAlarmKind) })

	var gotTitle, gotDetail string
	prev := devEcoAlarmDispatch
	devEcoAlarmDispatch = func(webhookURL, title, detail string) {
		gotTitle, gotDetail = title, detail
	}
	t.Cleanup(func() { devEcoAlarmDispatch = prev })

	AlarmDevEcoTokenExpiry(devecoAlarmTestContext(constant.ChannelTypeDevEco), "jwtToken 已失效且服务端无法自动续期")
	assert.Equal(t, "DevEco 渠道 Token 过期告警", gotTitle)
	assert.Contains(t, gotDetail, "渠道ID: 26")
	assert.Contains(t, gotDetail, "DevEco Code_9227")
	assert.Contains(t, gotDetail, "jwtToken 已失效且服务端无法自动续期")
	assert.Contains(t, gotDetail, "deveco-refresh")
}
