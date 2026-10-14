#!/usr/bin/env bash
# ensure-auto-free-height.sh — 幂等恢复 auto-free-height 虚拟模型的渠道注册
#
# 背景（2026-10-12 判例）：auto-free-height 是 llm-gateway 虚拟模型名
# （modelCapabilities.free-height 池 → mimo-v2.6-flash-free）。它必须在
# 渠道 15/17 的 models + model_mapping 与 abilities 表注册，否则 new-api
# distributor 报「分组 default 下模型 auto-free-height 无可用渠道」（503）。
# 渠道 models 会被外部操作（如管理 UI 拉取上游模型列表）重写清掉，本脚本幂等补回。
#
# 用法：直接跑（无参数）；可挂 cron 巡检。
set -u
PSQL="docker exec spk-pg-prod psql -U spk -d new-api -v ON_ERROR_STOP=1"

changed=0

need_model=$($PSQL -t -A -c "SELECT count(*) FROM channels WHERE id IN (15,17) AND models NOT LIKE '%auto-free-height%';")
if [ "$need_model" != "0" ]; then
  $PSQL -c "UPDATE channels SET models = models || ',auto-free-height' WHERE id IN (15,17) AND models NOT LIKE '%auto-free-height%';"
  changed=1
fi

need_map=$($PSQL -t -A -c "SELECT count(*) FROM channels WHERE id IN (15,17) AND (model_mapping::jsonb ->> 'auto-free-height') IS DISTINCT FROM 'mimo-v2.6-flash-free';")
if [ "$need_map" != "0" ]; then
  $PSQL -c "UPDATE channels SET model_mapping = (COALESCE(model_mapping::jsonb,'{}'::jsonb) || '{\"auto-free-height\":\"mimo-v2.6-flash-free\"}'::jsonb)::text WHERE id IN (15,17);"
  changed=1
fi

need_ab=$($PSQL -t -A -c "SELECT count(*) FROM channels c WHERE c.id IN (15,17) AND (SELECT count(*) FROM abilities a WHERE a.model='auto-free-height' AND a.channel_id=c.id) < 3;")
if [ "$need_ab" != "0" ]; then
  $PSQL -c "INSERT INTO abilities (\"group\", model, channel_id, enabled, priority, weight, tag) SELECT g.g, 'auto-free-height', c.id, true, c.priority, 0, c.tag FROM channels c CROSS JOIN (VALUES ('default'),('efficient'),('quality')) AS g(g) WHERE c.id IN (15,17) ON CONFLICT DO NOTHING;"
  changed=1
fi

if [ "$changed" = "1" ]; then
  echo "$(date '+%F %T') auto-free-height 渠道注册已修复（等 10s 内部 sync 生效）"
else
  echo "$(date '+%F %T') auto-free-height 渠道注册完好，无需修复"
fi
