#!/bin/bash
# deveco 渠道 25 (DevEco Code_6506) accessToken 自动续期
# 账号身份（2026-10-12 用户确认）：6506 = 大**** = userId 450086000000005386
#   ⚠ 与 9227 是同一账号（9227 时代的 .deveco-9227 归档为旧凭证，勿再用于本渠道；
#     9226/hid843 是另一账号，见 deveco-refresh-9226.sh）。
# 背景：该账号 jwtToken 被华为风控绑定登录环境（仅 Windows+node 客户端可通过 jwToken/check），
#       服务器侧无法直接刷新。accessToken 无绑定（约 24h 有效），经 ssh winbox 在 Windows 端
#       换出后回填渠道 key。troubleshoot.md 问题 18。
# 凭证目录：C:\Users\Administrator\.deveco-6506（独立快照目录，与默认目录互不影响）。
# 就绪状态（2026-10-12）：Windows 端已重新登录（默认目录登录后凭证已快照至 .deveco-6506，
#   userId=450086000000005386 已填入下方 EXPECTED_USER_ID），cron 已恢复。
# 依赖：root 免密 ssh winbox；winbox:Temp/deveco-get-access.js（与 ha/bin/deveco-get-access.js 同步）
# 注意：
#   1. PUT /api/channel/ 的 body 不得含 status 字段（UpdateChannel 校验拒绝），只传 id+key。
#   2. 网关 SaveConfig/config-sync 会用 yaml 的 key 覆盖渠道 key（正向同步），因此新 token
#      必须同时回写网关 yaml（DevEco Code_6506 块），否则下次配置保存会把渠道 key 回写成旧 token。
#   3. 2026-10-11 起传入 expectedUserId 校验：防止目录里被其他账号登录覆盖（当日 9227 登录
#      覆盖过默认目录 token.enc）。2026-10-12 6506 重新登录并快照后 cron 恢复。
set -euo pipefail

ADMIN="Authorization: Bearer 99da79052d7de01cc6ef74b968568bdf"
API="http://localhost:3500/api/channel/"
CFG_API="http://localhost:3500/api/admin/gateway/config"
WIN_SCRIPT="C:/Users/Administrator/AppData/Local/Temp/deveco-get-access.js"
WIN_CONFIG_DIR="C:/Users/Administrator/.deveco-6506"
PROVIDER_NAME="DevEco Code_6506"
CHANNEL_ID=25
# 2026-10-12 用户确认：6506 = 大**** = userId 450086000000005386（见脚本头部说明）
EXPECTED_USER_ID="450086000000005386"

AT=$(ssh -o ConnectTimeout=15 -o BatchMode=yes winbox "node $WIN_SCRIPT $WIN_CONFIG_DIR $EXPECTED_USER_ID" 2>&1 | tail -1 || true)
# 目录未登录/不存在（node 读 token.dek 崩溃）或账号不符时的兜底提示
if [[ -z "$AT" || "$AT" == *ENOENT* || "$AT" == *"no such file"* || "$AT" == Node.js* ]]; then
  echo "[deveco-refresh-6506] 凭证目录 $WIN_CONFIG_DIR 尚未登录（见脚本头部的一次性登录命令）" >&2
  exit 3
fi
if [[ "$AT" == ACCOUNT_MISMATCH* ]]; then
  echo "[deveco-refresh-6506] $AT (token.enc 已被其他账号登录覆盖，请在 Windows 端重新登录 6506 账号)" >&2
  exit 2
fi
if [[ ! "$AT" =~ ^DgEAA ]] || [[ ${#AT} -lt 100 ]]; then
  echo "[deveco-refresh-6506] unexpected output: $AT" >&2
  exit 1
fi

HTTP_CODE=$(curl -sS -o /tmp/deveco-refresh-resp.json -w '%{http_code}' -X PUT "$API" \
  -H "$ADMIN" -H "Content-Type: application/json" \
  -d "{\"id\":$CHANNEL_ID,\"key\":\"$AT\"}")
if [[ "$HTTP_CODE" != "200" ]]; then
  echo "[deveco-refresh-6506] PUT failed HTTP $HTTP_CODE: $(cat /tmp/deveco-refresh-resp.json)" >&2
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
  echo "[deveco-refresh-6506] yaml PUT failed HTTP $HTTP2: $(head -c 300 /tmp/deveco-refresh-cfg-resp.json)" >&2
  exit 1
fi

# 同步等待 SYNC_FREQUENCY 后测试
sleep 12
TEST=$(curl -sS -m 90 "http://localhost:3500/api/channel/test/$CHANNEL_ID?model=hw_glm-5.3" -H "$ADMIN")
echo "[deveco-refresh-6506] $(date '+%F %T') key updated (len=${#AT}, yaml synced), test: $TEST"
