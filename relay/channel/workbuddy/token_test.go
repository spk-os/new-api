package workbuddy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCredential(t *testing.T) {
	// JSON 凭据（workbuddy-desktop.info 同源结构）解析出全部字段
	cred := parseCredential(`{"accessToken":"at-1","refreshToken":"rt-1","uid":"u1","enterpriseId":"e1","domain":"workbuddy.cn"}`)
	require.NotNil(t, cred)
	assert.Equal(t, "at-1", cred.AccessToken)
	assert.Equal(t, "rt-1", cred.RefreshToken)
	assert.Equal(t, "u1", cred.UID)
	assert.Equal(t, "e1", cred.EnterpriseID)
	assert.Equal(t, "workbuddy.cn", cred.Domain)

	// 非 JSON 的原始 accessToken 整串透传，无刷新能力
	cred = parseCredential("raw-access-token")
	require.NotNil(t, cred)
	assert.Equal(t, "raw-access-token", cred.AccessToken)
	assert.Empty(t, cred.RefreshToken)
}

func TestExpiresAtFromUnix(t *testing.T) {
	// 毫秒时间戳归一化为秒
	assert.True(t, expiresAtFromUnix(time.Now().Add(time.Hour).UnixMilli()).After(time.Now()))
	// 秒时间戳原样
	assert.True(t, expiresAtFromUnix(time.Now().Add(time.Hour).Unix()).After(time.Now()))
	// 过期/非法值返回零值，交给调用方走兜底 TTL
	assert.True(t, expiresAtFromUnix(0).IsZero())
	assert.True(t, expiresAtFromUnix(1).IsZero())
	assert.True(t, expiresAtFromUnix(time.Now().Add(-time.Hour).Unix()).IsZero())
}

func TestWorkbuddyTokenRefreshURL(t *testing.T) {
	url, err := workbuddyTokenRefreshURL("https://www.workbuddy.cn")
	require.NoError(t, err)
	assert.Equal(t, "https://www.workbuddy.cn/v2/plugin/auth/token/refresh", url)

	_, err = workbuddyTokenRefreshURL("not-a-url")
	assert.Error(t, err)
}

func TestRefreshEnvelopeVariants(t *testing.T) {
	// 扁平形态
	var e refreshEnvelope
	require.NoError(t, common.UnmarshalJsonStr(`{"accessToken":"a","refreshToken":"r"}`, &e))
	r := e.resolved()
	require.NotNil(t, r)
	assert.Equal(t, "a", r.AccessToken)

	// {auth:{...}} 包裹（workbuddy-desktop.info 同源结构）
	e = refreshEnvelope{}
	require.NoError(t, common.UnmarshalJsonStr(`{"auth":{"accessToken":"a2","refreshToken":"r2","expiresAt":1893456000000}}`, &e))
	r = e.resolved()
	require.NotNil(t, r)
	assert.Equal(t, "a2", r.AccessToken)
	assert.Equal(t, "r2", r.RefreshToken)

	// {data:{...}} 包裹
	e = refreshEnvelope{}
	require.NoError(t, common.UnmarshalJsonStr(`{"data":{"accessToken":"a3"}}`, &e))
	r = e.resolved()
	require.NotNil(t, r)
	assert.Equal(t, "a3", r.AccessToken)

	// 缺 accessToken 视为解析失败
	e = refreshEnvelope{}
	require.NoError(t, common.UnmarshalJsonStr(`{"auth":{"foo":1}}`, &e))
	assert.Nil(t, e.resolved())
}

func TestRefreshTokenRejected4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "rt-old", r.Header.Get("X-Refresh-Token"))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, _, _, err := workbuddyRefreshToken(context.Background(), "rt-old", srv.URL)
	require.ErrorIs(t, err, ErrTokenRejected)
}

func TestRefreshTokenRotatedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"auth":{"accessToken":"at-new","refreshToken":"rt-new"}}`))
	}))
	defer srv.Close()

	access, refresh, expiresAt, err := workbuddyRefreshToken(context.Background(), "rt-old", srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "at-new", access)
	assert.Equal(t, "rt-new", refresh)
	assert.True(t, expiresAt.IsZero(), "响应无 expiresAt 时应返回零值走兜底 TTL")
}

func TestResolveCredentialRawTokenPassthrough(t *testing.T) {
	cred, err := ResolveCredential(context.Background(), "raw-access-token", "https://www.workbuddy.cn")
	require.NoError(t, err)
	assert.Equal(t, "raw-access-token", cred.AccessToken)
}
