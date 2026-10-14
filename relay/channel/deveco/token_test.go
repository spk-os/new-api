package deveco

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

const devecoTestBaseURL = "https://cn.devecostudio.huawei.com/sse/codeGenie/maas/v2"

func devecoNewRefreshServer(t *testing.T, hits *atomic.Int32, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		require.Equal(t, "true", r.Header.Get("refresh"))
		require.NotEmpty(t, r.Header.Get("jwtToken"))
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestDevecoGetAccessTokenPlainKeyPassthrough(t *testing.T) {
	hits := &atomic.Int32{}
	server := devecoNewRefreshServer(t, hits, func(w http.ResponseWriter, r *http.Request) {
		t.Error("plain access-token key must not hit the refresh endpoint")
	})

	// 非 JWT 形态的 key 视为静态 access token，直接透传
	access, err := getAccessToken(context.Background(), "plain-access-token", server.URL)
	require.NoError(t, err)
	assert.Equal(t, "plain-access-token", access)
	assert.Equal(t, int32(0), hits.Load())
}

func TestDevecoGetAccessTokenRefreshAndCache(t *testing.T) {
	hits := &atomic.Int32{}
	server := devecoNewRefreshServer(t, hits, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":true,"userInfo":{"accessToken":"acc-1","refreshToken":"r","realName":"true"}}`))
	})

	jwt := "header.payload.signature"
	access, err := getAccessToken(context.Background(), jwt, server.URL)
	require.NoError(t, err)
	assert.Equal(t, "acc-1", access)
	require.Equal(t, int32(1), hits.Load())

	// TTL 内第二次获取走缓存，不再请求刷新端点
	access, err = getAccessToken(context.Background(), jwt, server.URL)
	require.NoError(t, err)
	assert.Equal(t, "acc-1", access)
	assert.Equal(t, int32(1), hits.Load())
}

func TestDevecoGetAccessTokenFailureCooldown(t *testing.T) {
	hits := &atomic.Int32{}
	server := devecoNewRefreshServer(t, hits, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	jwt := "cooldown.header.payload"
	_, err := getAccessToken(context.Background(), jwt, server.URL)
	require.Error(t, err)
	require.Equal(t, int32(1), hits.Load())
	// 5xx 属瞬时故障，不是"token 被拒"，不得触发人工告警语义
	assert.False(t, errors.Is(err, ErrTokenRejected))

	// 冷却期内无缓存 token：直接报错，不再打刷新端点
	_, err = getAccessToken(context.Background(), jwt, server.URL)
	require.Error(t, err)
	assert.Equal(t, int32(1), hits.Load())
}

func TestDevecoGetAccessTokenRejectedToken(t *testing.T) {
	hits := &atomic.Int32{}
	server := devecoNewRefreshServer(t, hits, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":false,"userInfo":{}}`))
	})

	// 服务端明确拒绝（jwt 已失效）：报错、不缓存，且标记为需人工介入的 ErrTokenRejected
	_, err := getAccessToken(context.Background(), "rejected.header.payload", server.URL)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrTokenRejected)
}

func TestDevecoGetAccessTokenRejectedHTTP4xx(t *testing.T) {
	hits := &atomic.Int32{}
	server := devecoNewRefreshServer(t, hits, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})

	// 刷新端点 4xx 同样是"token 被拒"（过期/吊销），须归入 ErrTokenRejected
	_, err := getAccessToken(context.Background(), "http401.header.payload", server.URL)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrTokenRejected)
}

func TestDevecoTokenCheckURL(t *testing.T) {
	checkURL, err := devecoTokenCheckURL(devecoTestBaseURL)
	require.NoError(t, err)
	assert.Equal(t, "https://cn.devecostudio.huawei.com/authrouter/auth/api/jwToken/check", checkURL)

	_, err = devecoTokenCheckURL("not a url")
	assert.Error(t, err)
}

func TestDevecoGetRequestURL(t *testing.T) {
	adaptor := &Adaptor{}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl: devecoTestBaseURL,
		},
		RelayMode: relayconstant.RelayModeChatCompletions,
	}

	info.IsStream = true
	streamURL, err := adaptor.GetRequestURL(info)
	require.NoError(t, err)
	assert.Equal(t, devecoTestBaseURL+"/chat/completions", streamURL)

	info.IsStream = false
	noStreamURL, err := adaptor.GetRequestURL(info)
	require.NoError(t, err)
	assert.Equal(t, devecoTestBaseURL+"/no-stream/chat/completions", noStreamURL)

	// Claude 格式（/v1/messages）的 RelayMode 是 Unknown，body 经通用转换
	// 后仍打向本上游；若未按 RelayFormat 映射会落入内嵌 openai 分支拼出
	// /v1/chat/completions 导致上游 404。
	claudeInfo := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl: devecoTestBaseURL,
		},
		RelayMode:   relayconstant.RelayModeUnknown,
		RelayFormat: types.RelayFormatClaude,
	}
	claudeInfo.IsStream = true
	claudeStreamURL, err := adaptor.GetRequestURL(claudeInfo)
	require.NoError(t, err)
	assert.Equal(t, devecoTestBaseURL+"/chat/completions", claudeStreamURL)

	claudeInfo.IsStream = false
	claudeNoStreamURL, err := adaptor.GetRequestURL(claudeInfo)
	require.NoError(t, err)
	assert.Equal(t, devecoTestBaseURL+"/no-stream/chat/completions", claudeNoStreamURL)
}

// TestDevecoDoRequestDispatch 锁定嵌入派发不变量：经继承的 DoRequest 发出的
// 请求必须命中本类型的 URL 改写与头注入。若缺少 DoRequest 覆盖，内层
// openai.Adaptor 会被传入 DoApiRequest，导致 URL 变成 /v1/chat/completions
// 且 deveco 头全部缺失（曾在 green 节点复现为上游 404）。
func TestDevecoDoRequestDispatch(t *testing.T) {
	var gotPath, gotLang, gotChatID, gotImprovement, gotAccept atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		gotLang.Store(r.Header.Get("lang"))
		gotChatID.Store(r.Header.Get("Chat-Id"))
		gotImprovement.Store(r.Header.Get("X-DevEco-Improvement-Enabled"))
		gotAccept.Store(r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(server.Close)

	// InitHttpClient 在服务启动时初始化共享 http client，裸测试环境须显式初始化
	service.InitHttpClient()

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	body := `{"model":"GLM-5.3","messages":[{"role":"user","content":"hi"}],"stream":false,"max_tokens":16}`
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))

	adaptor := &Adaptor{}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl: server.URL,
			ApiKey:         "plain-access-token",
		},
		RelayMode: relayconstant.RelayModeChatCompletions,
	}

	resp, err := adaptor.DoRequest(c, info, strings.NewReader(body))
	require.NoError(t, err)
	httpResp, ok := resp.(*http.Response)
	require.True(t, ok)
	require.Equal(t, http.StatusOK, httpResp.StatusCode)

	assert.Equal(t, "/no-stream/chat/completions", gotPath.Load())
	assert.Equal(t, "en", gotLang.Load())
	assert.Len(t, gotChatID.Load(), 32)
	assert.Equal(t, "false", gotImprovement.Load())
	// 客户端 Accept: application/json 不得透传到流式端点（上游 406）；
	// 非流式请求须显式带 application/json
	assert.Equal(t, "application/json", gotAccept.Load())
}

// TestDevecoStreamAcceptOverride 锁定 Claude Code 实际请求形态：客户端带
// Accept: application/json 的流式请求，Accept 必须被覆写为 text/event-stream，
// 否则上游流式端点回 406 Not Acceptable（曾在线上复现）。
func TestDevecoStreamAcceptOverride(t *testing.T) {
	var gotAccept, gotPath atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept.Store(r.Header.Get("Accept"))
		gotPath.Store(r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(server.Close)

	service.InitHttpClient()

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	body := `{"model":"GLM-5.3","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":16}`
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	c.Request.Header.Set("Accept", "application/json")

	adaptor := &Adaptor{}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl: server.URL,
			ApiKey:         "plain-access-token",
		},
		RelayMode:   relayconstant.RelayModeUnknown,
		RelayFormat: types.RelayFormatClaude,
		IsStream:    true,
	}

	resp, err := adaptor.DoRequest(c, info, strings.NewReader(body))
	require.NoError(t, err)
	httpResp, ok := resp.(*http.Response)
	require.True(t, ok)
	require.Equal(t, http.StatusOK, httpResp.StatusCode)

	assert.Equal(t, "/chat/completions", gotPath.Load())
	assert.Equal(t, "text/event-stream", gotAccept.Load())
}
