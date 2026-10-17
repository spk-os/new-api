package workbuddy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// workbuddy 渠道鉴权模型（design/info/workbuddy-auth-llm-mechanism.md）：
//   - 渠道 key 存从 Windows 端导出的凭据 JSON：
//     {"accessToken":"...","refreshToken":"...","uid":"...","enterpriseId":"...","domain":"..."}
//     （workbuddy-desktop.info 同源结构；5.6+ envelope 加密需 WorkBuddy.exe
//     helper 解密，导出一次性完成）
//   - accessToken 过期经 POST <origin>/v2/plugin/auth/token/refresh（头
//     X-Refresh-Token）刷新，CLI 侧 401 后也是刷新重试一次
//   - refreshToken 可能轮换：进程内缓存最新值；刷新被拒时回退原始凭据里的
//     refreshToken 重试一次（服务重启后缓存丢失的场景）
//   - key 也支持直接粘贴 accessToken（便于短期验证），跳过刷新流程，此时
//     X-User-Id 等身份头缺失，部分接口可能被上游风控拒绝
const (
	// 响应无任何过期信息时的兜底 TTL。实测刷新响应携带 expiresIn=2419200
	//（accessToken 有效期 28 天），此兜底仅在异常响应下生效。
	workbuddyAccessTokenTTL  = 30 * time.Minute
	workbuddyRefreshLead     = 2 * time.Minute
	workbuddyRefreshCooldown = 30 * time.Second
)

var workbuddyHTTPClient = &http.Client{Timeout: 15 * time.Second}

// workbuddyErrRefreshCooldown 表示前一次刷新刚失败、冷却期内直接报错不打上游。
var workbuddyErrRefreshCooldown = errors.New("workbuddy token refresh recently failed, in cooldown")

// ErrTokenRejected 表示刷新端点明确拒绝了 refreshToken（HTTP 4xx 或响应缺
// accessToken）：会话已死（上游 401/code 12153 同义），服务端无法自动续期，
// 需要人工在 Windows 端 WorkBuddy 重新登录并重新导出凭据。网络类失败与 5xx
// 不使用该哨兵，避免瞬时故障误告警（relay 层据此触发飞书告警）。
var ErrTokenRejected = errors.New("workbuddy refreshToken rejected by refresh endpoint (session dead, re-login required)")

// Credential 是渠道 key 携带的凭据，也是 SetupRequestHeader 注入身份头的来源。
type Credential struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Domain       string `json:"domain"`
}

// parseCredential 解析渠道 key：JSON 凭据 → Credential；其余整串视为原始
// accessToken（无 refreshToken，不刷新，无身份头）。
func parseCredential(key string) *Credential {
	var cred Credential
	if err := common.UnmarshalJsonStr(key, &cred); err == nil && cred.AccessToken != "" {
		return &cred
	}
	return &Credential{AccessToken: key}
}

// refreshEnvelope 兼容刷新响应的多种包裹形态（实测接口文档未公开响应结构，
// 按 workbuddy-desktop.info 同源结构 {auth:{...}} 与常见 {data:{...}}、
// 扁平三种形态解析）。ExpiresAt 为绝对时间戳（秒或毫秒），ExpiresIn 为
// 相对秒数（实测线上响应 {data:{expiresIn:2419200}} 形态）。
type refreshEnvelope struct {
	AccessToken  string           `json:"accessToken"`
	RefreshToken string           `json:"refreshToken"`
	ExpiresAt    int64            `json:"expiresAt"`
	ExpiresIn    int64            `json:"expiresIn"`
	Auth         *refreshEnvelope `json:"auth"`
	Data         *refreshEnvelope `json:"data"`
}

func (e *refreshEnvelope) resolved() *refreshEnvelope {
	switch {
	case e.AccessToken != "":
		return e
	case e.Auth != nil && e.Auth.AccessToken != "":
		return e.Auth
	case e.Data != nil && e.Data.AccessToken != "":
		return e.Data
	}
	return nil
}

type tokenState struct {
	mu sync.Mutex
	// access/expiresAt 为当前缓存的 accessToken；refresh 为最新 refreshToken
	//（轮换后与原始凭据不同）。
	access        string
	refresh       string
	expiresAt     time.Time
	lastFailureAt time.Time
}

var tokenCache sync.Map // channel key -> *tokenState

// workbuddyTokenRefreshURL 从渠道 base_url 推导刷新端点：
// https://www.workbuddy.cn → https://www.workbuddy.cn/v2/plugin/auth/token/refresh
func workbuddyTokenRefreshURL(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid workbuddy channel base_url: %q", baseURL)
	}
	parsed.Path = workbuddyTokenRefreshPath
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// expiresAtFromUnix 把凭据/响应里的 expiresAt（秒或毫秒时间戳）转换为绝对
// 时间；0 或已过期的值返回零值，交给调用方走兜底 TTL。
func expiresAtFromUnix(expiresAt int64) time.Time {
	if expiresAt <= 0 {
		return time.Time{}
	}
	if expiresAt > 1e12 {
		expiresAt /= 1000
	}
	t := time.Unix(expiresAt, 0)
	if time.Until(t) <= 0 {
		return time.Time{}
	}
	return t
}

// ResolveCredential 返回可用于本次请求的凭据：key 为 JSON 凭据时经刷新端点
// 续期 accessToken 并缓存；key 为原始 accessToken 时原样返回。
func ResolveCredential(ctx context.Context, key, baseURL string) (*Credential, error) {
	cred := parseCredential(key)
	if cred.RefreshToken == "" {
		return cred, nil
	}

	stateAny, _ := tokenCache.LoadOrStore(key, &tokenState{refresh: cred.RefreshToken})
	state := stateAny.(*tokenState)

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.access != "" && time.Now().Before(state.expiresAt) {
		return state.credential(cred), nil
	}
	// 前一次刷新失败的冷却期内：宁可让上游拿旧 token 自行裁决（401 走渠道
	// 报错路径），也不在冷却期内反复打刷新端点；无旧 token 则直接报错。
	if !state.lastFailureAt.IsZero() && time.Since(state.lastFailureAt) < workbuddyRefreshCooldown {
		if state.access != "" {
			return state.credential(cred), nil
		}
		return nil, workbuddyErrRefreshCooldown
	}

	refreshURL, err := workbuddyTokenRefreshURL(baseURL)
	if err != nil {
		return nil, err
	}
	// 优先用缓存里轮换后的 refreshToken；被端点拒绝（4xx）时回退原始凭据
	// 的 refreshToken 重试一次——覆盖服务重启后缓存轮换值丢失、上游已按
	// 原值签发的场景。
	access, refresh, expiresAt, err := workbuddyRefreshToken(ctx, state.refresh, refreshURL)
	if errors.Is(err, ErrTokenRejected) && state.refresh != cred.RefreshToken {
		access, refresh, expiresAt, err = workbuddyRefreshToken(ctx, cred.RefreshToken, refreshURL)
	}
	if err != nil {
		state.lastFailureAt = time.Now()
		return nil, fmt.Errorf("workbuddy token refresh failed: %w", err)
	}
	state.access = access
	state.refresh = refresh
	if !expiresAt.IsZero() {
		state.expiresAt = expiresAt.Add(-workbuddyRefreshLead)
	} else {
		state.expiresAt = time.Now().Add(workbuddyAccessTokenTTL - workbuddyRefreshLead)
	}
	state.lastFailureAt = time.Time{}
	return state.credential(cred), nil
}

// credential 组装本次请求用的凭据：access/refresh 取缓存最新值，身份字段
// 取原始凭据（刷新响应不含 uid/enterpriseId）。
func (s *tokenState) credential(orig *Credential) *Credential {
	return &Credential{
		AccessToken:  s.access,
		RefreshToken: s.refresh,
		UID:          orig.UID,
		EnterpriseID: orig.EnterpriseID,
		Domain:       orig.Domain,
	}
}

func workbuddyRefreshToken(ctx context.Context, refreshToken, refreshURL string) (access, refresh string, expiresAt time.Time, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, refreshURL, nil)
	if err != nil {
		return "", "", time.Time{}, err
	}
	req.Header.Set("X-Refresh-Token", refreshToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := workbuddyHTTPClient.Do(req)
	if err != nil {
		return "", "", time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return "", "", time.Time{}, fmt.Errorf("%w: token/refresh HTTP %d", ErrTokenRejected, resp.StatusCode)
		}
		return "", "", time.Time{}, fmt.Errorf("token/refresh HTTP %d", resp.StatusCode)
	}
	var envelope refreshEnvelope
	if err := common.DecodeJson(resp.Body, &envelope); err != nil {
		return "", "", time.Time{}, err
	}
	result := envelope.resolved()
	if result == nil || result.AccessToken == "" {
		return "", "", time.Time{}, fmt.Errorf("%w: response missing accessToken", ErrTokenRejected)
	}
	if result.RefreshToken != "" {
		refresh = result.RefreshToken
	} else {
		refresh = refreshToken
	}
	// 优先绝对时间戳，其次相对秒数（实测线上响应 expiresIn=2419200 即 28 天）
	expiresAt = expiresAtFromUnix(result.ExpiresAt)
	if expiresAt.IsZero() && result.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	}
	return result.AccessToken, refresh, expiresAt, nil
}
