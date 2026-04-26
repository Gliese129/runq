#!/usr/bin/env bash

set -euo pipefail
export PATH="$PATH:$HOME/bin"

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)

export RUNQ_E2E_TIMEOUT=${RUNQ_E2E_TIMEOUT:-45m}
export RUNQ_COMMAND_TIMEOUT=${RUNQ_COMMAND_TIMEOUT:-60s}
export RUNQ_E2E_RUN_ID=${RUNQ_E2E_RUN_ID:-$(date '+%Y%m%d-%H%M%S')}
export RUNQ_E2E_BASE=${RUNQ_E2E_BASE:-"$REPO_ROOT/.runq-e2e"}
export RUNQ_E2E_ROOT=${RUNQ_E2E_ROOT:-"$RUNQ_E2E_BASE/$RUNQ_E2E_RUN_ID"}
export RUNQ_DATA_DIR=${RUNQ_DATA_DIR:-"$RUNQ_E2E_ROOT/data"}
export RUNQ_SOCKET=${RUNQ_SOCKET:-"$RUNQ_DATA_DIR/runq.sock"}
export RUNQ_BIN=${RUNQ_BIN:-"$RUNQ_E2E_ROOT/runq"}
export RUNQ_PROJECT_DIR=${RUNQ_PROJECT_DIR:-"$RUNQ_E2E_ROOT/project"}
export RUNQ_DEFAULT_DATA_DIR=${RUNQ_DEFAULT_DATA_DIR:-"$HOME/.local/share/runq"}

if [[ "${RUNQ_E2E_TIMEOUT}" != "0" && "${RUNQ_TIMEOUT_WRAPPED:-0}" != "1" ]] && command -v timeout >/dev/null 2>&1; then
  export RUNQ_TIMEOUT_WRAPPED=1
  exec timeout --preserve-status --kill-after=30s "${RUNQ_E2E_TIMEOUT}" bash "$0" "$@"
fi

LOG_DIR="$RUNQ_E2E_ROOT/case-logs"
REPORT="$RUNQ_E2E_ROOT/report.md"
RUNQ_CMD=()
SUMMARY_ROWS=()
FAILURES=()
JOB1=""
JOBF=""

log() {
  printf '\n[%s] %s\n' "$(date '+%H:%M:%S')" "$*"
}

record() {
  local case_id=$1
  local result=$2
  local notes=$3

  SUMMARY_ROWS+=("| ${case_id} | ${result} | ${notes} |")
  printf '[%s] %s - %s\n' "${result}" "${case_id}" "${notes}"
}

fail_case() {
  local case_id=$1
  local notes=$2

  record "${case_id}" "FAIL" "${notes}"
  FAILURES+=("### ${case_id}

${notes}
")
}

run_log() {
  local name=$1
  shift

  mkdir -p "${LOG_DIR}"
  {
    printf '$'
    printf ' %q' "$@"
    printf '\n'
    timeout --kill-after=5s "${RUNQ_COMMAND_TIMEOUT}" "$@"
  } >"${LOG_DIR}/${name}.log" 2>&1
}

capture() {
  local name=$1
  shift

  mkdir -p "${LOG_DIR}"
  {
    printf '$'
    printf ' %q' "$@"
    printf '\n'
  } >"${LOG_DIR}/${name}.log"
  timeout --kill-after=5s "${RUNQ_COMMAND_TIMEOUT}" "$@" 2>&1 | tee -a "${LOG_DIR}/${name}.log"
}

contains() {
  grep -Eiq "$2" <<<"$1"
}

extract_id_from_text() {
  grep -Eo 'id=[^ ]+' | head -n 1 | cut -d= -f2
}

sqlite_value() {
  sqlite3 "${RUNQ_DATA_DIR}/runq.db" "$1"
}

sql_ids() {
  local first=1
  local id

  for id in "$@"; do
    if [[ ${first} -eq 0 ]]; then
      printf ','
    fi
    printf "'%s'" "${id//\'/\'\'}"
    first=0
  done
}

wait_sql_match() {
  local name=$1
  local timeout_seconds=$2
  local query=$3
  local pattern=$4
  local start output

  start=$(date +%s)
  mkdir -p "${LOG_DIR}"
  while true; do
    output=$(sqlite_value "${query}" 2>&1 || true)
    {
      printf 'query: %s\n' "${query}"
      printf 'output: %s\n' "${output}"
    } >"${LOG_DIR}/${name}.log"
    if grep -Eiq "${pattern}" <<<"${output}"; then
      return 0
    fi
    if (( "$(date +%s)" - start >= timeout_seconds )); then
      return 1
    fi
    sleep 1
  done
}

wait_job_successes() {
  local name=$1
  local job_id=$2
  local expected=$3
  local timeout_seconds=$4

  wait_sql_match "${name}" "${timeout_seconds}" \
    "select count(*) from tasks where job_id='${job_id}' and status='success';" \
    "^${expected}$"
}

wait_job_terminal() {
  local name=$1
  local job_id=$2
  local expected_tasks=$3
  local timeout_seconds=$4

  wait_sql_match "${name}" "${timeout_seconds}" \
    "select count(*) from tasks where job_id='${job_id}' and status in ('success','failed','killed');" \
    "^${expected_tasks}$"
}

wait_no_active_tasks() {
  wait_sql_match "$1" "$2" \
    "select count(*) from tasks where status in ('running','pending');" \
    '^0$'
}

submit_many_jobs() {
  local case_id=$1
  local yaml=$2
  local count=$3
  local ids_file="${LOG_DIR}/${case_id}_job_ids.txt"
  local pids=()
  local i pid rc id unique_count

  : >"${ids_file}"
  for i in $(seq 1 "${count}"); do
    (
      cd "${RUNQ_PROJECT_DIR}"
      timeout --kill-after=5s "${RUNQ_COMMAND_TIMEOUT}" "${RUNQ_CMD[@]}" submit "${yaml}"
    ) >"${LOG_DIR}/${case_id}_submit_${i}.log" 2>&1 &
    pids+=("$!")
  done

  rc=0
  for pid in "${pids[@]}"; do
    if ! wait "${pid}"; then
      rc=1
    fi
  done
  [[ ${rc} -eq 0 ]]

  for i in $(seq 1 "${count}"); do
    id=$(extract_id_from_text <"${LOG_DIR}/${case_id}_submit_${i}.log" || true)
    [[ -n "${id}" ]]
    printf '%s\n' "${id}" >>"${ids_file}"
  done

  unique_count=$(sort -u "${ids_file}" | wc -l | tr -d ' ')
  [[ "${unique_count}" == "${count}" ]]
  cat "${ids_file}"
}

resolve_runq() {
  if [[ -d "${REPO_ROOT}/cmd/runq" ]]; then
    timeout --kill-after=10s "${RUNQ_COMMAND_TIMEOUT}" go test ./...
    timeout --kill-after=10s "${RUNQ_COMMAND_TIMEOUT}" go build -o "${RUNQ_BIN}" ./cmd/runq
    RUNQ_CMD=("${RUNQ_BIN}")
    return 0
  fi

  local runq_path
  runq_path=$(command -v runq || true)
  if [[ -z "${runq_path}" ]]; then
    printf 'runq not found in PATH, even after appending $HOME/bin\n' >&2
    return 1
  fi
  RUNQ_CMD=("${runq_path}")
}

prepare_workspace() {
  case "${RUNQ_E2E_ROOT}" in
    ""|"/"|"/tmp"|"/home"|"$HOME"|"$REPO_ROOT"|"$RUNQ_E2E_BASE")
      printf 'refusing unsafe RUNQ_E2E_ROOT: %s\n' "${RUNQ_E2E_ROOT}" >&2
      return 1
      ;;
  esac
  rm -rf "${RUNQ_E2E_ROOT}"
  mkdir -p "${RUNQ_DATA_DIR}" "${RUNQ_PROJECT_DIR}/logs" "${LOG_DIR}"
  cp "${SCRIPT_DIR}/gpu_probe.py" "${RUNQ_PROJECT_DIR}/gpu_probe.py"
  cp "${SCRIPT_DIR}"/*.yaml "${RUNQ_PROJECT_DIR}/"
  sed -i "s#^working_dir: .*#working_dir: ${RUNQ_PROJECT_DIR}#" \
    "${RUNQ_PROJECT_DIR}/project.yaml" \
    "${RUNQ_PROJECT_DIR}/project_bad_template.yaml"
  chmod +x "${RUNQ_PROJECT_DIR}/gpu_probe.py"
}

write_report() {
  mkdir -p "${RUNQ_E2E_ROOT}"
  {
    printf '# runq MVP 4-GPU Linux Test Report\n\n'
    printf '## Environment\n\n```text\n'
    [[ -f "${LOG_DIR}/env.log" ]] && sed -n '1,240p' "${LOG_DIR}/env.log"
    printf '```\n\n'
    printf '## Summary\n\n'
    printf '| Case | Result | Notes |\n|---|---|---|\n'
    for row in "${SUMMARY_ROWS[@]}"; do
      printf '%s\n' "${row}"
    done
    printf '\n## Failures\n\n'
    if [[ ${#FAILURES[@]} -eq 0 ]]; then
      printf 'None.\n'
    else
      for failure in "${FAILURES[@]}"; do
        printf '%s\n' "${failure}"
      done
    fi
    printf '\n## Logs And Artifacts\n\n'
    printf '%s\n' "- RUNQ_DATA_DIR: \`${RUNQ_DATA_DIR}\`"
    printf '%s\n' "- RUNQ_PROJECT_DIR: \`${RUNQ_PROJECT_DIR}\`"
    printf '%s\n' "- RUNQ_E2E_RUN_ID: \`${RUNQ_E2E_RUN_ID}\`"
    printf '%s\n' "- RUNQ_E2E_BASE: \`${RUNQ_E2E_BASE}\`"
    printf '%s\n' "- Case logs: \`${LOG_DIR}\`"
    printf '%s\n' "- Task logs: \`${RUNQ_PROJECT_DIR}/logs\`"
    printf '%s\n' "- E2E timeout: \`${RUNQ_E2E_TIMEOUT}\`"
    printf '%s\n' "- Command timeout: \`${RUNQ_COMMAND_TIMEOUT}\`"
    printf '%s\n' "- Cleaned default data dir: \`${RUNQ_DEFAULT_DATA_DIR}\`"
  } >"${REPORT}"
}

cleanup() {
  set +e
  if [[ ${KEEP_RUNQ_E2E:-0} != "1" && ${#RUNQ_CMD[@]} -gt 0 ]]; then
    timeout --kill-after=5s "${RUNQ_COMMAND_TIMEOUT}" "${RUNQ_CMD[@]}" daemon stop >/dev/null 2>&1 || true
  fi
  if [[ ${RUNQ_CLEAN_DEFAULT_DATA_DIR:-1} == "1" ]]; then
    case "${RUNQ_DEFAULT_DATA_DIR}" in
      ""|"/"|"/tmp"|"/home"|"$HOME")
        printf 'refusing unsafe RUNQ_DEFAULT_DATA_DIR cleanup: %s\n' "${RUNQ_DEFAULT_DATA_DIR}" >&2
        ;;
      *)
        rm -rf "${RUNQ_DEFAULT_DATA_DIR}"
        ;;
    esac
  fi
  write_report
}

run_case() {
  local case_id=$1
  local notes=$2
  shift 2

  log "${case_id}: ${notes}"
  set +e
  ( set -e; "$@" )
  local rc=$?
  set -e
  if [[ ${rc} -eq 0 ]]; then
    record "${case_id}" "PASS" "${notes}"
  else
    fail_case "${case_id}" "${notes}; see ${LOG_DIR}/${case_id}*.log"
  fi
}

t00_environment() {
  {
    uname -a
    go version || true
    python3 --version || true
    nvidia-smi -L || true
    nvidia-smi --query-gpu=index,name,memory.free,memory.used,utilization.gpu --format=csv,noheader,nounits || true
    pwd
    git status --short || true
  } >"${LOG_DIR}/env.log" 2>&1

  local gpu_count
  gpu_count=$(timeout --kill-after=5s "${RUNQ_COMMAND_TIMEOUT}" nvidia-smi -L | wc -l | tr -d ' ')
  [[ "${gpu_count}" == "4" ]]
  run_log T00_probe python3 "${RUNQ_PROJECT_DIR}/gpu_probe.py" --name local --sleep 1
}

t01_start_daemon() {
  run_log T01_start "${RUNQ_CMD[@]}" daemon start --detach
  sleep 2
  [[ -S "${RUNQ_SOCKET}" ]]
  [[ -f "${RUNQ_DATA_DIR}/daemon.pid" ]]
  local status gpu
  status=$(capture T01_status "${RUNQ_CMD[@]}" status)
  gpu=$(capture T01_gpu "${RUNQ_CMD[@]}" gpu)
  contains "${status}" 'Running:?[[:space:]]+0'
  contains "${status}" 'Pending:?[[:space:]]+0'
  contains "${status}" 'GPUs free:?[[:space:]]+4'
  for idx in 0 1 2 3; do
    contains "${gpu}" "(^|[^0-9])${idx}([^0-9]|$)"
  done
}

t02_duplicate_start() {
  run_log T02_start "${RUNQ_CMD[@]}" daemon start --detach
  sleep 1
  run_log T02_status "${RUNQ_CMD[@]}" status
}

t03_stop_start_daemon() {
  run_log T03_stop "${RUNQ_CMD[@]}" daemon stop
  sleep 1
  [[ ! -S "${RUNQ_SOCKET}" ]]
  set +e
  run_log T03_status_stopped "${RUNQ_CMD[@]}" status
  local rc=$?
  set -e
  [[ ${rc} -ne 0 ]]
  run_log T03_start "${RUNQ_CMD[@]}" daemon start --detach
  sleep 2
  [[ -S "${RUNQ_SOCKET}" ]]
}

t04_project_register() {
  (
    cd "${RUNQ_PROJECT_DIR}"
    run_log T04_add "${RUNQ_CMD[@]}" project add .
    run_log T04_ls "${RUNQ_CMD[@]}" project ls
    run_log T04_show "${RUNQ_CMD[@]}" project show runq_gpu_probe
  )
}

t05_dry_run() {
  local output status
  output=$(cd "${RUNQ_PROJECT_DIR}" && capture T05 "${RUNQ_CMD[@]}" submit job_dryrun.yaml --dry-run)
  [[ $(grep -Eic '(^|[[:space:]│])([abxy])([[:space:]│]|$)' <<<"${output}" || true) -ge 8 ]]
  status=$(capture T05_status "${RUNQ_CMD[@]}" status)
  contains "${status}" 'Running:?[[:space:]]+0'
  contains "${status}" 'Pending:?[[:space:]]+0'
}

submit_job() {
  local log_name=$1
  local yaml=$2
  local output job_id
  output=$(cd "${RUNQ_PROJECT_DIR}" && capture "${log_name}" "${RUNQ_CMD[@]}" submit "${yaml}")
  job_id=$(printf '%s\n' "${output}" | extract_id_from_text)
  printf '%s\n' "${job_id}" >"${LOG_DIR}/${log_name}_job_id.txt"
  printf '%s\n' "${job_id}"
}

t06_1gpu_eight_tasks() {
  JOB1=$(submit_job T06_submit job_1gpu_8tasks.yaml)
  [[ -n "${JOB1}" ]]
  sleep 2
  run_log T06_status "${RUNQ_CMD[@]}" status
  run_log T06_gpu "${RUNQ_CMD[@]}" gpu
  run_log T06_ps "${RUNQ_CMD[@]}" ps --job "${JOB1}"
  sleep 14
  run_log T06_show "${RUNQ_CMD[@]}" job show "${JOB1}"
  local final
  final=$(capture T06_final "${RUNQ_CMD[@]}" ps -a --job "${JOB1}")
  contains "${final}" 'success|succeeded|completed'
}

t07_2gpu_tasks() {
  local job final
  job=$(submit_job T07_submit job_2gpu_2tasks.yaml)
  [[ -n "${job}" ]]
  sleep 2
  run_log T07_gpu "${RUNQ_CMD[@]}" gpu
  run_log T07_ps "${RUNQ_CMD[@]}" ps --job "${job}"
  sleep 10
  final=$(capture T07_final "${RUNQ_CMD[@]}" ps -a --job "${job}")
  contains "${final}" 'success|succeeded|completed'
}

t08_4gpu_task() {
  local job final
  job=$(submit_job T08_submit job_4gpu_1task.yaml)
  [[ -n "${job}" ]]
  sleep 2
  run_log T08_gpu "${RUNQ_CMD[@]}" gpu
  run_log T08_ps "${RUNQ_CMD[@]}" ps --job "${job}"
  sleep 8
  final=$(capture T08_final "${RUNQ_CMD[@]}" ps -a --job "${job}")
  contains "${final}" 'success|succeeded|completed'
}

t09_backfill() {
  local long2 wait4 small final
  long2=$(submit_job T09_long2 job_long_2gpu.yaml)
  sleep 2
  wait4=$(submit_job T09_wait4 job_wait_4gpu.yaml)
  sleep 2
  small=$(submit_job T09_small job_backfill_1gpu.yaml)
  [[ -n "${long2}" && -n "${wait4}" && -n "${small}" ]]
  run_log T09_status "${RUNQ_CMD[@]}" status
  run_log T09_gpu "${RUNQ_CMD[@]}" gpu
  run_log T09_long2_ps "${RUNQ_CMD[@]}" ps --job "${long2}"
  run_log T09_wait4_ps "${RUNQ_CMD[@]}" ps --job "${wait4}"
  run_log T09_small_ps "${RUNQ_CMD[@]}" ps --job "${small}"
  sleep 24
  final=$(capture T09_final "${RUNQ_CMD[@]}" ps -a --job "${wait4}")
  contains "${final}" 'success|succeeded|completed'
}

t10_observe_commands() {
  if [[ -z "${JOB1}" && -f "${LOG_DIR}/T06_submit_job_id.txt" ]]; then
    JOB1=$(<"${LOG_DIR}/T06_submit_job_id.txt")
  fi
  [[ -n "${JOB1}" ]]
  run_log T10_job_ls "${RUNQ_CMD[@]}" job ls
  run_log T10_job_show "${RUNQ_CMD[@]}" job show "${JOB1}"
  local task_id
  task_id=$(sqlite_value "select id from tasks where job_id='${JOB1}' order by enqueued_at limit 1;")
  [[ -n "${task_id}" ]]
  run_log T10_task_show "${RUNQ_CMD[@]}" task show "${task_id}"
  run_log T10_logs "${RUNQ_CMD[@]}" logs "${task_id}" --no-follow
  grep -Eiq 'cuda_visible_devices|heartbeat' "${LOG_DIR}/T10_logs.log"
}

t21_concurrent_submit_light() {
  local ids ids_csv job_count total_tasks task_count success_count
  mapfile -t ids < <(submit_many_jobs T21 job_concurrent_2tasks.yaml 5)
  ids_csv=$(sql_ids "${ids[@]}")
  job_count=$(sqlite_value "select count(*) from jobs where id in (${ids_csv});")
  total_tasks=$(sqlite_value "select coalesce(sum(total_tasks),0) from jobs where id in (${ids_csv});")
  task_count=$(sqlite_value "select count(*) from tasks where job_id in (${ids_csv});")
  [[ "${job_count}" == "5" ]]
  [[ "${total_tasks}" == "10" ]]
  [[ "${task_count}" == "10" ]]
  wait_sql_match T21_wait_success 60 \
    "select count(*) from tasks where job_id in (${ids_csv}) and status='success';" \
    '^10$'
  success_count=$(sqlite_value "select count(*) from tasks where job_id in (${ids_csv}) and status='success';")
  [[ "${success_count}" == "10" ]]
  ! grep -Eiq 'database is locked|SQLITE_BUSY|duplicate id|missing task' "${RUNQ_DATA_DIR}/daemon.log"
}

t22_concurrent_submit_pressure() {
  local ids ids_csv job_count total_tasks task_count
  mapfile -t ids < <(submit_many_jobs T22 job_concurrent_4tasks.yaml 10)
  ids_csv=$(sql_ids "${ids[@]}")
  job_count=$(sqlite_value "select count(*) from jobs where id in (${ids_csv});")
  total_tasks=$(sqlite_value "select coalesce(sum(total_tasks),0) from jobs where id in (${ids_csv});")
  task_count=$(sqlite_value "select count(*) from tasks where job_id in (${ids_csv});")
  [[ "${job_count}" == "10" ]]
  [[ "${total_tasks}" == "40" ]]
  [[ "${task_count}" == "40" ]]
  wait_sql_match T22_wait_success 120 \
    "select count(*) from tasks where job_id in (${ids_csv}) and status='success';" \
    '^40$'
  ! grep -Eiq 'database is locked|SQLITE_BUSY|duplicate id|missing task' "${RUNQ_DATA_DIR}/daemon.log"
}

t23_pause_survives_daemon_restart() {
  local job status_before pending_before status_after pending_after terminal_count
  job=$(submit_job T23_submit job_pause_restart.yaml)
  [[ -n "${job}" ]]
  run_log T23_pause "${RUNQ_CMD[@]}" job pause "${job}"
  sleep 2
  status_before=$(sqlite_value "select status from jobs where id='${job}';")
  pending_before=$(sqlite_value "select count(*) from tasks where job_id='${job}' and status='pending';")
  [[ "${status_before}" == "paused" ]]
  [[ "${pending_before}" -gt 0 ]]

  run_log T23_stop "${RUNQ_CMD[@]}" daemon stop
  sleep 1
  run_log T23_start "${RUNQ_CMD[@]}" daemon start --detach
  sleep 3

  status_after=$(sqlite_value "select status from jobs where id='${job}';")
  pending_after=$(sqlite_value "select count(*) from tasks where job_id='${job}' and status='pending';")
  [[ "${status_after}" == "paused" ]]
  [[ "${pending_after}" -gt 0 ]]

  run_log T23_resume "${RUNQ_CMD[@]}" job resume "${job}"
  wait_job_terminal T23_wait_terminal "${job}" 8 90
  terminal_count=$(sqlite_value "select count(*) from tasks where job_id='${job}' and status in ('success','failed','killed');")
  [[ "${terminal_count}" == "8" ]]
}

t24_kill_running_task_regression() {
  local job task killed_status retry_count success_count active_count
  job=$(submit_job T24_submit job_kill_running.yaml)
  [[ -n "${job}" ]]
  wait_sql_match T24_wait_running 30 \
    "select count(*) from tasks where job_id='${job}' and status='running';" \
    '^[1-4]$'
  task=$(sqlite_value "select id from tasks where job_id='${job}' and status='running' limit 1;")
  [[ -n "${task}" ]]
  run_log T24_kill "${RUNQ_CMD[@]}" kill "${task}"
  wait_sql_match T24_wait_killed 30 \
    "select status from tasks where id='${task}';" \
    '^killed$'
  killed_status=$(sqlite_value "select status from tasks where id='${task}';")
  retry_count=$(sqlite_value "select retry_count from tasks where id='${task}';")
  [[ "${killed_status}" == "killed" ]]
  [[ "${retry_count}" == "0" ]]
  wait_job_terminal T24_wait_terminal "${job}" 5 90
  success_count=$(sqlite_value "select count(*) from tasks where job_id='${job}' and status='success';")
  active_count=$(sqlite_value "select count(*) from tasks where job_id='${job}' and status in ('running','pending');")
  [[ "${success_count}" -ge 4 ]]
  [[ "${active_count}" == "0" ]]
}

t25_missing_logs_dir_autocreate() {
  local job success_count
  trap 'mkdir -p "${RUNQ_PROJECT_DIR}/logs"' RETURN
  wait_no_active_tasks T25_wait_clean 90
  rm -rf "${RUNQ_PROJECT_DIR}/logs"
  job=$(submit_job T25_submit job_missing_logs_dir.yaml)
  [[ -n "${job}" ]]
  wait_job_terminal T25_wait_terminal "${job}" 1 30
  [[ -d "${RUNQ_PROJECT_DIR}/logs" ]]
  success_count=$(sqlite_value "select count(*) from tasks where job_id='${job}' and status='success';")
  [[ "${success_count}" == "1" ]]
}

t26_db_queue_consistency_smoke() {
  wait_no_active_tasks T26_wait_clean 120
  local mismatch active terminal_without_finish success_without_runtime
  mismatch=$(sqlite_value "select count(*) from jobs j where j.total_tasks != (select count(*) from tasks t where t.job_id=j.id);")
  active=$(sqlite_value "select count(*) from tasks where status in ('running','pending');")
  terminal_without_finish=$(sqlite_value "select count(*) from tasks where status in ('success','failed','killed') and finished_at is null;")
  success_without_runtime=$(sqlite_value "select count(*) from tasks where status='success' and (pid=0 or gpus='' or started_at is null);")
  [[ "${mismatch}" == "0" ]]
  [[ "${active}" == "0" ]]
  [[ "${terminal_without_finish}" == "0" ]]
  [[ "${success_without_runtime}" == "0" ]]
}

t11_kill_task() {
  local job task
  job=$(submit_job T11_submit job_kill_task.yaml)
  sleep 2
  task=$(sqlite_value "select id from tasks where job_id='${job}' and status='running' limit 1;")
  [[ -n "${task}" ]]
  run_log T11_kill "${RUNQ_CMD[@]}" kill "${task}"
  sleep 2
  run_log T11_task_show "${RUNQ_CMD[@]}" task show "${task}"
  run_log T11_gpu "${RUNQ_CMD[@]}" gpu
}

t12_kill_job() {
  local job
  job=$(submit_job T12_submit job_kill_task.yaml)
  sleep 2
  run_log T12_kill "${RUNQ_CMD[@]}" job kill "${job}"
  sleep 2
  run_log T12_show "${RUNQ_CMD[@]}" job show "${job}"
  run_log T12_status "${RUNQ_CMD[@]}" status
}

t13_auto_retry() {
  JOBF=$(submit_job T13_submit job_fail_retry.yaml)
  [[ -n "${JOBF}" ]]
  sleep 8
  run_log T13_show "${RUNQ_CMD[@]}" job show "${JOBF}"
  local final
  final=$(capture T13_ps "${RUNQ_CMD[@]}" ps -a --job "${JOBF}")
  contains "${final}" 'failed'
}

t14_manual_retry() {
  if [[ -z "${JOBF}" && -f "${LOG_DIR}/T13_submit_job_id.txt" ]]; then
    JOBF=$(<"${LOG_DIR}/T13_submit_job_id.txt")
  fi
  [[ -n "${JOBF}" ]]
  local task status
  task=$(sqlite_value "select id from tasks where job_id='${JOBF}' and status='failed' limit 1;")
  [[ -n "${task}" ]]
  run_log T14_retry "${RUNQ_CMD[@]}" task retry "${task}"
  wait_sql_match T14_wait_terminal 30 \
    "select status from tasks where id='${task}';" \
    '^(failed|success)$'
  status=$(sqlite_value "select status from tasks where id='${task}';")
  [[ "${status}" == "failed" || "${status}" == "success" ]]
  run_log T14_show "${RUNQ_CMD[@]}" task show "${task}"
}

t15_pause_resume() {
  local job terminal_count
  job=$(submit_job T15_submit job_pause_8tasks.yaml)
  sleep 2
  run_log T15_pause "${RUNQ_CMD[@]}" job pause "${job}"
  run_log T15_show_paused "${RUNQ_CMD[@]}" job show "${job}"
  sleep 5
  run_log T15_ps_paused "${RUNQ_CMD[@]}" ps --job "${job}"
  run_log T15_resume "${RUNQ_CMD[@]}" job resume "${job}"
  wait_job_terminal T15_wait_terminal "${job}" 8 90
  terminal_count=$(sqlite_value "select count(*) from tasks where job_id='${job}' and status='success';")
  [[ "${terminal_count}" == "8" ]]
}

t16_daemon_restart_recovery() {
  local job active_count terminal_count
  job=$(submit_job T16_submit job_restart_8tasks.yaml)
  sleep 2
  run_log T16_restart "${RUNQ_CMD[@]}" daemon restart
  sleep 3
  run_log T16_status "${RUNQ_CMD[@]}" status
  run_log T16_ps "${RUNQ_CMD[@]}" ps --job "${job}"
  wait_job_terminal T16_wait_terminal "${job}" 8 120
  terminal_count=$(sqlite_value "select count(*) from tasks where job_id='${job}' and status in ('success','failed','killed');")
  active_count=$(sqlite_value "select count(*) from tasks where job_id='${job}' and status in ('running','pending');")
  [[ "${terminal_count}" == "8" ]]
  [[ "${active_count}" == "0" ]]
}

t17_unknown_project() {
  set +e
  (cd "${RUNQ_PROJECT_DIR}" && run_log T17_submit "${RUNQ_CMD[@]}" submit job_unknown_project.yaml)
  local rc=$?
  set -e
  [[ ${rc} -ne 0 ]]
  run_log T17_job_ls "${RUNQ_CMD[@]}" job ls
}

t18_bad_list() {
  set +e
  (cd "${RUNQ_PROJECT_DIR}" && run_log T18_submit "${RUNQ_CMD[@]}" submit job_bad_list.yaml)
  local rc=$?
  set -e
  [[ ${rc} -ne 0 ]]
  run_log T18_status "${RUNQ_CMD[@]}" status
}

t19_too_many_gpus() {
  local job status
  job=$(submit_job T19_submit job_too_many_gpus.yaml)
  trap '"${RUNQ_CMD[@]}" job kill "${job}" >/dev/null 2>&1 || true' RETURN
  sleep 3
  status=$(capture T19_status "${RUNQ_CMD[@]}" status)
  run_log T19_ps "${RUNQ_CMD[@]}" ps --job "${job}"
  contains "${status}" 'GPUs free:?[[:space:]]+4'
  run_log T19_kill "${RUNQ_CMD[@]}" job kill "${job}"
  sleep 1
  run_log T19_status_after "${RUNQ_CMD[@]}" status
}

t20_bad_template() {
  (cd "${RUNQ_PROJECT_DIR}" && run_log T20_project "${RUNQ_CMD[@]}" project add runq_bad_template --file project_bad_template.yaml)
  set +e
  (cd "${RUNQ_PROJECT_DIR}" && run_log T20_submit "${RUNQ_CMD[@]}" submit job_bad_template.yaml)
  local rc=$?
  set -e
  [[ ${rc} -ne 0 ]]
}

trap cleanup EXIT

prepare_workspace
cd "${REPO_ROOT}"
resolve_runq

run_case T00 "environment has exactly four NVIDIA GPUs" t00_environment
run_case T01 "daemon starts, socket and GPU inventory are visible" t01_start_daemon
run_case T02 "duplicate daemon start keeps one responsive daemon" t02_duplicate_start
run_case T03 "daemon stop cleans socket and daemon can restart" t03_stop_start_daemon
run_case T04 "project registration and show commands work" t04_project_register
run_case T05 "dry-run expands sweep without enqueueing tasks" t05_dry_run
run_case T06 "eight 1-GPU tasks complete across four cards" t06_1gpu_eight_tasks
run_case T07 "two 2-GPU tasks run concurrently" t07_2gpu_tasks
run_case T08 "one 4-GPU task uses all cards" t08_4gpu_task
run_case T09 "large pending task allows 1-GPU backfill" t09_backfill
run_case T10 "job, task and logs observation commands work" t10_observe_commands
run_case T21 "five concurrent submits each create two 1-GPU tasks" t21_concurrent_submit_light
run_case T22 "ten concurrent submits each create four short 1-GPU tasks" t22_concurrent_submit_pressure
run_case T23 "paused job remains paused across daemon stop/start and resumes afterward" t23_pause_survives_daemon_restart
run_case T24 "killing one running task marks killed without retry and frees GPU for pending work" t24_kill_running_task_regression
run_case T11 "single task kill releases GPU" t11_kill_task
run_case T12 "job kill terminates running and pending tasks" t12_kill_job
run_case T13 "failed task auto-retries and ends failed" t13_auto_retry
run_case T14 "manual task retry requeues failed task" t14_manual_retry
run_case T15 "pause leaves running tasks alive and resume drains pending" t15_pause_resume
run_case T16 "daemon restart recovers pending task state" t16_daemon_restart_recovery
run_case T17 "unknown project is rejected without daemon crash" t17_unknown_project
run_case T18 "malformed list sweep is rejected" t18_bad_list
run_case T19 "gpus_per_task greater than four remains pending and can be killed" t19_too_many_gpus
run_case T20 "invalid command template is rejected" t20_bad_template
run_case T26 "SQLite job/task counts and queue terminal state stay consistent" t26_db_queue_consistency_smoke
run_case T25 "executor handles missing project logs directory" t25_missing_logs_dir_autocreate

write_report
printf '\nReport: %s\n' "${REPORT}"

if printf '%s\n' "${SUMMARY_ROWS[@]}" | grep -q '| FAIL |'; then
  exit 1
fi
