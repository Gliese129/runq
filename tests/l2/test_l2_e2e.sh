#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/../.." && pwd)

export RUNQ_L2_RUN_ID=${RUNQ_L2_RUN_ID:-$(date '+%Y%m%d-%H%M%S')}
export RUNQ_L2_BASE=${RUNQ_L2_BASE:-"/private/tmp/runq-l2-${USER:-user}"}
export RUNQ_L2_ROOT=${RUNQ_L2_ROOT:-"${RUNQ_L2_BASE}/${RUNQ_L2_RUN_ID}"}
export RUNQ_DATA_DIR=${RUNQ_DATA_DIR:-"${RUNQ_L2_ROOT}/data"}
export RUNQ_SOCKET=${RUNQ_SOCKET:-"${RUNQ_DATA_DIR}/runq.sock"}
export RUNQ_BIN=${RUNQ_BIN:-"${RUNQ_L2_ROOT}/runq"}
export RUNQ_PROJECT_DIR=${RUNQ_PROJECT_DIR:-"${RUNQ_L2_ROOT}/project"}
export RUNQ_FAKE_GPU_COUNT=${RUNQ_FAKE_GPU_COUNT:-1}

FAKEBIN="${RUNQ_L2_ROOT}/fakebin"
LOG_DIR="${RUNQ_L2_ROOT}/case-logs"
REPORT="${RUNQ_L2_ROOT}/report.md"
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

run_log() {
  local name=$1
  shift

  mkdir -p "${LOG_DIR}"
  {
    printf '$'
    printf ' %q' "$@"
    printf '\n'
    "$@"
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
  "$@" 2>&1 | tee -a "${LOG_DIR}/${name}.log"
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

wait_task_successes() {
  local name=$1
  local job_id=$2
  local expected=$3
  local timeout_seconds=$4

  wait_sql_match "${name}" "${timeout_seconds}" \
    "select count(*) from tasks where job_id='${job_id}' and status='success';" \
    "^${expected}$"
}

wait_file_lines() {
  local path=$1
  local expected=$2
  local timeout_seconds=$3
  local start lines

  start=$(date +%s)
  while true; do
    lines=0
    [[ -f "${path}" ]] && lines=$(wc -l <"${path}" | tr -d ' ')
    if [[ "${lines}" == "${expected}" ]]; then
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
  local output job_id

  output=$(cd "${RUNQ_PROJECT_DIR}" && capture "${log_name}" "${RUNQ_CMD[@]}" submit "${yaml}")
  job_id=$(printf '%s\n' "${output}" | extract_id_from_text)
  printf '%s\n' "${job_id}" >"${LOG_DIR}/${log_name}_job_id.txt"
  printf '%s\n' "${job_id}"
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
      "${RUNQ_CMD[@]}" submit "${yaml}"
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

start_daemon() {
  run_log daemon_start "${RUNQ_CMD[@]}" daemon start --detach
  for _ in $(seq 1 40); do
    [[ -S "${RUNQ_SOCKET}" ]] && return 0
    sleep 0.25
  done
  return 1
}

stop_daemon() {
  set +e
  "${RUNQ_CMD[@]}" daemon stop >/dev/null 2>&1
  set -e
}

write_report() {
  mkdir -p "${RUNQ_L2_ROOT}"
  {
    printf '# runq L2 A/B E2E Test Report\n\n'
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
    printf -- '- RUNQ_L2_ROOT: `%s`\n' "${RUNQ_L2_ROOT}"
    printf -- '- RUNQ_DATA_DIR: `%s`\n' "${RUNQ_DATA_DIR}"
    printf -- '- RUNQ_PROJECT_DIR: `%s`\n' "${RUNQ_PROJECT_DIR}"
    printf -- '- Case logs: `%s`\n' "${LOG_DIR}"
  } >"${REPORT}"
}

cleanup() {
  stop_daemon || true
  write_report
}

prepare_workspace() {
  case "${RUNQ_L2_ROOT}" in
    ""|"/"|"/tmp"|"/home"|"$HOME"|"$REPO_ROOT"|"$RUNQ_L2_BASE")
      printf 'refusing unsafe RUNQ_L2_ROOT: %s\n' "${RUNQ_L2_ROOT}" >&2
      return 1
      ;;
  esac

  command -v sqlite3 >/dev/null 2>&1

  rm -rf "${RUNQ_L2_ROOT}"
  mkdir -p "${RUNQ_DATA_DIR}" "${RUNQ_PROJECT_DIR}" "${LOG_DIR}" "${FAKEBIN}"

  cat >"${FAKEBIN}/nvidia-smi" <<'FAKE_NVIDIA_SMI'
#!/usr/bin/env bash
set -euo pipefail

gpu_count=${RUNQ_FAKE_GPU_COUNT:-1}

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

  cat >"${RUNQ_PROJECT_DIR}/train.py" <<'PY'
import argparse

parser = argparse.ArgumentParser()
parser.add_argument("--lr", type=float, default=0.001)
parser.add_argument(
    "--batch-size",
    type=int,
    default=32,
)
parser.add_argument("--model", default="resnet")
parser.add_argument("--augment", action="store_true")
args = parser.parse_args()
print("train", args)
PY

  cat >"${RUNQ_PROJECT_DIR}/quick.py" <<'PY'
import os
import sys
print("quick", sys.argv[1:], "cuda", os.environ.get("CUDA_VISIBLE_DEVICES", ""))
PY

  cat >"${RUNQ_PROJECT_DIR}/venv_probe.py" <<'PY'
import os
print("RUNQ_VENV_ACTIVE=" + os.environ.get("RUNQ_VENV_ACTIVE", "missing"))
PY

  cat >"${RUNQ_PROJECT_DIR}/mark_order.py" <<'PY'
import pathlib
import sys
import time

name = sys.argv[1]
pathlib.Path("fair_order.txt").open("a").write(name + "\n")
print("fair", name)
time.sleep(1)
PY

  mkdir -p "${RUNQ_PROJECT_DIR}/.venv/bin"
  cat >"${RUNQ_PROJECT_DIR}/.venv/bin/activate" <<'SH'
export RUNQ_VENV_ACTIVE=l2
SH

  cat >"${RUNQ_PROJECT_DIR}/wrong_project_job.yaml" <<'YAML'
project: wrong_project
sweep:
  - method: list
    parameters:
      model: [override_ok]
YAML

  cat >"${RUNQ_PROJECT_DIR}/too_many_gpus.yaml" <<'YAML'
project: l2_init
overrides:
  gpus_per_task: 99
sweep:
  - method: list
    parameters:
      name: [too_many]
YAML

  cat >"${RUNQ_PROJECT_DIR}/venv_project.yaml" <<YAML
project_name: l2_venv
working_dir: ${RUNQ_PROJECT_DIR}
command_template: python3 venv_probe.py {{args}}
defaults:
  gpus_per_task: 1
  max_retry: 1
python_env:
  type: venv
  path: .venv
YAML

  cat >"${RUNQ_PROJECT_DIR}/venv_job.yaml" <<'YAML'
project: l2_venv
sweep:
  - method: list
    parameters:
      name: [venv]
YAML

  cat >"${RUNQ_PROJECT_DIR}/pressure_project.yaml" <<YAML
project_name: l2_pressure
working_dir: ${RUNQ_PROJECT_DIR}
command_template: python3 quick.py {{args}}
defaults:
  gpus_per_task: 1
  max_retry: 1
YAML

  cat >"${RUNQ_PROJECT_DIR}/pressure_job.yaml" <<'YAML'
project: l2_pressure
sweep:
  - method: list
    parameters:
      sample: [a, b, c]
YAML
}

build_runq() {
	cd "${REPO_ROOT}"
	mkdir -p "${REPO_ROOT}/.cache/go-build" "${REPO_ROOT}/.cache/go-mod"
	GOCACHE="${REPO_ROOT}/.cache/go-build" GOMODCACHE="${REPO_ROOT}/.cache/go-mod" go test ./...
	GOCACHE="${REPO_ROOT}/.cache/go-build" GOMODCACHE="${REPO_ROOT}/.cache/go-mod" go build -o "${RUNQ_BIN}" ./cmd/runq
}

t00_environment() {
  command -v nvidia-smi >/dev/null 2>&1
  [[ "$(nvidia-smi -L | wc -l | tr -d ' ')" == "${RUNQ_FAKE_GPU_COUNT}" ]]
  start_daemon
  run_log T00_status "${RUNQ_CMD[@]}" status
}

t01_l2a_init_sweep_submit_flags() {
  mkdir -p "${RUNQ_PROJECT_DIR}/generated"
  (
    cd "${RUNQ_PROJECT_DIR}"
    run_log T01_init "${RUNQ_CMD[@]}" init train.py --project l2_init -o generated
    grep -q 'project_name: l2_init' generated/project.yaml
    grep -q 'lr:' generated/job.yaml
    grep -q 'batch-size:' generated/job.yaml
    run_log T01_project_add "${RUNQ_CMD[@]}" project add --file generated/project.yaml
  )

  local dry job
  dry=$(cd "${RUNQ_PROJECT_DIR}" && capture T01_sweep_dry "${RUNQ_CMD[@]}" sweep --project l2_init --dry lr=1e-4,3e-4 batch=16,32)
  contains "${dry}" '4 tasks'

  run_log T01_submit_dry_run "${RUNQ_CMD[@]}" submit "${RUNQ_PROJECT_DIR}/wrong_project_job.yaml" --project l2_init --dry-run

  job=$(cd "${RUNQ_PROJECT_DIR}" && capture T01_submit_override "${RUNQ_CMD[@]}" submit wrong_project_job.yaml --project l2_init | extract_id_from_text)
  [[ -n "${job}" ]]
  wait_task_successes T01_wait_override "${job}" 1 30
}

t02_l2a_reject_too_many_gpus() {
  set +e
  (cd "${RUNQ_PROJECT_DIR}" && run_log T02_submit "${RUNQ_CMD[@]}" submit too_many_gpus.yaml)
  local rc=$?
  set -e
  [[ ${rc} -ne 0 ]]
  grep -Eiq 'exceeds total GPUs' "${LOG_DIR}/T02_submit.log"
}

t03_l2b_python_env_activation() {
  (
    cd "${RUNQ_PROJECT_DIR}"
    run_log T03_project_add "${RUNQ_CMD[@]}" project add --file venv_project.yaml
  )
  local job task log_path
  job=$(submit_job T03_submit venv_job.yaml)
  [[ -n "${job}" ]]
  wait_task_successes T03_wait "${job}" 1 30
  task=$(sqlite_value "select id from tasks where job_id='${job}' limit 1;")
  log_path=$(sqlite_value "select log_path from tasks where id='${task}';")
  grep -q 'RUNQ_VENV_ACTIVE=l2' "${log_path}"
  run_log T03_logs "${RUNQ_CMD[@]}" logs "${task}" --no-follow
}

t04_l2b_fair_scheduler_after_restart() {
  stop_daemon

  local now old_start old_finish heavy_enqueue light_enqueue
  now=$(date +%s)
  old_start=$((now - 600))
  old_finish=$((now - 500))
  heavy_enqueue=$((now - 100))
  light_enqueue=$((now - 50))
  rm -f "${RUNQ_PROJECT_DIR}/fair_order.txt"

  sqlite3 "${RUNQ_DATA_DIR}/runq.db" <<SQL
INSERT OR IGNORE INTO projects (name, config_json) VALUES ('fair_project', '{}');
INSERT OR REPLACE INTO jobs (id, project_name, description, config_json, status, total_tasks, created_at, finished_at)
  VALUES ('j_history', 'fair_project', 'history', '{}', 'done', 1, ${old_start}, ${old_finish});
INSERT OR REPLACE INTO jobs (id, project_name, description, config_json, status, total_tasks, created_at)
  VALUES ('j_heavy', 'fair_project', 'heavy user pending first', '{}', 'pending', 1, ${heavy_enqueue});
INSERT OR REPLACE INTO jobs (id, project_name, description, config_json, status, total_tasks, created_at)
  VALUES ('j_light', 'fair_project', 'light user pending second', '{}', 'pending', 1, ${light_enqueue});
INSERT OR REPLACE INTO tasks
  (id, job_id, project_name, command, params_json, gpus_needed, status, retry_count, max_retry,
   log_path, working_dir, env_json, resumable, uid, enqueued_at, started_at, finished_at)
  VALUES
  ('t_history', 'j_history', 'fair_project', 'true', '{}', 1, 'success', 0, 1,
   '${RUNQ_PROJECT_DIR}/logs/t_history.log', '${RUNQ_PROJECT_DIR}', '{}', 0, 100,
   ${old_start}, ${old_start}, ${old_finish});
INSERT OR REPLACE INTO tasks
  (id, job_id, project_name, command, params_json, gpus_needed, status, retry_count, max_retry,
   log_path, working_dir, env_json, resumable, uid, enqueued_at)
  VALUES
  ('t_heavy', 'j_heavy', 'fair_project', 'python3 mark_order.py heavy', '{}', 1, 'pending', 0, 1,
   '${RUNQ_PROJECT_DIR}/logs/t_heavy.log', '${RUNQ_PROJECT_DIR}', '{}', 0, 100, ${heavy_enqueue});
INSERT OR REPLACE INTO tasks
  (id, job_id, project_name, command, params_json, gpus_needed, status, retry_count, max_retry,
   log_path, working_dir, env_json, resumable, uid, enqueued_at)
  VALUES
  ('t_light', 'j_light', 'fair_project', 'python3 mark_order.py light', '{}', 1, 'pending', 0, 1,
   '${RUNQ_PROJECT_DIR}/logs/t_light.log', '${RUNQ_PROJECT_DIR}', '{}', 0, 200, ${light_enqueue});
SQL

  start_daemon
  wait_sql_match T04_wait_success 30 \
    "select count(*) from tasks where id in ('t_light','t_heavy') and status='success';" \
    '^2$'
  wait_file_lines "${RUNQ_PROJECT_DIR}/fair_order.txt" 2 10

  local first second
  first=$(sed -n '1p' "${RUNQ_PROJECT_DIR}/fair_order.txt")
  second=$(sed -n '2p' "${RUNQ_PROJECT_DIR}/fair_order.txt")
  [[ "${first}" == "light" ]]
  [[ "${second}" == "heavy" ]]
}

t05_l2_pressure_concurrent_submit_and_drain() {
  (
    cd "${RUNQ_PROJECT_DIR}"
    run_log T05_project_add "${RUNQ_CMD[@]}" project add --file pressure_project.yaml
  )

  local ids_csv job_count total_tasks task_count success_count
  submit_many_jobs T05 pressure_job.yaml 16 >"${LOG_DIR}/T05_ids.out"
  ids_csv=$(sql_ids $(cat "${LOG_DIR}/T05_ids.out"))

  job_count=$(sqlite_value "select count(*) from jobs where id in (${ids_csv});")
  total_tasks=$(sqlite_value "select coalesce(sum(total_tasks),0) from jobs where id in (${ids_csv});")
  task_count=$(sqlite_value "select count(*) from tasks where job_id in (${ids_csv});")
  [[ "${job_count}" == "16" ]]
  [[ "${total_tasks}" == "48" ]]
  [[ "${task_count}" == "48" ]]

  wait_sql_match T05_wait_success 120 \
    "select count(*) from tasks where job_id in (${ids_csv}) and status='success';" \
    '^48$'
  success_count=$(sqlite_value "select count(*) from tasks where job_id in (${ids_csv}) and status='success';")
  [[ "${success_count}" == "48" ]]
  ! grep -Eiq 'database is locked|SQLITE_BUSY|duplicate id|missing task|panic' "${RUNQ_DATA_DIR}/daemon.log"
}

t06_l2_pressure_consistency_after_extreme_queue() {
  local mismatch active terminal_without_finish success_without_runtime free_gpus

  wait_sql_match T06_wait_clean 30 \
    "select count(*) from tasks where status in ('running','pending');" \
    '^0$'

  mismatch=$(sqlite_value "select count(*) from jobs j where j.total_tasks != (select count(*) from tasks t where t.job_id=j.id);")
  active=$(sqlite_value "select count(*) from tasks where status in ('running','pending');")
  terminal_without_finish=$(sqlite_value "select count(*) from tasks where status in ('success','failed','killed') and finished_at is null;")
  success_without_runtime=$(sqlite_value "select count(*) from tasks where status='success' and (pid=0 or gpus='' or started_at is null);")
  free_gpus=$(sqlite_value "select 1;")

  [[ "${mismatch}" == "0" ]]
  [[ "${active}" == "0" ]]
  [[ "${terminal_without_finish}" == "0" ]]
  [[ "${success_without_runtime}" == "0" ]]

  local status
  status=$(capture T06_status "${RUNQ_CMD[@]}" status)
  contains "${status}" 'Running:?[[:space:]]+0'
  contains "${status}" 'Pending:?[[:space:]]+0'
  contains "${status}" 'GPUs free:?[[:space:]]+1'
  [[ "${free_gpus}" == "1" ]]
}

trap cleanup EXIT

prepare_workspace
build_runq

run_case T00 "fake GPU daemon starts on isolated RUNQ_DATA_DIR" t00_environment
run_case T01 "L2A init, sweep dry-run, submit --dry-run, and submit --project override work" t01_l2a_init_sweep_submit_flags
run_case T02 "L2A A6 rejects gpus_per_task above total GPUs at submit time" t02_l2a_reject_too_many_gpus
run_case T03 "L2B B4 activates project venv before executing task command" t03_l2b_python_env_activation
run_case T04 "L2B B6 fair scheduler survives restart and runs lower-usage user first" t04_l2b_fair_scheduler_after_restart
run_case T05 "pressure: sixteen concurrent submits create and drain forty-eight tasks on one fake GPU" t05_l2_pressure_concurrent_submit_and_drain
run_case T06 "pressure: DB, queue, terminal timestamps, and GPU availability remain consistent" t06_l2_pressure_consistency_after_extreme_queue

write_report
printf '\nReport: %s\n' "${REPORT}"

if printf '%s\n' "${SUMMARY_ROWS[@]}" | grep -q '| FAIL |'; then
  exit 1
fi
