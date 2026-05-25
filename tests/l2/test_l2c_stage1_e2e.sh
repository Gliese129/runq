#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/../.." && pwd)

export RUNQ_L2C_STAGE1_RUN_ID=${RUNQ_L2C_STAGE1_RUN_ID:-$(date '+%Y%m%d-%H%M%S')}
export RUNQ_L2C_STAGE1_BASE=${RUNQ_L2C_STAGE1_BASE:-"/private/tmp/runq-l2c-stage1-${USER:-user}"}
export RUNQ_L2C_STAGE1_ROOT=${RUNQ_L2C_STAGE1_ROOT:-"${RUNQ_L2C_STAGE1_BASE}/${RUNQ_L2C_STAGE1_RUN_ID}"}
export RUNQ_DATA_DIR=${RUNQ_DATA_DIR:-"${RUNQ_L2C_STAGE1_ROOT}/data"}
export RUNQ_SOCKET=${RUNQ_SOCKET:-"${RUNQ_DATA_DIR}/runq.sock"}
export RUNQ_BIN=${RUNQ_BIN:-"${RUNQ_L2C_STAGE1_ROOT}/runq"}
export RUNQ_PROJECT_DIR=${RUNQ_PROJECT_DIR:-"${RUNQ_L2C_STAGE1_ROOT}/project"}
export RUNQ_FAKE_GPU_COUNT=${RUNQ_FAKE_GPU_COUNT:-16}
export RUNQ_L2C_STAGE1_STRESS_TASKS=${RUNQ_L2C_STAGE1_STRESS_TASKS:-8}
export RUNQ_L2C_STAGE1_SLEEP_SECONDS=${RUNQ_L2C_STAGE1_SLEEP_SECONDS:-120}
export GOCACHE=${GOCACHE:-"${REPO_ROOT}/.cache/go-build"}
export GOMODCACHE=${GOMODCACHE:-"${REPO_ROOT}/.cache/go-mod"}

FAKEBIN="${RUNQ_L2C_STAGE1_ROOT}/fakebin"
LOG_DIR="${RUNQ_L2C_STAGE1_ROOT}/case-logs"
REPORT="${RUNQ_L2C_STAGE1_ROOT}/report.md"
DB_PATH="${RUNQ_DATA_DIR}/runq.db"
RUNQ_CMD=("${RUNQ_BIN}")
SUMMARY_ROWS=()
FAILURES=()

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

print_cmd() {
  printf '$'
  printf ' %q' "$@"
  printf '\n'
}

run_log() {
  local name=$1
  shift

  mkdir -p "${LOG_DIR}"
  {
    print_cmd "$@"
    "$@"
  } >"${LOG_DIR}/${name}.log" 2>&1
}

capture() {
  local name=$1
  shift

  mkdir -p "${LOG_DIR}"
  {
    print_cmd "$@"
  } >"${LOG_DIR}/${name}.log"
  "$@" 2>&1 | tee -a "${LOG_DIR}/${name}.log"
}

contains() {
  grep -Eiq "$2" <<<"$1"
}

extract_id_from_text() {
  grep -Eo 'id=[^ ]+' | head -n 1 | cut -d= -f2
}

sqlite_value() {
  sqlite3 "${DB_PATH}" "$1"
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

submit_job() {
  local log_name=$1
  local yaml=$2
  local log_file="${LOG_DIR}/${log_name}.log"
  local job_id rc

  mkdir -p "${LOG_DIR}"
  print_cmd "${RUNQ_CMD[@]}" submit "${yaml}" >"${log_file}"
  set +e
  (cd "${RUNQ_PROJECT_DIR}" && "${RUNQ_CMD[@]}" submit "${yaml}") >>"${log_file}" 2>&1
  rc=$?
  set -e
  if [[ ${rc} -ne 0 ]]; then
    cat "${log_file}" >&2
    return "${rc}"
  fi

  job_id=$(extract_id_from_text <"${log_file}" || true)
  [[ -n "${job_id}" ]]
  printf '%s\n' "${job_id}" >"${LOG_DIR}/${log_name}_job_id.txt"
  printf '%s\n' "${job_id}"
}

task_ids_for_job() {
  local job_id=$1
  sqlite3 -noheader -list "${DB_PATH}" \
    "select id from tasks where job_id='${job_id}' order by enqueued_at, id;"
}

first_task_for_job() {
  local job_id=$1
  task_ids_for_job "${job_id}" | head -n 1
}

task_field() {
  local task_id=$1
  local field=$2
  sqlite_value "select ${field} from tasks where id='${task_id}';"
}

mount_for_path() {
  df -P "$1" | awk 'NR == 2 {print $6}'
}

wait_job_running() {
  local name=$1
  local job_id=$2
  local expected=$3
  local timeout_seconds=$4

  wait_sql_match "${name}" "${timeout_seconds}" \
    "select count(*) from tasks where job_id='${job_id}' and status='running' and pid is not null and pid != 0;" \
    "^${expected}$"
}

api_post() {
  local name=$1
  local path=$2
  local data=${3:-}
  local expected_status=${4:-200}
  local body_file="${LOG_DIR}/${name}.json"
  local meta_file="${LOG_DIR}/${name}.meta"
  local status

  mkdir -p "${LOG_DIR}"
  if [[ -n "${data}" ]]; then
    status=$(curl --silent --show-error --unix-socket "${RUNQ_SOCKET}" \
      -X POST -H 'Content-Type: application/json' --data "${data}" \
      -o "${body_file}" -w '%{http_code}' "http://runq${path}")
  else
    status=$(curl --silent --show-error --unix-socket "${RUNQ_SOCKET}" \
      -X POST -o "${body_file}" -w '%{http_code}' "http://runq${path}")
  fi

  {
    printf 'POST %s\n' "${path}"
    printf 'status: %s\n' "${status}"
    if [[ -n "${data}" ]]; then
      printf 'body: %s\n' "${data}"
    fi
  } >"${meta_file}"

  if [[ "${status}" != "${expected_status}" ]]; then
    printf 'expected HTTP %s, got %s\n' "${expected_status}" "${status}" >&2
    cat "${body_file}" >&2 || true
    return 1
  fi
  cat "${body_file}"
}

assert_thaw_json_counts() {
  local json=$1
  local expected_thawed=$2
  local expected_blocked=$3
  JSON_PAYLOAD="${json}" python3 - "${expected_thawed}" "${expected_blocked}" <<'PY'
import json
import os
import sys

data = json.loads(os.environ["JSON_PAYLOAD"])
expected_thawed = int(sys.argv[1])
expected_blocked = int(sys.argv[2])
thawed = data.get("thawed") or []
blocked = data.get("blocked") or {}
if len(thawed) != expected_thawed or len(blocked) != expected_blocked:
    raise SystemExit(
        f"expected thawed={expected_thawed}, blocked={expected_blocked}; "
        f"got thawed={len(thawed)}, blocked={len(blocked)}; payload={data}"
    )
PY
}

freeze_payload() {
  local task_id=$1
  local free_bytes=$2
  local needed_est=$3
  local mount=$4
  printf '{"task_id":"%s","free_bytes":%s,"needed_est":%s,"mount":"%s"}' \
    "${task_id}" "${free_bytes}" "${needed_est}" "${mount}"
}

freeze_task() {
  local name=$1
  local task_id=$2
  local needed_est=$3
  local mount=$4
  local payload

  payload=$(freeze_payload "${task_id}" 1 "${needed_est}" "${mount}")
  api_post "${name}" "/api/internal/freeze-self" "${payload}" 200 >/dev/null
}

maybe_wait_process_state() {
  local task_id=$1
  local expected_prefix=$2
  local pid state saw_state

  pid=$(task_field "${task_id}" pid)
  [[ -n "${pid}" ]]
  saw_state=0
  for _ in $(seq 1 20); do
    state=$(ps -o state= -p "${pid}" 2>/dev/null | tr -d ' ' || true)
    if [[ -n "${state}" ]]; then
      saw_state=1
      if [[ "${state}" == "${expected_prefix}"* ]]; then
        return 0
      fi
    fi
    sleep 0.1
  done

  if [[ "${saw_state}" == "0" ]]; then
    log "ps state unavailable for task ${task_id}; skipping OS-state assertion"
    return 0
  fi

  printf 'task %s pid %s state did not become %s* (last=%s)\n' \
    "${task_id}" "${pid}" "${expected_prefix}" "${state}" >&2
  return 1
}

write_report() {
  mkdir -p "${RUNQ_L2C_STAGE1_ROOT}"
  {
    printf '# runq L2-C Stage 1 E2E Test Report\n\n'
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
    printf '\n## Artifacts\n\n'
    printf -- '- RUNQ_L2C_STAGE1_ROOT: `%s`\n' "${RUNQ_L2C_STAGE1_ROOT}"
    printf -- '- RUNQ_DATA_DIR: `%s`\n' "${RUNQ_DATA_DIR}"
    printf -- '- RUNQ_SOCKET: `%s`\n' "${RUNQ_SOCKET}"
    printf -- '- RUNQ_PROJECT_DIR: `%s`\n' "${RUNQ_PROJECT_DIR}"
    printf -- '- Case logs: `%s`\n' "${LOG_DIR}"
  } >"${REPORT}"
}

stop_daemon() {
  set +e
  "${RUNQ_CMD[@]}" daemon stop >/dev/null 2>&1
  set -e
}

cleanup() {
  stop_daemon || true
  write_report || true
}

prepare_workspace() {
  case "${RUNQ_L2C_STAGE1_ROOT}" in
    ""|"/"|"/tmp"|"/private/tmp"|"/home"|"$HOME"|"$REPO_ROOT"|"$RUNQ_L2C_STAGE1_BASE")
      printf 'refusing unsafe RUNQ_L2C_STAGE1_ROOT: %s\n' "${RUNQ_L2C_STAGE1_ROOT}" >&2
      return 1
      ;;
  esac

  command -v go >/dev/null 2>&1
  command -v sqlite3 >/dev/null 2>&1
  command -v curl >/dev/null 2>&1
  command -v python3 >/dev/null 2>&1
  command -v df >/dev/null 2>&1

  local min_gpus
  min_gpus=$((RUNQ_L2C_STAGE1_STRESS_TASKS + 3))
  if (( RUNQ_FAKE_GPU_COUNT < min_gpus )); then
    printf 'RUNQ_FAKE_GPU_COUNT=%s is too small; need at least %s for this script\n' \
      "${RUNQ_FAKE_GPU_COUNT}" "${min_gpus}" >&2
    return 1
  fi

  rm -rf "${RUNQ_L2C_STAGE1_ROOT}"
  mkdir -p "${RUNQ_DATA_DIR}" "${RUNQ_PROJECT_DIR}" "${LOG_DIR}" "${FAKEBIN}"

  cat >"${FAKEBIN}/nvidia-smi" <<'FAKE_NVIDIA_SMI'
#!/usr/bin/env bash
set -euo pipefail

gpu_count=${RUNQ_FAKE_GPU_COUNT:-16}

if [[ "${1:-}" == "pmon" ]]; then
  echo "# gpu pid type sm mem enc dec fb command"
  for i in $(seq 0 $((gpu_count - 1))); do
    echo "${i} - - - - - - 0 -"
  done
  exit 0
fi

args="$*"
if [[ "${args}" == *"--query-gpu=index,name,memory.free,memory.used,utilization.gpu"* ]]; then
  for i in $(seq 0 $((gpu_count - 1))); do
    echo "${i}, FakeGPU-${i}, 80000, 0, 0"
  done
  exit 0
fi

if [[ "${args}" == *"--query-gpu=index"* ]]; then
  for i in $(seq 0 $((gpu_count - 1))); do
    echo "${i}"
  done
  exit 0
fi

if [[ "${1:-}" == "-L" ]]; then
  for i in $(seq 0 $((gpu_count - 1))); do
    echo "GPU ${i}: FakeGPU-${i} (UUID: GPU-fake-${i})"
  done
  exit 0
fi

echo "fake nvidia-smi: unsupported args: $*" >&2
exit 1
FAKE_NVIDIA_SMI
  chmod +x "${FAKEBIN}/nvidia-smi"
  export PATH="${FAKEBIN}:${PATH}"

  cat >"${RUNQ_PROJECT_DIR}/project.yaml" <<YAML
project_name: l2c_stage1
working_dir: ${RUNQ_PROJECT_DIR}
command_template: 'sleep ${RUNQ_L2C_STAGE1_SLEEP_SECONDS} # {{args}}'
defaults:
  gpus_per_task: 1
  max_retry: 1
YAML

  cat >"${RUNQ_PROJECT_DIR}/normal_job.yaml" <<'YAML'
project: l2c_stage1
description: L2-C normal checked thaw
sweep:
  - method: list
    parameters:
      sample: [normal]
YAML

  cat >"${RUNQ_PROJECT_DIR}/blocked_job.yaml" <<'YAML'
project: l2c_stage1
description: L2-C blocked then force thaw
sweep:
  - method: list
    parameters:
      sample: [blocked]
YAML

  cat >"${RUNQ_PROJECT_DIR}/owner_job.yaml" <<'YAML'
project: l2c_stage1
description: L2-C owner-scoped thaw API
sweep:
  - method: list
    parameters:
      sample: [owner]
YAML

  {
    printf 'project: l2c_stage1\n'
    printf 'description: L2-C pressure freeze thaw\n'
    printf 'sweep:\n'
    printf '  - method: list\n'
    printf '    parameters:\n'
    printf '      sample: ['
    local first=1
    local i
    for i in $(seq 1 "${RUNQ_L2C_STAGE1_STRESS_TASKS}"); do
      if [[ ${first} -eq 0 ]]; then
        printf ', '
      fi
      printf 'stress%s' "${i}"
      first=0
    done
    printf ']\n'
  } >"${RUNQ_PROJECT_DIR}/stress_job.yaml"
}

build_runq() {
  cd "${REPO_ROOT}"
  mkdir -p "${GOCACHE}" "${GOMODCACHE}"
  GOCACHE="${GOCACHE}" GOMODCACHE="${GOMODCACHE}" go build -o "${RUNQ_BIN}" ./cmd/runq
}

start_daemon() {
  run_log daemon_start "${RUNQ_CMD[@]}" daemon start --detach
  for _ in $(seq 1 80); do
    [[ -S "${RUNQ_SOCKET}" ]] && [[ -f "${DB_PATH}" ]] && return 0
    sleep 0.25
  done
  return 1
}

t00_environment() {
  command -v nvidia-smi >/dev/null 2>&1
  [[ "$(nvidia-smi -L | wc -l | tr -d ' ')" == "${RUNQ_FAKE_GPU_COUNT}" ]]
  start_daemon
  local status
  status=$(capture T00_status "${RUNQ_CMD[@]}" status)
  contains "${status}" "GPUs free:?[[:space:]]+${RUNQ_FAKE_GPU_COUNT}"
  (
    cd "${RUNQ_PROJECT_DIR}"
    run_log T00_project_add "${RUNQ_CMD[@]}" project add --file project.yaml
  )
}

t01_normal_checked_thaw() {
  local job task task_dir ckpt_dir mount out out_again

  job=$(submit_job T01_submit normal_job.yaml)
  wait_job_running T01_wait_running "${job}" 1 30
  task=$(first_task_for_job "${job}")
  [[ -n "${task}" ]]
  task_dir=$(task_field "${task}" task_dir)
  [[ -d "${task_dir}/checkpoints" ]]
  ckpt_dir="${task_dir}/checkpoints"
  mount=$(mount_for_path "${ckpt_dir}")
  [[ -n "${mount}" ]]

  freeze_task T01_freeze "${task}" 1 "${mount}"
  maybe_wait_process_state "${task}" T

  out=$(capture T01_thaw "${RUNQ_CMD[@]}" thaw)
  grep -Fq "thawed ${task}" <<<"${out}"
  wait_sql_match T01_still_running 5 \
    "select count(*) from tasks where id='${task}' and status='running';" '^1$'

  out_again=$(capture T01_thaw_again "${RUNQ_CMD[@]}" thaw)
  grep -Fq "nothing was frozen for you" <<<"${out_again}"
}

t02_blocked_then_force_thaw() {
  local job task task_dir ckpt_dir mount out force_out blocked_count

  job=$(submit_job T02_submit blocked_job.yaml)
  wait_job_running T02_wait_running "${job}" 1 30
  task=$(first_task_for_job "${job}")
  [[ -n "${task}" ]]
  task_dir=$(task_field "${task}" task_dir)
  ckpt_dir="${task_dir}/checkpoints"
  mount=$(mount_for_path "${ckpt_dir}")
  [[ -n "${mount}" ]]

  freeze_task T02_freeze "${task}" 1152921504606846976 "${mount}"
  maybe_wait_process_state "${task}" T

  out=$(capture T02_checked_thaw "${RUNQ_CMD[@]}" thaw)
  grep -Fq "${task} blocked" <<<"${out}"
  grep -Fq "Hint: clean disk space" <<<"${out}"
  blocked_count=$(grep -Ec ' blocked \(mount ' <<<"${out}" || true)
  [[ "${blocked_count}" == "1" ]]
  wait_sql_match T02_still_running 5 \
    "select count(*) from tasks where id='${task}' and status='running';" '^1$'

  force_out=$(capture T02_force_thaw "${RUNQ_CMD[@]}" thaw --force)
  grep -Fq "thawed ${task}" <<<"${force_out}"
}

t03_owner_scoped_api_shape() {
  local job task task_dir ckpt_dir mount wrong_owner checked force owner_uid

  job=$(submit_job T03_submit owner_job.yaml)
  wait_job_running T03_wait_running "${job}" 1 30
  task=$(first_task_for_job "${job}")
  [[ -n "${task}" ]]
  task_dir=$(task_field "${task}" task_dir)
  ckpt_dir="${task_dir}/checkpoints"
  mount=$(mount_for_path "${ckpt_dir}")
  [[ -n "${mount}" ]]

  freeze_task T03_freeze "${task}" 1152921504606846976 "${mount}"

  wrong_owner=$(api_post T03_wrong_owner "/api/thaw?owner=-1" "" 200)
  assert_thaw_json_counts "${wrong_owner}" 0 0

  owner_uid=$(id -u)
  checked=$(api_post T03_checked_owner "/api/thaw?owner=${owner_uid}" "" 200)
  assert_thaw_json_counts "${checked}" 0 1

  force=$(api_post T03_force_owner "/api/thaw?owner=${owner_uid}&force=true" "" 200)
  assert_thaw_json_counts "${force}" 1 0
}

t04_pressure_extreme_freeze_thaw() {
  local job checked out force_out blocked_count thawed_count task_id task_dir ckpt_dir mount
  local task_ids=()

  job=$(submit_job T04_submit stress_job.yaml)
  wait_job_running T04_wait_running "${job}" "${RUNQ_L2C_STAGE1_STRESS_TASKS}" 45

  while IFS= read -r task_id; do
    [[ -n "${task_id}" ]] && task_ids+=("${task_id}")
  done < <(task_ids_for_job "${job}")
  [[ "${#task_ids[@]}" == "${RUNQ_L2C_STAGE1_STRESS_TASKS}" ]]

  for task_id in "${task_ids[@]}"; do
    task_dir=$(task_field "${task_id}" task_dir)
    ckpt_dir="${task_dir}/checkpoints"
    mount=$(mount_for_path "${ckpt_dir}")
    [[ -n "${mount}" ]]
    freeze_task "T04_freeze_${task_id}" "${task_id}" 1152921504606846976 "${mount}"
  done

  checked=$(api_post T04_api_checked "/api/thaw?owner=$(id -u)" "" 200)
  assert_thaw_json_counts "${checked}" 0 "${RUNQ_L2C_STAGE1_STRESS_TASKS}"

  out=$(capture T04_cli_checked "${RUNQ_CMD[@]}" thaw)
  blocked_count=$(grep -Ec ' blocked \(mount ' <<<"${out}" || true)
  [[ "${blocked_count}" == "${RUNQ_L2C_STAGE1_STRESS_TASKS}" ]]

  force_out=$(capture T04_cli_force "${RUNQ_CMD[@]}" thaw --force)
  thawed_count=$(grep -Ec 'thawed ' <<<"${force_out}" || true)
  [[ "${thawed_count}" == "${RUNQ_L2C_STAGE1_STRESS_TASKS}" ]]

  wait_sql_match T04_all_running_after_force 10 \
    "select count(*) from tasks where job_id='${job}' and status='running';" \
    "^${RUNQ_L2C_STAGE1_STRESS_TASKS}$"
}

t05_post_stress_consistency() {
  local failed_or_killed pending running_without_pid job_mismatch log_hits status

  failed_or_killed=$(sqlite_value "select count(*) from tasks where status in ('failed','killed');")
  pending=$(sqlite_value "select count(*) from tasks where status='pending';")
  running_without_pid=$(sqlite_value "select count(*) from tasks where status='running' and (pid is null or pid=0);")
  job_mismatch=$(sqlite_value "select count(*) from jobs j where j.total_tasks != (select count(*) from tasks t where t.job_id=j.id);")
  [[ "${failed_or_killed}" == "0" ]]
  [[ "${pending}" == "0" ]]
  [[ "${running_without_pid}" == "0" ]]
  [[ "${job_mismatch}" == "0" ]]

  status=$(capture T05_status "${RUNQ_CMD[@]}" status)
  contains "${status}" 'Pending:?[[:space:]]+0'

  log_hits=$(grep -Eic 'panic|fatal error|database is locked|SQLITE_BUSY|concurrent map writes' \
    "${RUNQ_DATA_DIR}/daemon.log" || true)
  [[ "${log_hits}" == "0" ]]
}

trap cleanup EXIT

prepare_workspace
build_runq

log "L2-C stage1 root: ${RUNQ_L2C_STAGE1_ROOT}"
log "RUNQ_DATA_DIR: ${RUNQ_DATA_DIR}"
log "RUNQ_SOCKET: ${RUNQ_SOCKET}"
log "fake GPUs: ${RUNQ_FAKE_GPU_COUNT}, stress tasks: ${RUNQ_L2C_STAGE1_STRESS_TASKS}"

run_case T00 "isolated daemon boots with fake GPU inventory and project config" t00_environment
run_case T01 "normal path: SDK freeze with tiny threshold, checked runq thaw releases it" t01_normal_checked_thaw
run_case T02 "extreme path: huge threshold blocks checked thaw, --force releases it" t02_blocked_then_force_thaw
run_case T03 "API path: owner filter and ThawResponse thawed/blocked shape are correct" t03_owner_scoped_api_shape
run_case T04 "pressure: freeze and thaw many concurrent running tasks through API plus CLI" t04_pressure_extreme_freeze_thaw
run_case T05 "post-pressure consistency: no pending/failed tasks, no DB/GPU concurrency crashes" t05_post_stress_consistency

write_report
printf '\nReport: %s\n' "${REPORT}"

if printf '%s\n' "${SUMMARY_ROWS[@]}" | grep -q '| FAIL |'; then
  exit 1
fi
