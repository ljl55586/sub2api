#!/usr/bin/env bash

# Shared runtime for the orchestrator and per-session scripts.
# Callers are expected to enable: set -euo pipefail.

soak_runtime_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
export SOAK_SCRIPT_DIR=${SOAK_SCRIPT_DIR:-$(cd -- "$soak_runtime_dir/.." && pwd)}

soak_init_runtime() {
  : "${SOAK_BASE_URL:?set SOAK_BASE_URL before starting the run}"
  : "${SOAK_API_KEY:?set SOAK_API_KEY before starting the run}"
  : "${SOAK_OUTPUT_DIR:?set SOAK_OUTPUT_DIR so all sessions share one run directory}"

  mkdir -p "$SOAK_OUTPUT_DIR"/{control,tmp}
  export SOAK_DEADLINE_EPOCH=${SOAK_DEADLINE_EPOCH:-$(( $(date +%s) + 24 * 60 * 60 ))}
  export SOAK_SUCCESS_TIMES_FILE=${SOAK_SUCCESS_TIMES_FILE:-"$SOAK_OUTPUT_DIR/successful-request-times.txt"}
  export SOAK_RESERVATIONS_FILE=${SOAK_RESERVATIONS_FILE:-"$SOAK_OUTPUT_DIR/inflight-request-reservations.tsv"}
  export SOAK_RUN_LOG=${SOAK_RUN_LOG:-"$SOAK_OUTPUT_DIR/run.log"}
  export SOAK_USAGE_SAMPLES_FILE=${SOAK_USAGE_SAMPLES_FILE:-"$SOAK_OUTPUT_DIR/usage-samples.tsv"}

  if [[ ! -f $SOAK_SUCCESS_TIMES_FILE ]]; then
    : >"$SOAK_SUCCESS_TIMES_FILE"
  fi
  if [[ ! -f $SOAK_RESERVATIONS_FILE ]]; then
    : >"$SOAK_RESERVATIONS_FILE"
  fi
  if [[ ${SOAK_PARALLEL_SESSIONS:-0} == 1 ]] && ! command -v flock >/dev/null 2>&1; then
    echo "flock is required when SOAK_PARALLEL_SESSIONS=1" >&2
    return 69
  fi
}

soak_log() {
  printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" | tee -a "$SOAK_RUN_LOG"
}

soak_random_between() {
  local min=$1
  local max=$2
  if (( max <= min )); then
    printf '%s\n' "$min"
    return
  fi
  printf '%s\n' $((min + RANDOM % (max - min + 1)))
}

soak_check_stop() {
  if [[ -f "$SOAK_OUTPUT_DIR/control/STOP" ]]; then
    soak_log "STOP file detected; ending the run before the next request"
    exit 0
  fi
}

soak_check_deadline() {
  if (( $(date +%s) >= SOAK_DEADLINE_EPOCH )); then
    printf '%s\n' "24h deadline reached" >"$SOAK_OUTPUT_DIR/control/DEADLINE_REACHED"
    : >"$SOAK_OUTPUT_DIR/control/STOP"
    soak_log "24h deadline reached; no further request will be sent"
    exit 0
  fi
}

soak_sleep_with_stop() {
  local remaining=$1
  while (( remaining > 0 )); do
    soak_check_stop
    local chunk=30
    if (( remaining < chunk )); then
      chunk=$remaining
    fi
    sleep "$chunk"
    remaining=$((remaining - chunk))
  done
}

soak_sleep_random() {
  local min=$1
  local max=$2
  if [[ ${SOAK_SKIP_WAITS:-0} == 1 ]]; then
    soak_log "SOAK_SKIP_WAITS=1; skipping planned wait ${min}-${max}s"
    return
  fi
  local seconds
  seconds=$(soak_random_between "$min" "$max")
  soak_log "sleeping ${seconds}s before next request"
  soak_sleep_with_stop "$seconds"
}

soak_number_ge() {
  local actual=$1
  local threshold=$2
  awk -v actual="$actual" -v threshold="$threshold" 'BEGIN { exit !(actual + 0 >= threshold + 0) }'
}

soak_shared_lock() {
  if [[ ${SOAK_PARALLEL_SESSIONS:-0} != 1 ]]; then
    return
  fi
  exec 9>>"$SOAK_OUTPUT_DIR/control/shared-state.lock"
  flock -x 9
}

soak_shared_unlock() {
  if [[ ${SOAK_PARALLEL_SESSIONS:-0} != 1 ]]; then
    return
  fi
  flock -u 9
  exec 9>&-
}

soak_usage_guard_failure() {
  local reason=$1
  local mode=${SOAK_USAGE_GUARD_MODE:-required}
  if [[ $mode == best_effort ]]; then
    soak_log "usage guard unavailable in best_effort mode: $reason"
    return 0
  fi

  printf '%s\n' "$reason" >"$SOAK_OUTPUT_DIR/control/STOPPED_ON_USAGE_GUARD_ERROR"
  : >"$SOAK_OUTPUT_DIR/control/STOP"
  soak_log "usage guard failed closed: $reason"
  return 1
}

soak_record_usage_sample() {
  local five_hour=$1
  local seven_day=$2
  local reset_epoch=$3
  local sampled_at=$4
  local sample_age=$5

  soak_shared_lock
  if [[ ! -f $SOAK_USAGE_SAMPLES_FILE ]]; then
    printf '%s\n' $'observed_at\tfive_hour_percent\tseven_day_percent\tfive_hour_reset_epoch\tupstream_sampled_at\tsample_age_seconds' >"$SOAK_USAGE_SAMPLES_FILE"
  fi
  printf '%s\t%s\t%s\t%s\t%s\t%s\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$five_hour" "$seven_day" \
    "$reset_epoch" "$sampled_at" "$sample_age" >>"$SOAK_USAGE_SAMPLES_FILE"
  soak_shared_unlock
}

soak_missing_usage_action() {
  local marker="$SOAK_OUTPUT_DIR/control/USAGE_BOOTSTRAP_OWNER"
  local now owner owner_since success_count action
  now=$(date +%s)
  action=fail

  soak_shared_lock
  success_count=$(wc -l <"$SOAK_SUCCESS_TIMES_FILE" | tr -d ' ')
  owner=""
  owner_since=0
  if [[ -s $marker ]]; then
    IFS='|' read -r owner owner_since <"$marker"
    owner_since=${owner_since:-0}
  fi

  if (( success_count == 0 )); then
    if [[ -z $owner ]] || ! kill -0 "$owner" 2>/dev/null; then
      printf '%s|%s\n' "$$" "$now" >"$marker"
      owner=$$
      owner_since=$now
    fi
    if [[ $owner == "$$" ]]; then
      action=owner
    else
      action=wait
    fi
  elif [[ -n $owner ]] && (( now - owner_since < ${SOAK_BOOTSTRAP_SAMPLE_WAIT_SECONDS:-60} )); then
    action=wait
  fi
  soak_shared_unlock

  printf '%s\n' "$action"
}

soak_check_usage_limits() {
  local mode=${SOAK_USAGE_GUARD_MODE:-required}
  case "$mode" in
    off) return 0 ;;
    required|best_effort) ;;
    *) soak_usage_guard_failure "invalid SOAK_USAGE_GUARD_MODE=$mode"; return $? ;;
  esac

  local snapshot five_hour seven_day reset_epoch sampled_at sample_age missing_action
  while true; do
    if ! snapshot=$("$SOAK_SCRIPT_DIR/usage_guard.sh" 2>>"$SOAK_RUN_LOG"); then
      soak_usage_guard_failure "could not read account utilization from PostgreSQL"
      return $?
    fi
    IFS='|' read -r five_hour seven_day reset_epoch sampled_at sample_age <<<"$snapshot"

    if [[ -n $five_hour && -n $seven_day ]]; then
      break
    fi
    if [[ ${SOAK_ALLOW_BOOTSTRAP_WITHOUT_USAGE:-1} != 1 ]]; then
      soak_usage_guard_failure "passive 5h/7d utilization is missing and bootstrap is disabled"
      return $?
    fi

    missing_action=$(soak_missing_usage_action)
    case "$missing_action" in
      owner)
        soak_log "usage guard has no passive sample yet; this session owns the single bootstrap request"
        return 0
        ;;
      wait)
        soak_log "usage guard is waiting for the bootstrap request to produce a passive sample"
        soak_sleep_with_stop 5
        soak_check_deadline
        ;;
      *)
        soak_usage_guard_failure "passive 5h/7d utilization is missing after bootstrap"
        return $?
        ;;
    esac
  done

  soak_record_usage_sample "$five_hour" "$seven_day" "$reset_epoch" "$sampled_at" "$sample_age"
  soak_log "usage guard sample: 5h=${five_hour}% 7d=${seven_day}% reset_epoch=${reset_epoch:-unknown} sample_age=${sample_age:-unknown}s"

  local weekly_stop=${SOAK_USAGE_7D_STOP_PERCENT:-70}
  if soak_number_ge "$seven_day" "$weekly_stop"; then
    printf '7d utilization %s%% reached stop threshold %s%%\n' "$seven_day" "$weekly_stop" \
      >"$SOAK_OUTPUT_DIR/control/STOPPED_ON_7D_LIMIT"
    : >"$SOAK_OUTPUT_DIR/control/STOP"
    soak_log "7d utilization reached ${weekly_stop}%; stopping the whole test"
    return 1
  fi

  local five_hour_stop=${SOAK_USAGE_5H_STOP_PERCENT:-78}
  if soak_number_ge "$five_hour" "$five_hour_stop"; then
    if [[ ! $reset_epoch =~ ^[0-9]+$ ]] || (( reset_epoch <= $(date +%s) )); then
      soak_usage_guard_failure "5h limit reached but no future reset time is available"
      return $?
    fi

    local now jitter wait_seconds deadline_remaining
    now=$(date +%s)
    jitter=$(soak_random_between 60 180)
    wait_seconds=$((reset_epoch - now + jitter))
    deadline_remaining=$((SOAK_DEADLINE_EPOCH - now))
    if (( deadline_remaining <= 0 )); then
      soak_check_deadline
    fi
    if (( wait_seconds > deadline_remaining )); then
      wait_seconds=$deadline_remaining
    fi

    printf '5h utilization %s%% reached pause threshold %s%%; reset_epoch=%s\n' \
      "$five_hour" "$five_hour_stop" "$reset_epoch" \
      >"$SOAK_OUTPUT_DIR/control/PAUSED_ON_5H_LIMIT"
    soak_log "5h utilization reached ${five_hour_stop}%; pausing ${wait_seconds}s until reset"
    soak_sleep_with_stop "$wait_seconds"
    soak_check_deadline
    soak_log "5h pause completed; checking utilization again before sending"
    soak_check_usage_limits
    return $?
  fi

  return 0
}

soak_reserve_rolling_slot() {
  local reservation_id=$1
  local limit=${SOAK_MAX_REQUESTS_PER_5H:-18}
  local window_seconds=$((5 * 60 * 60))
  local reservation_stale_seconds=${SOAK_RESERVATION_STALE_SECONDS:-1800}
  while true; do
    soak_check_stop
    soak_check_deadline
    local now cutoff reservation_cutoff success_count reservation_count total_count oldest wait_seconds jitter
    now=$(date +%s)
    cutoff=$((now - window_seconds))
    reservation_cutoff=$((now - reservation_stale_seconds))

    soak_shared_lock
    awk -v cutoff="$cutoff" '$1 > cutoff { print $1 }' "$SOAK_SUCCESS_TIMES_FILE" >"$SOAK_SUCCESS_TIMES_FILE.tmp.$$"
    mv "$SOAK_SUCCESS_TIMES_FILE.tmp.$$" "$SOAK_SUCCESS_TIMES_FILE"
    awk -F '\t' -v cutoff="$reservation_cutoff" '$2 > cutoff { print $0 }' "$SOAK_RESERVATIONS_FILE" >"$SOAK_RESERVATIONS_FILE.tmp.$$"
    mv "$SOAK_RESERVATIONS_FILE.tmp.$$" "$SOAK_RESERVATIONS_FILE"

    success_count=$(wc -l <"$SOAK_SUCCESS_TIMES_FILE" | tr -d ' ')
    reservation_count=$(wc -l <"$SOAK_RESERVATIONS_FILE" | tr -d ' ')
    total_count=$((success_count + reservation_count))
    if (( total_count < limit )); then
      printf '%s\t%s\n' "$reservation_id" "$now" >>"$SOAK_RESERVATIONS_FILE"
      soak_shared_unlock
      return
    fi

    if (( success_count >= limit )); then
      oldest=$(head -n 1 "$SOAK_SUCCESS_TIMES_FILE")
      jitter=$(soak_random_between 60 180)
      wait_seconds=$((oldest + window_seconds - now + jitter))
      if (( wait_seconds < 60 )); then
        wait_seconds=60
      fi
    else
      wait_seconds=5
    fi
    soak_shared_unlock

    soak_log "rolling 5h request slots full (success=$success_count inflight=$reservation_count limit=$limit); waiting ${wait_seconds}s"
    soak_sleep_with_stop "$wait_seconds"
  done
}

soak_release_rolling_slot() {
  local reservation_id=$1
  soak_shared_lock
  awk -F '\t' -v reservation_id="$reservation_id" '$1 != reservation_id { print $0 }' "$SOAK_RESERVATIONS_FILE" >"$SOAK_RESERVATIONS_FILE.tmp.$$"
  mv "$SOAK_RESERVATIONS_FILE.tmp.$$" "$SOAK_RESERVATIONS_FILE"
  soak_shared_unlock
}

soak_complete_rolling_slot() {
  local reservation_id=$1
  soak_shared_lock
  awk -F '\t' -v reservation_id="$reservation_id" '$1 != reservation_id { print $0 }' "$SOAK_RESERVATIONS_FILE" >"$SOAK_RESERVATIONS_FILE.tmp.$$"
  mv "$SOAK_RESERVATIONS_FILE.tmp.$$" "$SOAK_RESERVATIONS_FILE"
  date +%s >>"$SOAK_SUCCESS_TIMES_FILE"
  soak_shared_unlock
}

soak_prompt_file_for_turn() {
  local session=$1
  local initial_prompt=$2
  local turn=$3
  if (( turn == 1 )); then
    printf '%s\n' "$initial_prompt"
    return
  fi

  local index=$((turn - 2))
  local generated="$SOAK_OUTPUT_DIR/tmp/${session}-turn-${turn}.md"
  jq -er \
    --arg session "$session" \
    --argjson index "$index" \
    '.[$session][$index]' \
    "$SOAK_SCRIPT_DIR/prompts/followups.json" >"$generated"
  printf '%s\n' "$generated"
}

soak_assert_next_turn() {
  local session=$1
  local planned_turn=$2
  if [[ ${SOAK_DRY_RUN:-0} == 1 ]]; then
    return 0
  fi

  local state_file="$SOAK_OUTPUT_DIR/state/$session.messages.json"
  local actual_turn=1
  if [[ -f $state_file ]]; then
    actual_turn=$(jq -er '[.[] | select(.role == "user")] | length + 1' "$state_file")
  fi
  if (( planned_turn != actual_turn )); then
    printf 'session=%s planned_turn=%s actual_next_turn=%s\n' "$session" "$planned_turn" "$actual_turn" \
      >"$SOAK_OUTPUT_DIR/control/STOPPED_ON_STATE_MISMATCH"
    : >"$SOAK_OUTPUT_DIR/control/STOP"
    soak_log "session state mismatch: session=$session planned_turn=$planned_turn actual_next_turn=$actual_turn"
    return 1
  fi
}

soak_effective_cache_expectation() {
  local session=$1
  local planned=$2
  if [[ $planned != hit ]]; then
    printf '%s\n' "$planned"
    return
  fi

  local manifest_file="$SOAK_OUTPUT_DIR/manifest.tsv"
  if [[ ! -f $manifest_file ]]; then
    printf '%s\n' "$planned"
    return
  fi

  if [[ ${SOAK_PARALLEL_SESSIONS:-0} == 1 ]]; then
    exec 8>>"$SOAK_OUTPUT_DIR/control/manifest.lock"
    flock -s 8
  fi
  local last_finished
  last_finished=$(awk -F '\t' -v session="$session" 'NR > 1 && $4 == session { value=$3 } END { print value }' "$manifest_file")
  if [[ ${SOAK_PARALLEL_SESSIONS:-0} == 1 ]]; then
    flock -u 8
    exec 8>&-
  fi

  local cache_ttl_seconds=${SOAK_CACHE_TTL_SECONDS:-300}
  if [[ $last_finished =~ ^[0-9]+$ ]] && (( $(date +%s) - last_finished >= cache_ttl_seconds )); then
    printf '%s\n' ttl_miss
    return
  fi
  printf '%s\n' "$planned"
}

soak_send_event() {
  local session=$1
  local initial_prompt=$2
  local turn=$3
  local expectation=$4
  local wait_min=$5
  local wait_max=$6

  if ! soak_assert_next_turn "$session" "$turn"; then
    return 1
  fi
  if ! soak_check_usage_limits; then
    return 1
  fi
  soak_sleep_random "$wait_min" "$wait_max"
  soak_check_stop
  soak_check_deadline
  if ! soak_check_usage_limits; then
    return 1
  fi

  local prompt_file reservation_id effective_expectation
  prompt_file=$(soak_prompt_file_for_turn "$session" "$initial_prompt" "$turn")
  effective_expectation=$(soak_effective_cache_expectation "$session" "$expectation")
  if [[ $effective_expectation != "$expectation" ]]; then
    soak_log "cache expectation adjusted: session=$session turn=$turn planned=$expectation effective=$effective_expectation"
    expectation=$effective_expectation
  fi
  reservation_id="${session}-t${turn}-$$-$(date +%s)"
  soak_reserve_rolling_slot "$reservation_id"
  soak_log "sending session=$session turn=$turn expected_cache=$expectation"
  if ! "$SOAK_SCRIPT_DIR/send_turn.sh" "$session" "$prompt_file" "$expectation" | tee -a "$SOAK_RUN_LOG"; then
    soak_release_rolling_slot "$reservation_id"
    soak_log "request failed; stopping the whole run without automatic retry"
    printf '%s\n' "request failure at $(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$SOAK_OUTPUT_DIR/control/STOPPED_ON_ERROR"
    : >"$SOAK_OUTPUT_DIR/control/STOP"
    return 1
  fi
  if [[ ${SOAK_DRY_RUN:-0} == 1 ]]; then
    soak_release_rolling_slot "$reservation_id"
  else
    soak_complete_rolling_slot "$reservation_id"
  fi
  if ! soak_check_usage_limits; then
    return 1
  fi
}

soak_send_burst() {
  local session=$1
  local initial_prompt=$2
  local first_turn=$3
  local first_expectation=$4
  local gap_min=$5
  local gap_max=$6

  if [[ ! $first_turn =~ ^[1-9][0-9]*$ ]]; then
    echo "first turn must be a positive integer: $first_turn" >&2
    return 64
  fi
  case "$first_expectation" in
    cold|ttl_miss) ;;
    *) echo "first expectation must be cold or ttl_miss: $first_expectation" >&2; return 64 ;;
  esac
  if [[ ! $gap_min =~ ^[0-9]+$ || ! $gap_max =~ ^[0-9]+$ ]] || (( gap_max < gap_min )); then
    echo "invalid burst wait range: $gap_min-$gap_max" >&2
    return 64
  fi

  soak_send_event "$session" "$initial_prompt" "$first_turn" "$first_expectation" "$gap_min" "$gap_max" || return $?
  soak_send_event "$session" "$initial_prompt" $((first_turn + 1)) hit 90 180 || return $?
  soak_send_event "$session" "$initial_prompt" $((first_turn + 2)) hit 90 240 || return $?
}
