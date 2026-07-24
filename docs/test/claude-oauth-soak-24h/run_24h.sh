#!/usr/bin/env bash
set -euo pipefail

umask 077

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
run_stamp=$(date -u +%Y%m%dT%H%M%SZ)
export SOAK_SCRIPT_DIR=$script_dir
export SOAK_OUTPUT_DIR=${SOAK_OUTPUT_DIR:-"$script_dir/runs/$run_stamp"}
export SOAK_PARALLEL_SESSIONS=0
export SOAK_CONFIRM_BEFORE_SEND=1
export SOAK_CONFIRM_TIMEOUT_SECONDS=${SOAK_CONFIRM_TIMEOUT_SECONDS:-0}

planned_session_count=1
planned_turns_per_session=30
planned_wave_count=10
planned_request_count=$((planned_session_count * planned_turns_per_session))

: "${SOAK_BASE_URL:?set SOAK_BASE_URL before starting the run}"
: "${SOAK_API_KEY:?set SOAK_API_KEY before starting the run}"

if [[ -d $SOAK_OUTPUT_DIR ]] && [[ -n $(find "$SOAK_OUTPUT_DIR" -mindepth 1 -maxdepth 1 -print -quit) ]]; then
  echo "SOAK_OUTPUT_DIR is not empty; choose a new directory: $SOAK_OUTPUT_DIR" >&2
  exit 73
fi

mkdir -p "$SOAK_OUTPUT_DIR"/{control,tmp}
start_epoch=$(date +%s)
export SOAK_DEADLINE_EPOCH=$((start_epoch + 24 * 60 * 60))
export SOAK_SUCCESS_TIMES_FILE="$SOAK_OUTPUT_DIR/successful-request-times.txt"
export SOAK_RESERVATIONS_FILE="$SOAK_OUTPUT_DIR/inflight-request-reservations.tsv"
export SOAK_RUN_LOG="$SOAK_OUTPUT_DIR/run.log"
export SOAK_USAGE_SAMPLES_FILE="$SOAK_OUTPUT_DIR/usage-samples.tsv"
printf 'start_epoch=%s\ndeadline_epoch=%s\nparallel_sessions=%s\nconfirmation_required=1\nconfirmation_timeout_seconds=%s\nscheduling_mode=single_session_confirmed_waves\n' \
  "$start_epoch" "$SOAK_DEADLINE_EPOCH" "$SOAK_PARALLEL_SESSIONS" \
  "$SOAK_CONFIRM_TIMEOUT_SECONDS" >"$SOAK_OUTPUT_DIR/run.meta"
: >"$SOAK_SUCCESS_TIMES_FILE"
: >"$SOAK_RESERVATIONS_FILE"

source "$script_dir/lib/runtime.sh"
soak_init_runtime

soak_validate_schedule() {
  local wave session first_turn expectation gap_min gap_max session_script followup_count expected_turn seen_count
  local last_wave=0
  local row_count=0
  local wave_count=0
  local session_count=0
  local schedule_state_file="$SOAK_OUTPUT_DIR/tmp/schedule-next-turns.tsv"
  local schedule_seen_file="$SOAK_OUTPUT_DIR/tmp/schedule-wave-sessions.tsv"
  : >"$schedule_state_file"
  : >"$schedule_seen_file"

  while IFS=$'\t' read -r wave session first_turn expectation gap_min gap_max; do
    [[ -z $wave || $wave == \#* ]] && continue
    if [[ ! $wave =~ ^[1-9][0-9]*$ ]] || (( wave < last_wave || wave > last_wave + 1 )); then
      echo "schedule.tsv has an invalid, skipped, or out-of-order wave: $wave" >&2
      return 1
    fi
    if [[ ! $session =~ ^[A-Za-z0-9._-]+$ ]]; then
      echo "schedule.tsv has an invalid session name: $session" >&2
      return 1
    fi
    if [[ ! $first_turn =~ ^[1-9][0-9]*$ || ! $gap_min =~ ^[0-9]+$ || ! $gap_max =~ ^[0-9]+$ ]] || (( gap_max < gap_min )); then
      echo "schedule.tsv has invalid turn/wait values for $session" >&2
      return 1
    fi
    case "$expectation" in
      cold|hit|ttl_miss) ;;
      *) echo "schedule.tsv has invalid expectation for $session: $expectation" >&2; return 1 ;;
    esac
    if (( first_turn == 1 )) && [[ $expectation != cold ]]; then
      echo "the first burst for $session must start cold" >&2
      return 1
    fi
    if (( first_turn > 1 )) && [[ $expectation == cold ]]; then
      echo "only turn 1 may use a cold expectation: $session turn $first_turn" >&2
      return 1
    fi

    session_script="$script_dir/sessions/$session.sh"
    if [[ ! -x $session_script ]]; then
      echo "session script is missing or not executable: $session_script" >&2
      return 1
    fi

    seen_count=$(awk -F '\t' -v wave="$wave" -v session="$session" '$1 == wave && $2 == session { count++ } END { print count + 0 }' "$schedule_seen_file")
    if (( seen_count > 0 )); then
      echo "schedule.tsv runs $session more than once in wave $wave" >&2
      return 1
    fi
    printf '%s\t%s\t%s\t%s\n' "$wave" "$session" "$gap_min" "$gap_max" >>"$schedule_seen_file"
    if ! awk -F '\t' -v wave="$wave" -v gap_min="$gap_min" -v gap_max="$gap_max" \
      '$1 == wave && ($3 != gap_min || $4 != gap_max) { bad=1 } END { exit bad }' "$schedule_seen_file"; then
      echo "all rows in wave $wave must use the same gap range" >&2
      return 1
    fi

    expected_turn=$(awk -F '\t' -v session="$session" '$1 == session { value = $2 } END { print value }' "$schedule_state_file")
    expected_turn=${expected_turn:-1}
    if (( first_turn != expected_turn )); then
      echo "schedule.tsv expected ${session} turn ${expected_turn}, got $first_turn" >&2
      return 1
    fi
    awk -F '\t' -v session="$session" '$1 != session { print $0 }' "$schedule_state_file" >"$schedule_state_file.tmp"
    printf '%s\t%s\n' "$session" "$((first_turn + 3))" >>"$schedule_state_file.tmp"
    mv "$schedule_state_file.tmp" "$schedule_state_file"

    followup_count=$(jq -er --arg session "$session" '.[$session] | length' "$script_dir/prompts/followups.json")
    if (( followup_count < first_turn + 1 )); then
      echo "not enough followups for $session burst ending at turn $((first_turn + 2))" >&2
      return 1
    fi

    if (( wave > last_wave )); then
      last_wave=$wave
      wave_count=$((wave_count + 1))
    fi
    row_count=$((row_count + 1))
  done <"$script_dir/schedule.tsv"

  if (( row_count == 0 )); then
    echo "schedule.tsv has no runnable rows" >&2
    return 1
  fi
  if ! awk -F '\t' -v expected="$planned_session_count" \
    '{ count[$1]++ } END { for (wave in count) if (count[wave] != expected) exit 1 }' \
    "$schedule_seen_file"; then
    echo "every wave must contain exactly $planned_session_count sessions" >&2
    return 1
  fi
  session_count=$(wc -l <"$schedule_state_file" | tr -d ' ')
  if (( session_count != planned_session_count )); then
    echo "schedule.tsv must contain exactly $planned_session_count sessions, got $session_count" >&2
    return 1
  fi
  if ! awk -F '\t' -v next_turn="$((planned_turns_per_session + 1))" \
    '$2 != next_turn { exit 1 }' "$schedule_state_file"; then
    echo "each scheduled session must contain exactly $planned_turns_per_session turns" >&2
    return 1
  fi
  if (( wave_count != planned_wave_count || row_count != planned_wave_count * planned_session_count )); then
    echo "schedule.tsv must define $planned_wave_count waves and $((planned_wave_count * planned_session_count)) bursts, got waves=$wave_count rows=$row_count" >&2
    return 1
  fi

  soak_log "schedule validated: $planned_session_count session x $planned_turns_per_session turns in $planned_wave_count confirmed waves"
}

if ! soak_validate_schedule; then
  printf '%s\n' "schedule validation failed" >"$SOAK_OUTPUT_DIR/control/STOPPED_ON_INVALID_SCHEDULE"
  : >"$SOAK_OUTPUT_DIR/control/STOP"
  exit 1
fi

scheduled_sessions=$(awk -F '\t' '$1 !~ /^#/ && $2 != "" && !seen[$2]++ { print $2 }' "$script_dir/schedule.tsv")
if [[ -z $scheduled_sessions ]]; then
  echo "schedule.tsv has no sessions for identity initialization" >&2
  exit 1
fi
# Session names are restricted to [A-Za-z0-9._-]+ by soak_validate_schedule.
# shellcheck disable=SC2086
"$script_dir/init_session_identities.sh" $scheduled_sessions
export SOAK_SESSION_IDENTITIES_FILE=${SOAK_SESSION_IDENTITIES_FILE:-"$SOAK_OUTPUT_DIR/session-identities.json"}
printf 'session_identities_file=%s\nplanned_sessions=%s\nplanned_requests=%s\n' \
  "$SOAK_SESSION_IDENTITIES_FILE" "$planned_session_count" "$planned_request_count" \
  >>"$SOAK_OUTPUT_DIR/run.meta"

if [[ ${SOAK_VALIDATE_ONLY:-0} == 1 ]]; then
  soak_log "SOAK_VALIDATE_ONLY=1; schedule and identities validated without sending requests"
  exit 0
fi

soak_run_confirmed_waves() {
  local target_wave wave session first_turn expectation gap_min gap_max session_script rc
  local wave_gap_min wave_gap_max wave_failed
  local waves

  soak_log "starting one-session confirmed waves; every request waits for SEND REQUEST"
  waves=$(awk -F '\t' '$1 !~ /^#/ && !seen[$1]++ { print $1 }' "$script_dir/schedule.tsv")
  for target_wave in $waves; do
    if [[ -s $SOAK_SUCCESS_TIMES_FILE ]]; then
      if ! soak_check_usage_limits; then
        return 0
      fi
    fi

    wave_gap_min=$(awk -F '\t' -v target_wave="$target_wave" '$1 == target_wave { print $5; exit }' "$script_dir/schedule.tsv")
    wave_gap_max=$(awk -F '\t' -v target_wave="$target_wave" '$1 == target_wave { print $6; exit }' "$script_dir/schedule.tsv")
    soak_log "wave=$target_wave/$planned_wave_count waiting ${wave_gap_min}-${wave_gap_max}s before the session burst"
    soak_sleep_random "$wave_gap_min" "$wave_gap_max"
    soak_check_stop
    soak_check_deadline
    if [[ -s $SOAK_SUCCESS_TIMES_FILE ]]; then
      if ! soak_check_usage_limits; then
        return 0
      fi
    fi

    wave_failed=0
    exec 6<"$script_dir/schedule.tsv"
    while IFS=$'\t' read -r wave session first_turn expectation gap_min gap_max <&6; do
      [[ -z $wave || $wave == \#* ]] && continue
      [[ $wave == "$target_wave" ]] || continue

      session_script="$script_dir/sessions/$session.sh"
      soak_log "launching session=$session burst=${first_turn}-$((first_turn + 2)) wave=$wave"
      set +e
      "$session_script" "$first_turn" "$expectation" 0 0
      rc=$?
      set -e
      if (( rc != 0 )); then
        wave_failed=1
        soak_log "session failed: wave=$target_wave session=$session exit=$rc"
      else
        soak_log "session completed: wave=$target_wave session=$session"
      fi
    done
    exec 6<&-

    if (( wave_failed != 0 )); then
      printf 'wave=%s\n' "$target_wave" >"$SOAK_OUTPUT_DIR/control/STOPPED_ON_SESSION_ERROR"
      : >"$SOAK_OUTPUT_DIR/control/STOP"
      return 1
    fi
    if [[ -f "$SOAK_OUTPUT_DIR/control/STOP" ]]; then
      soak_log "wave stopped by a safety guard"
      return 0
    fi
    soak_log "wave=$target_wave/$planned_wave_count completed"
  done
}

soak_run_confirmed_waves
soak_check_stop
soak_check_deadline
soak_log "all $planned_request_count planned requests completed; the run ends even if the 5h threshold was not reached"
