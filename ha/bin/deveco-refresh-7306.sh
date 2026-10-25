#!/bin/bash
# deveco 渠道 23 (DECode_7306) accessToken 自动续期
# 账号身份：7306 = wei2**** = userId 260086000075131897
# 背景：渠道 23 原存 jwtToken（服务器侧 token.go 自动续期），2026-10-24 jwtToken 被华为刷新
#       端点拒绝（status=false → ErrTokenRejected 告警），迁移为 accessToken 模式（与其他
#       deveco 渠道一致）：Windows 侧重新登录后换出 accessToken 回填渠道 key。
#       accessToken 无绑定（约 24h 有效），jwtToken 被风控绑定登录环境（仅 Windows+node 可过
#       jwToken/check），故经 ssh winbox 在 Windows 端换出。troubleshoot.md 问题 18。
# 凭证目录：C:\Users\Administrator\.config\deveco（CLI 默认目录）——7306 是默认目录的当前登录，
#       用户在 Windows 端重新登录 7306 账号即更新此处凭证（登录前请先退出其他华为账号）。
#       ⚠ 该目录是"默认登录位"：若用户在 Windows 端登录了其他 deveco 账号，expectedUserId
#       校验会拦下（ACCOUNT_MISMATCH exit 2），需重新登录 7306 恢复。
# 依赖：root 免密 ssh winbox；winbox:Temp/deveco-get-access.js（与 ha/bin/deveco-get-access.js 同步，
#       Windows Temp 目录可能被清理丢失，脚本自动检测并重新 scp）
# 注意：
#   1. PUT /api/channel/ 的 body 不得含 status 字段（UpdateChannel 校验拒绝），只传 id+key。
#   2. 网关 SaveConfig/config-sync 会用 yaml 的 key 覆盖渠道 key（正向同步），因此新 token
#      必须同时回写网关 yaml（DECode_7306 块），否则下次配置保存会把渠道 key 回写成旧 token。
set -euo pipefail

ADMIN="Authorization: Bearer 99da79052d7de01cc6ef74b968568bdf"
API="http://localhost:3500/api/channel/"
CFG_API="http://localhost:3500/api/admin/gateway/config"
LOCAL_SCRIPT="/work/spk-os/soft/basic/new-api/ha/bin/deveco-get-access.js"
WIN_SCRIPT="C:/Users/Administrator/AppData/Local/Temp/deveco-get-access.js"
WIN_CONFIG_DIR="C:/Users/Administrator/.config/deveco"
PROVIDER_NAME="DECode_7306"
CHANNEL_ID=23
EXPECTED_USER_ID="260086000075131897"

# Windows Temp 下的脚本可能被清理丢失（2026-10-24 实际发生过），先检测再自动补传
if ! ssh -o ConnectTimeout=15 -o BatchMode=yes winbox "node -e \"require('fs').accessSync(process.argv[1])\" $WIN_SCRIPT" 2>/dev/null; then
  scp -o ConnectTimeout=15 -o BatchMode=yes "$LOCAL_SCRIPT" "winbox:$WIN_SCRIPT" || {
    echo "[deveco-refresh-7306] 无法同步 $WIN_SCRIPT 到 winbox" >&2
    exit 4
  }
fi

AT=$(ssh -o ConnectTimeout=15 -o BatchMode=yes winbox "node $WIN_SCRIPT $WIN_CONFIG_DIR $EXPECTED_USER_ID" 2>&1 | tail -1 || true)
# 目录未登录/不存在（node 读 token.dek 崩溃）或账号不符时的兜底提示
if [[ -z "$AT" || "$AT" == *ENOENT* || "$AT" == *"no such file"* || "$AT" == Node.js* ]]; then
  echo "[deveco-refresh-7306] 凭证目录 $WIN_CONFIG_DIR 尚未登录（Windows 端：浏览器退出华为账号后 deveco login）" >&2
  exit 3
fi
if [[ "$AT" == ACCOUNT_MISMATCH* ]]; then
  echo "[deveco-refresh-7306] $AT (默认目录已被其他账号登录覆盖，请在 Windows 端重新登录 7306 账号)" >&2
  exit 2
fi
if [[ ! "$AT" =~ ^DgEAA ]] || [[ ${#AT} -lt 100 ]]; then
  echo "[deveco-refresh-7306] unexpected output: $AT" >&2
  exit 1
fi

HTTP_CODE=$(curl -sS -o /tmp/deveco-refresh-7306-resp.json -w '%{http_code}' -X PUT "$API" \
  -H "$ADMIN" -H "Content-Type: application/json" \
  -d "{\"id\":$CHANNEL_ID,\"key\":\"$AT\"}")
if [[ "$HTTP_CODE" != "200" ]]; then
  echo "[deveco-refresh-7306] PUT failed HTTP $HTTP_CODE: $(cat /tmp/deveco-refresh-7306-resp.json)" >&2
  exit 1
fi

# 回写网关 yaml（防止下次 config 保存用旧 key 覆盖渠道）
curl -sS "$CFG_API" -H "$ADMIN" -o /tmp/deveco-refresh-7306-cfg.json
NEW_YAML=$(PROVIDER="$PROVIDER_NAME" NEWKEY="$AT" python3 - << 'PYEOF'
import json, os, re
cfg = json.load(open('/tmp/deveco-refresh-7306-cfg.json'))
yaml_str = cfg.get('data', cfg)['yaml']
name, key = os.environ['PROVIDER'], os.environ['NEWKEY']
i = yaml_str.find('name: ' + name)
assert i > 0, f"provider {name} not in yaml"
j = yaml_str.find('\n    key: ', i)
assert j > 0 and j - i < 400, "key line not near provider block"
end = yaml_str.find('\n', j + 1)
yaml_str = yaml_str[:j] + '\n    key: ' + key + yaml_str[end:]
print(json.dumps({"yaml": yaml_str}, ensure_ascii=False))
PYEOF
)
HTTP2=$(curl -sS -o /tmp/deveco-refresh-7306-cfg-resp.json -w '%{http_code}' -X PUT "$CFG_API" \
  -H "$ADMIN" -H "Content-Type: application/json" -d "$NEW_YAML")
if [[ "$HTTP2" != "200" ]]; then
  echo "[deveco-refresh-7306] yaml PUT failed HTTP $HTTP2: $(head -c 300 /tmp/deveco-refresh-7306-cfg-resp.json)" >&2
  exit 1
fi

# 同步等待 SYNC_FREQUENCY 后测试
sleep 12
TEST=$(curl -sS -m 90 "http://localhost:3500/api/channel/test/$CHANNEL_ID?model=hw_glm-5.3" -H "$ADMIN")
echo "[deveco-refresh-7306] $(date '+%F %T') key updated (len=${#AT}, yaml synced), test: $TEST"
