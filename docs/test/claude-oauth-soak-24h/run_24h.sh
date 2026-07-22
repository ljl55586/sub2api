#!/usr/bin/env bash
set -euo pipefail

umask 077

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
run_stamp=$(date -u +%Y%m%dT%H%M%SZ)
export SOAK_SCRIPT_DIR=$script_dir
export SOAK_OUTPUT_DIR=${SOAK_OUTPUT_DIR:-"$script_dir/runs/$run_stamp"}
export SOAK_PARALLEL_SESSIONS=${SOAK_PARALLEL_SESSIONS:-1}

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
printf 'start_epoch=%s\ndeadline_epoch=%s\nparallel_sessions=%s\n' \
  "$start_epoch" "$SOAK_DEADLINE_EPOCH" "$SOAK_PARALLEL_SESSIONS" >"$SOAK_OUTPUT_DIR/run.meta"
: >"$SOAK_SUCCESS_TIMES_FILE"
: >"$SOAK_RESERVATIONS_FILE"

source "$script_dir/lib/runtime.sh"
soak_init_runtime

soak_validate_schedule() {
  local phase wave session first_turn expectation gap_min gap_max session_script followup_count expected_turn seen_count wave_key
  local last_phase=0
  local last_wave=0
  local row_count=0
  local schedule_state_file="$SOAK_OUTPUT_DIR/tmp/schedule-next-turns.tsv"
  local schedule_seen_file="$SOAK_OUTPUT_DIR/tmp/schedule-wave-sessions.tsv"
  : >"$schedule_state_file"
  : >"$schedule_seen_file"

  while IFS=$'\t' read -r phase wave session first_turn expectation gap_min gap_max; do
    [[ -z $phase || $phase == \#* ]] && continue
    if [[ ! $phase =~ ^[1-5]$ ]] || (( phase < last_phase )); then
      echo "schedule.tsv has an invalid or out-of-order phase: $phase" >&2
      return 1
    fi
    if (( phase != last_phase )); then
      last_wave=0
    fi
    if [[ ! $wave =~ ^[1-9][0-9]*$ ]] || (( wave < last_wave )); then
      echo "schedule.tsv has an invalid or out-of-order wave in phase $phase: $wave" >&2
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
      cold|ttl_miss) ;;
      *) echo "schedule.tsv has invalid expectation for $session: $expectation" >&2; return 1 ;;
    esac

    session_script="$script_dir/sessions/$session.sh"
    if [[ ! -x $session_script ]]; then
      echo "session script is missing or not executable: $session_script" >&2
      return 1
    fi

    wave_key="${phase}-${wave}"
    seen_count=$(awk -F '\t' -v wave_key="$wave_key" -v session="$session" '$1 == wave_key && $2 == session { count++ } END { print count + 0 }' "$schedule_seen_file")
    if (( seen_count > 0 )); then
      echo "schedule.tsv runs $session more than once in parallel wave $wave_key" >&2
      return 1
    fi
    printf '%s\t%s\t%s\t%s\n' "$wave_key" "$session" "$gap_min" "$gap_max" >>"$schedule_seen_file"
    if ! awk -F '\t' -v wave_key="$wave_key" -v gap_min="$gap_min" -v gap_max="$gap_max" \
      '$1 == wave_key && ($3 != gap_min || $4 != gap_max) { bad=1 } END { exit bad }' "$schedule_seen_file"; then
      echo "all rows in parallel wave $wave_key must use the same gap range" >&2
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

    last_phase=$phase
    last_wave=$wave
    row_count=$((row_count + 1))
  done <"$script_dir/schedule.tsv"

  if (( row_count == 0 )); then
    echo "schedule.tsv has no runnable rows" >&2
    return 1
  fi
  soak_log "schedule validated: $row_count burst rows grouped into parallel waves"
}

if ! soak_validate_schedule; then
  printf '%s\n' "schedule validation failed" >"$SOAK_OUTPUT_DIR/control/STOPPED_ON_INVALID_SCHEDULE"
  : >"$SOAK_OUTPUT_DIR/control/STOP"
  exit 1
fi

scheduled_sessions=$(awk -F '\t' '$1 !~ /^#/ && $3 != "" && !seen[$3]++ { print $3 }' "$script_dir/schedule.tsv")
if [[ -z $scheduled_sessions ]]; then
  echo "schedule.tsv has no sessions for identity initialization" >&2
  exit 1
fi
# Session names are restricted to [A-Za-z0-9._-]+ by soak_validate_schedule.
# shellcheck disable=SC2086
"$script_dir/init_session_identities.sh" $scheduled_sessions
export SOAK_SESSION_IDENTITIES_FILE=${SOAK_SESSION_IDENTITIES_FILE:-"$SOAK_OUTPUT_DIR/session-identities.json"}
printf 'session_identities_file=%s\n' "$SOAK_SESSION_IDENTITIES_FILE" >>"$SOAK_OUTPUT_DIR/run.meta"

if [[ ${SOAK_VALIDATE_ONLY:-0} == 1 ]]; then
  soak_log "SOAK_VALIDATE_ONLY=1; schedule validation completed without sending requests"
  exit 0
fi

soak_sleep_until() {
  local target=$1
  if [[ ${SOAK_TEST_MODE:-0} == 1 ]]; then
    soak_log "SOAK_TEST_MODE=1; skipping phase clock wait"
    return
  fi
  local now
  now=$(date +%s)
  if (( target > now )); then
    soak_log "phase idle until epoch=$target"
    soak_sleep_with_stop $((target - now))
  fi
}

soak_await_checkpoint() {
  local next_phase=$1
  if [[ ${SOAK_TEST_MODE:-0} == 1 ]]; then
    soak_log "SOAK_TEST_MODE=1; skipping checkpoint for phase $next_phase"
    return
  fi
  if [[ ${SOAK_AUTO_CONTINUE:-0} == 1 ]]; then
    soak_log "automatic continuation enabled for phase $next_phase"
    return
  fi

  local marker="$SOAK_OUTPUT_DIR/control/continue-phase-$next_phase"
  soak_log "checkpoint: inspect account 5h/7d usage and local window cost"
  soak_log "continue with: touch $marker"
  while [[ ! -f $marker ]]; do
    soak_check_stop
    soak_check_deadline
    sleep 30
  done
  soak_log "checkpoint approved for phase $next_phase"
}

soak_run_phase() {
  local target_phase=$1
  local phase wave session first_turn expectation gap_min gap_max session_script rc pid
  local target_wave wave_gap_min wave_gap_max wave_failed wave_processes_file
  local start_skew_max=${SOAK_PARALLEL_START_SKEW_MAX_SECONDS:-20}
  local burst_count=0
  local waves

  soak_log "phase $target_phase/5: starting parallel session waves"
  waves=$(awk -F '\t' -v target_phase="$target_phase" \
    '$1 == target_phase && !seen[$2]++ { print $2 }' "$script_dir/schedule.tsv")
  for target_wave in $waves; do
    wave_gap_min=$(awk -F '\t' -v target_phase="$target_phase" -v target_wave="$target_wave" \
      '$1 == target_phase && $2 == target_wave { print $6; exit }' "$script_dir/schedule.tsv")
    wave_gap_max=$(awk -F '\t' -v target_phase="$target_phase" -v target_wave="$target_wave" \
      '$1 == target_phase && $2 == target_wave { print $7; exit }' "$script_dir/schedule.tsv")
    soak_log "phase=$target_phase wave=$target_wave waiting once before parallel launch"
    soak_sleep_random "$wave_gap_min" "$wave_gap_max"
    soak_check_stop
    soak_check_deadline

    wave_processes_file="$SOAK_OUTPUT_DIR/tmp/phase-${target_phase}-wave-${target_wave}-processes.tsv"
    : >"$wave_processes_file"
    while IFS=$'\t' read -r phase wave session first_turn expectation gap_min gap_max; do
      [[ -z $phase || $phase == \#* ]] && continue
      [[ $phase == "$target_phase" && $wave == "$target_wave" ]] || continue

      session_script="$script_dir/sessions/$session.sh"
      soak_log "launching parallel session=$session burst=${first_turn}-$((first_turn + 2)) phase=$phase wave=$wave"
      "$session_script" "$first_turn" "$expectation" 0 "$start_skew_max" &
      pid=$!
      printf '%s\t%s\n' "$pid" "$session" >>"$wave_processes_file"
      burst_count=$((burst_count + 1))
    done <"$script_dir/schedule.tsv"

    wave_failed=0
    while IFS=$'\t' read -r pid session; do
      set +e
      wait "$pid"
      rc=$?
      set -e
      if (( rc != 0 )); then
        wave_failed=1
        soak_log "parallel session failed: phase=$target_phase wave=$target_wave session=$session exit=$rc"
      else
        soak_log "parallel session completed: phase=$target_phase wave=$target_wave session=$session"
      fi
    done <"$wave_processes_file"

    if [[ -f "$SOAK_OUTPUT_DIR/control/STOP" ]]; then
      soak_log "parallel wave stopped by a safety guard"
      return 0
    fi
    if (( wave_failed != 0 )); then
      printf 'phase=%s wave=%s\n' "$target_phase" "$target_wave" >"$SOAK_OUTPUT_DIR/control/STOPPED_ON_SESSION_ERROR"
      : >"$SOAK_OUTPUT_DIR/control/STOP"
      return 1
    fi
  done

  if (( burst_count == 0 )); then
    soak_log "phase $target_phase has no entries in schedule.tsv"
    printf '%s\n' "$target_phase" >"$SOAK_OUTPUT_DIR/control/STOPPED_ON_INVALID_SCHEDULE"
    : >"$SOAK_OUTPUT_DIR/control/STOP"
    return 1
  fi
  soak_log "phase $target_phase/5 completed: $burst_count bursts across parallel waves"
}

for phase_number in 1 2 3 4 5; do
  soak_run_phase "$phase_number"
  soak_check_stop

  if (( phase_number < 5 )); then
    soak_sleep_until $((start_epoch + phase_number * 5 * 60 * 60))
    soak_await_checkpoint $((phase_number + 1))
  fi
done

soak_sleep_until "$SOAK_DEADLINE_EPOCH"
soak_log "24h soak period completed successfully"
