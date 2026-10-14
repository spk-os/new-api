#!/bin/bash
# deveco 渠道 26 (DevEco Code_9226) accessToken 自动续期（账号 userId=20086000973714746, hid843****）
# ⚠ 账号勘误（2026-10-11 终版）：账号映射以用户确认为准——
#   hid843**** (userId=20086000973714746) = 9226 → 渠道 26（本脚本）
#   大****    (userId=450086000000005386) = 9227 → 凭证归档 C:\Users\Administrator\.deveco-9227
# 背景：该账号 jwtToken 被华为风控绑定登录环境（仅 Windows+node 客户端可通过 jwToken/check），
#       服务器侧无法直接刷新。accessToken 无绑定（约 24h 有效），经 ssh winbox 在 Windows 端
#       换出后回填渠道 key。troubleshoot.md 问题 18。
# 凭证目录：C:\Users\Administrator\.deveco-9226（独立快照目录，与默认目录/6506 互不影响——
#       deveco CLI 支持 DEVECO_CONFIG_DIR，多账号共存无需切换登录）。
# 依赖：root 免密 ssh winbox；winbox:Temp/deveco-get-access.js（与 ha/bin/deveco-get-access.js 同步）
# 注意：
#   1. PUT /api/channel/ 的 body 不得含 status 字段（UpdateChannel 校验拒绝），只传 id+key。
#   2. 网关 SaveConfig/config-sync 会用 yaml 的 key 覆盖渠道 key（正向同步），因此新 token
#      必须同时回写网关 yaml（DevEco Code_9226 块），否则下次配置保存会把渠道 key 回写成旧 token。
set -euo pipefail

ADMIN="Authorization: Bearer 99da79052d7de01cc6ef74b968568bdf"
API="http://localhost:3500/api/channel/"
CFG_API="http://localhost:3500/api/admin/gateway/config"
WIN_SCRIPT="C:/Users/Administrator/AppData/Local/Temp/deveco-get-access.js"
WIN_CONFIG_DIR="C:/Users/Administrator/.deveco-9226"
PROVIDER_NAME="DevEco Code_9226"
CHANNEL_ID=26
EXPECTED_USER_ID=20086000973714746

AT=$(ssh -o ConnectTimeout=15 -o BatchMode=yes winbox "node $WIN_SCRIPT $WIN_CONFIG_DIR $EXPECTED_USER_ID" 2>&1 | tail -1 || true)
if [[ "$AT" == ACCOUNT_MISMATCH* ]]; then
  echo "[deveco-refresh-9226] $AT (token.enc 已被其他账号登录覆盖，请检查 Windows 端登录)" >&2
  exit 2
fi
if [[ ! "$AT" =~ ^DgEAA ]] || [[ ${#AT} -lt 100 ]]; then
  echo "[deveco-refresh-9226] unexpected output: $AT" >&2
  exit 1
fi

HTTP_CODE=$(curl -sS -o /tmp/deveco-refresh-resp.json -w '%{http_code}' -X PUT "$API" \
  -H "$ADMIN" -H "Content-Type: application/json" \
  -d "{\"id\":$CHANNEL_ID,\"key\":\"$AT\"}")
if [[ "$HTTP_CODE" != "200" ]]; then
  echo "[deveco-refresh-9226] PUT failed HTTP $HTTP_CODE: $(cat /tmp/deveco-refresh-resp.json)" >&2
  exit 1
fi

# 回写网关 yaml（防止下次 config 保存用旧 key 覆盖渠道）
curl -sS "$CFG_API" -H "$ADMIN" -o /tmp/deveco-refresh-cfg.json
NEW_YAML=$(PROVIDER="$PROVIDER_NAME" NEWKEY="$AT" python3 - << 'PYEOF'
import json, os, re
cfg = json.load(open('/tmp/deveco-refresh-cfg.json'))
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
HTTP2=$(curl -sS -o /tmp/deveco-refresh-cfg-resp.json -w '%{http_code}' -X PUT "$CFG_API" \
  -H "$ADMIN" -H "Content-Type: application/json" -d "$NEW_YAML")
if [[ "$HTTP2" != "200" ]]; then
  echo "[deveco-refresh-9226] yaml PUT failed HTTP $HTTP2: $(head -c 300 /tmp/deveco-refresh-cfg-resp.json)" >&2
  exit 1
fi

# 同步等待 SYNC_FREQUENCY 后测试
sleep 12
TEST=$(curl -sS -m 90 "http://localhost:3500/api/channel/test/$CHANNEL_ID?model=hw_glm-5.3" -H "$ADMIN")
echo "[deveco-refresh-9226] $(date '+%F %T') key updated (len=${#AT}, yaml synced), test: $TEST"
