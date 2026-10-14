package deveco

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// deveco 渠道鉴权模型（deveco CLI token-refresh.ts 的 Go 移植）：
//   - 渠道 key 存长效 jwtToken（华为账号 OAuth 登录产物）
//   - access token 通过 GET <origin>/authrouter/auth/api/jwToken/check 换取，
//     头 refresh:true + jwtToken:<jwt>，响应 {status,userInfo:{accessToken}}，
//     有效期约 30 分钟（响应无 expires 字段，CLI 硬编码 ACCESS_TOKEN_EXPIRES_MS）
//   - 提前 2 分钟主动刷新；刷新失败后 30 秒冷却，防止失效 jwt 打爆刷新端点
//   - 刷新无状态、jwtToken 不轮换，故只需进程内缓存，无需写回渠道 key
const (
	devecoAccessTokenTTL  = 30 * time.Minute
	devecoRefreshLead    = 2 * time.Minute
	devecoRefreshCooldown = 30 * time.Second
)

var devecoHTTPClient = &http.Client{Timeout: 15 * time.Second}

// devecoErrRefreshCooldown 表示前一次刷新刚失败、冷却期内直接报错不打上游。
var devecoErrRefreshCooldown = errors.New("deveco token refresh recently failed, in cooldown")

// ErrTokenRejected 表示刷新端点明确拒绝了 jwtToken（HTTP 4xx 或 status=false）：
// token 已过期/被吊销，服务端无法自动续期，需要人工在 Windows 端重新登录。
// 网络类失败与 5xx 不使用该哨兵，避免瞬时故障误告警（relay 层据此触发飞书告警）。
var ErrTokenRejected = errors.New("deveco jwtToken rejected by refresh endpoint (expired or revoked)")

type devecoTokenResponse struct {
	Status   bool `json:"status"`
	UserInfo struct {
		AccessToken string `json:"accessToken"`
	} `json:"userInfo"`
}

type devecoTokenState struct {
	mu            sync.Mutex
	access        string
	expiresAt     time.Time
	lastFailureAt time.Time
}

// 渠道 key 既支持长效 jwtToken，也支持直接粘贴的 access token（便于短期
// 验证）：非 JWT 形态（非两段 '.'）直接透传，跳过刷新流程。
func isJWTFormat(key string) bool {
	return strings.Count(key, ".") == 2
}

// 同一 jwtToken 的并发刷新在同一把互斥锁上串行，天然去重（对应 CLI 的
// 在途刷新共享）；多渠道共用同一账号时按 key 共享缓存。
var devecoTokenCache sync.Map // jwtToken -> *devecoTokenState

// devecoTokenCheckURL 从渠道 base_url 推导刷新端点：
// https://cn.devecostudio.huawei.com/sse/codeGenie/maas/v2
//   → https://cn.devecostudio.huawei.com/authrouter/auth/api/jwToken/check
func devecoTokenCheckURL(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid deveco channel base_url: %q", baseURL)
	}
	parsed.Path = devecoTokenCheckPath
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// getAccessToken 返回可用的 deveco access token。key 为 jwtToken 时经刷新
// 端点换取并缓存；否则视为静态 access token 直接返回。
func getAccessToken(ctx context.Context, key, baseURL string) (string, error) {
	if !isJWTFormat(key) {
		return key, nil
	}
	stateAny, _ := devecoTokenCache.LoadOrStore(key, &devecoTokenState{})
	state := stateAny.(*devecoTokenState)

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.access != "" && time.Now().Before(state.expiresAt) {
		return state.access, nil
	}
	// 前一次刷新失败的冷却期内：宁可让上游拿旧 token 自行裁决（401 走渠道
	// 报错路径），也不在冷却期内反复打刷新端点；无旧 token 则直接报错。
	if !state.lastFailureAt.IsZero() && time.Since(state.lastFailureAt) < devecoRefreshCooldown {
		if state.access != "" {
			return state.access, nil
		}
		return "", devecoErrRefreshCooldown
	}

	checkURL, err := devecoTokenCheckURL(baseURL)
	if err != nil {
		return "", err
	}
	access, err := devecoFetchAccessToken(ctx, key, checkURL)
	if err != nil {
		state.lastFailureAt = time.Now()
		return "", fmt.Errorf("deveco token refresh failed: %w", err)
	}
	state.access = access
	state.expiresAt = time.Now().Add(devecoAccessTokenTTL - devecoRefreshLead)
	state.lastFailureAt = time.Time{}
	return access, nil
}

func devecoFetchAccessToken(ctx context.Context, jwt, checkURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("refresh", "true")
	req.Header.Set("jwtToken", jwt)

	resp, err := devecoHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return "", fmt.Errorf("%w: jwToken/check HTTP %d", ErrTokenRejected, resp.StatusCode)
		}
		return "", fmt.Errorf("jwToken/check HTTP %d", resp.StatusCode)
	}
	var result devecoTokenResponse
	if err := common.DecodeJson(resp.Body, &result); err != nil {
		return "", err
	}
	if !result.Status || result.UserInfo.AccessToken == "" {
		return "", fmt.Errorf("%w: status=%v", ErrTokenRejected, result.Status)
	}
	return result.UserInfo.AccessToken, nil
}
