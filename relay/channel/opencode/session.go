package opencode

import (
	"crypto/rand"
	"math/big"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/dto"
)

// OpenCode Zen 客户端指纹与免费门禁所需元素。
//
// 门禁规则（缺一即 403/426，~60 次对照实验定稿）：
//  1. 上游请求必须 stream:true（非流式一律 403，由 adaptor 侧强制并聚合）
//  2. tools 须包含 bash/edit/read（缺则注入中性 stub，用户 tools 原样保留）
//  3. session id 须为 opencode id.ts 算法格式：`<prefix>_` + 12 位 hex
//     （(ms*0x1000 + counter) 取低 48 位）+ 14 位 base62 = 26 字符；
//     不需服务端预注册，固定占位串（如 ses_00000）必 403
//  4. 四个 x-opencode-* 头（client/project/session/request）
//  5. UA 完整串且版本 >=1.17.0（版本不足 → 426）
//  6. 上游首字节 ~23s，调用方超时须 >=60s

// OPENCODE_CLIENT_UA 真实 opencode 客户端完整 UA 串（版本须 >=1.17.0）。
const OPENCODE_CLIENT_UA = "opencode/1.18.31 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"

// 头取值：无 git repo 时 project 为字面量 "global"，client 为 flags.client "cli"。
const (
	opencodeClient  = "cli"
	opencodeProject = "global"
)

const (
	idLength       = 26
	hexLength      = 12
	timestampShift = 0x1000 // now = ms * 0x1000 + counter（opencode id.ts 同款单调编码）
	low48Mask      = (1 << 48) - 1
)

const base62Chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

var (
	idMu      sync.Mutex
	lastMs    int64
	msCounter int64
)

// GenerateOpencodeId 复刻 opencode id.ts create()：
// `<prefix>_` + 12 位 hex(单调时间) + 14 位 base62(随机)，总长 26 字符。
func GenerateOpencodeId(prefix string) string {
	idMu.Lock()
	now := time.Now().UnixMilli()
	if now != lastMs {
		lastMs = now
		msCounter = 0
	}
	msCounter++
	counter := msCounter
	idMu.Unlock()

	encoded := (now*timestampShift + counter) & low48Mask
	return prefix + "_" + zeroPadHex(encoded) + randomBase62(idLength-hexLength)
}

// 免费门禁最少必需的 3 个 opencode 内置工具名。
var gateToolNames = []string{"bash", "edit", "read"}

// stub 的中性 description（description 内容参与门禁校验：
// 反调用文案如 "Do not call this tool..." 会被拒）。
const gateToolDescription = "tool"

// ApplyGateTools 保证发往上游的 tools 含 bash/edit/read：
// 客户端已有这些名字时不注入任何东西，否则追加 stub 且用户 tools 原样保留。
func ApplyGateTools(tools []dto.ToolCallRequest) []dto.ToolCallRequest {
	existing := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		existing[t.Function.Name] = struct{}{}
	}
	missing := make([]string, 0, len(gateToolNames))
	for _, n := range gateToolNames {
		if _, ok := existing[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return tools
	}
	for _, n := range missing {
		tools = append(tools, dto.ToolCallRequest{
			Type: "function",
			Function: dto.FunctionRequest{
				Name:        n,
				Description: gateToolDescription,
				Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		})
	}
	return tools
}

func zeroPadHex(v int64) string {
	const hexDigits = "0123456789abcdef"
	buf := make([]byte, hexLength)
	for i := hexLength - 1; i >= 0; i-- {
		buf[i] = hexDigits[v&0xf]
		v >>= 4
	}
	return string(buf)
}

func randomBase62(length int) string {
	buf := make([]byte, length)
	max := big.NewInt(int64(len(base62Chars)))
	for i := range buf {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			// crypto/rand 失败极罕见（系统熵源故障），回退到固定字符保证不 panic
			buf[i] = base62Chars[0]
			continue
		}
		buf[i] = base62Chars[n.Int64()]
	}
	return string(buf)
}
