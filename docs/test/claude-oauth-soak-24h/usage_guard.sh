#!/usr/bin/env bash
set -euo pipefail
umask 077

: "${SOAK_ACCOUNT_ID:?set SOAK_ACCOUNT_ID to the Claude account database ID}"
if [[ ! $SOAK_ACCOUNT_ID =~ ^[1-9][0-9]*$ ]]; then
  echo "SOAK_ACCOUNT_ID must be a positive integer" >&2
  exit 64
fi

deploy_dir=${SOAK_DEPLOY_DIR:-/root/sub2api-deploy}
if [[ ! -f "$deploy_dir/docker-compose.yml" && ! -f "$deploy_dir/compose.yml" && ! -f "$deploy_dir/compose.yaml" ]]; then
  echo "Docker Compose deployment not found: $deploy_dir" >&2
  exit 66
fi

query="
SELECT
  CASE
    WHEN session_window_end IS NOT NULL AND session_window_end <= NOW() THEN '0'
    ELSE COALESCE(extra->>'session_window_utilization', '')
  END,
  CASE
    WHEN COALESCE(extra->>'passive_usage_7d_reset', '') ~ '^[0-9]+$'
      AND to_timestamp((extra->>'passive_usage_7d_reset')::double precision) <= NOW() THEN '0'
    ELSE COALESCE(extra->>'passive_usage_7d_utilization', '')
  END,
  COALESCE(EXTRACT(EPOCH FROM session_window_end)::bigint::text, ''),
  COALESCE(extra->>'passive_usage_sampled_at', ''),
  CASE
    WHEN COALESCE(extra->>'passive_usage_sampled_at', '') <> ''
      THEN GREATEST(0, EXTRACT(EPOCH FROM (NOW() - (extra->>'passive_usage_sampled_at')::timestamptz))::bigint)::text
    ELSE ''
  END
FROM accounts
WHERE id = $SOAK_ACCOUNT_ID AND deleted_at IS NULL;
"

snapshot=$(
  cd -- "$deploy_dir"
  docker compose exec -T postgres sh -c \
    'psql -X -A -t -F "|" -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "$1"' \
    sh "$query" </dev/null
)
snapshot=${snapshot//$'\r'/}
snapshot=${snapshot%$'\n'}

if [[ -z $snapshot ]]; then
  echo "account not found or utilization query returned no row: $SOAK_ACCOUNT_ID" >&2
  exit 1
fi

IFS='|' read -r five_hour_raw seven_day_raw reset_epoch sampled_at sample_age <<<"$snapshot"
for value_name in five_hour_raw seven_day_raw; do
  value=${!value_name}
  if [[ -n $value && ! $value =~ ^[0-9]+([.][0-9]+)?$ ]]; then
    echo "invalid utilization value from database: $value" >&2
    exit 1
  fi
done

five_hour_percent=""
seven_day_percent=""
if [[ -n $five_hour_raw ]]; then
  five_hour_percent=$(awk -v value="$five_hour_raw" 'BEGIN { printf "%.4f", value * 100 }')
fi
if [[ -n $seven_day_raw ]]; then
  seven_day_percent=$(awk -v value="$seven_day_raw" 'BEGIN { printf "%.4f", value * 100 }')
fi

printf '%s|%s|%s|%s|%s\n' \
  "$five_hour_percent" "$seven_day_percent" "$reset_epoch" "$sampled_at" "$sample_age"
