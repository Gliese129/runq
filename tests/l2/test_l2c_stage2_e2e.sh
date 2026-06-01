#!/usr/bin/env bash
#
# Stage 2 (Python SDK) end-to-end test.
#
# This test treats runq as a black box: it builds the CLI binary, starts a
# daemon, registers a temporary project, submits one daemon-managed task, thaws
# the SDK-triggered low-disk freeze, and verifies the on-disk plus DB contract.

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/../.." && pwd)
SDK_PYTHON_PKG="${REPO_ROOT}/sdk/python"
TRAINING_SCRIPT="${SCRIPT_DIR}/training_script.py"

export RUNQ_L2C_STAGE2_RUN_ID=${RUNQ_L2C_STAGE2_RUN_ID:-$(date '+%Y%m%d-%H%M%S')}
export RUNQ_L2C_STAGE2_BASE=${RUNQ_L2C_STAGE2_BASE:-"/private/tmp/runq-l2c-stage2-${USER:-user}"}
export RUNQ_L2C_STAGE2_ROOT=${RUNQ_L2C_STAGE2_ROOT:-"${RUNQ_L2C_STAGE2_BASE}/${RUNQ_L2C_STAGE2_RUN_ID}"}
export RUNQ_DATA_DIR=${RUNQ_DATA_DIR:-"${RUNQ_L2C_STAGE2_ROOT}/data"}
export RUNQ_SOCKET=${RUNQ_SOCKET:-"${RUNQ_DATA_DIR}/runq.sock"}
export RUNQ_BIN=${RUNQ_BIN:-"${RUNQ_L2C_STAGE2_ROOT}/runq"}
export RUNQ_PROJECT_DIR=${RUNQ_PROJECT_DIR:-"${RUNQ_L2C_STAGE2_ROOT}/project"}
export RUNQ_FAKE_GPU_COUNT=${RUNQ_FAKE_GPU_COUNT:-4}
export GOCACHE=${GOCACHE:-"${REPO_ROOT}/.cache/go-build"}
export GOMODCACHE=${GOMODCACHE:-"${REPO_ROOT}/.cache/go-mod"}

FAKEBIN="${RUNQ_L2C_STAGE2_ROOT}/fakebin"
LOG_DIR="${RUNQ_L2C_STAGE2_ROOT}/case-logs"
REPORT="${RUNQ_L2C_STAGE2_ROOT}/report.md"
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

extract_id_from_text() {
  grep -Eo 'id=[^ ]+' | head -n 1 | cut -d= -f2
}

sql_quote() {
  printf "'%s'" "${1//\'/\'\'}"
}

sqlite_value() {
  sqlite3 -noheader -list -cmd ".timeout 5000" "${DB_PATH}" "$1"
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
    sleep 0.25
  done
}

task_field() {
  local task_id=$1
  local field=$2
  sqlite_value "select ${field} from tasks where id=$(sql_quote "${task_id}");"
}

task_ids_for_job() {
  local job_id=$1
  sqlite_value "select id from tasks where job_id=$(sql_quote "${job_id}") order by enqueued_at, id;"
}

stop_daemon() {
  set +e
  "${RUNQ_CMD[@]}" daemon stop >/dev/null 2>&1
  set -e
}

write_report() {
  mkdir -p "${RUNQ_L2C_STAGE2_ROOT}"
  {
    printf '# runq L2-C Stage 2 E2E Test Report\n\n'
    printf '| Case | Result | Notes |\n|---|---|---|\n'
    if (( ${#SUMMARY_ROWS[@]} > 0 )); then
      for row in "${SUMMARY_ROWS[@]}"; do
        printf '%s\n' "${row}"
      done
    fi
    printf '\n## Failures\n\n'
    if [[ ${#FAILURES[@]} -eq 0 ]]; then
      printf 'None.\n'
    else
      for failure in "${FAILURES[@]}"; do
        printf '%s\n' "${failure}"
      done
    fi
    printf '\n## Artifacts\n\n'
    printf -- '- RUNQ_L2C_STAGE2_ROOT: `%s`\n' "${RUNQ_L2C_STAGE2_ROOT}"
    printf -- '- RUNQ_DATA_DIR: `%s`\n' "${RUNQ_DATA_DIR}"
    printf -- '- RUNQ_SOCKET: `%s`\n' "${RUNQ_SOCKET}"
    printf -- '- RUNQ_PROJECT_DIR: `%s`\n' "${RUNQ_PROJECT_DIR}"
    printf -- '- Case logs: `%s`\n' "${LOG_DIR}"
  } >"${REPORT}"
}

cleanup() {
  stop_daemon || true
  write_report || true
}

prepare_workspace() {
  case "${RUNQ_L2C_STAGE2_ROOT}" in
    ""|"/"|"/tmp"|"/private/tmp"|"/home"|"$HOME"|"$REPO_ROOT"|"$RUNQ_L2C_STAGE2_BASE")
      printf 'refusing unsafe RUNQ_L2C_STAGE2_ROOT: %s\n' "${RUNQ_L2C_STAGE2_ROOT}" >&2
      return 1
      ;;
  esac

  command -v go >/dev/null 2>&1
  command -v sqlite3 >/dev/null 2>&1
  command -v python3 >/dev/null 2>&1
  command -v df >/dev/null 2>&1
  [[ -f "${TRAINING_SCRIPT}" ]]

  PYTHONPATH="${SDK_PYTHON_PKG}${PYTHONPATH:+:${PYTHONPATH}}" python3 -c "import runq"

  rm -rf "${RUNQ_L2C_STAGE2_ROOT}"
  mkdir -p "${RUNQ_DATA_DIR}" "${RUNQ_PROJECT_DIR}" "${LOG_DIR}" "${FAKEBIN}"

  cat >"${FAKEBIN}/nvidia-smi" <<'FAKE_NVIDIA_SMI'
#!/usr/bin/env bash
set -euo pipefail

gpu_count=${RUNQ_FAKE_GPU_COUNT:-4}

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
project_name: l2c_stage2
working_dir: ${RUNQ_PROJECT_DIR}
command_template: 'PYTHONPATH="${SDK_PYTHON_PKG}\${PYTHONPATH:+:\$PYTHONPATH}" RUNQ_E2E_STARTED_MARKER="\$RUNQ_TASK_DIR/started" RUNQ_E2E_FINISHED_MARKER="\$RUNQ_TASK_DIR/finished" python3 ${TRAINING_SCRIPT} # {{args}}'
defaults:
  gpus_per_task: 1
  max_retry: 0
YAML

  cat >"${RUNQ_PROJECT_DIR}/sdk_job.yaml" <<'YAML'
project: l2c_stage2
description: SDK freeze-self thaw round trip
sweep:
  - method: list
    parameters:
      sample: [sdk]
YAML

  grep -Fq '{{args}}' "${RUNQ_PROJECT_DIR}/project.yaml"
  grep -Fq '$RUNQ_TASK_DIR' "${RUNQ_PROJECT_DIR}/project.yaml"
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

wait_running_pid() {
  local name=$1
  local job_id=$2
  local timeout_seconds=$3
  local start task pid

  start=$(date +%s)
  mkdir -p "${LOG_DIR}"
  while true; do
    task=$(sqlite_value \
      "select id from tasks where job_id=$(sql_quote "${job_id}") and status='running' and pid is not null and pid != 0 limit 1;" \
      2>"${LOG_DIR}/${name}.log" || true)
    if [[ -n "${task}" ]]; then
      pid=$(task_field "${task}" pid)
      printf '%s %s\n' "${task}" "${pid}"
      return 0
    fi
    if (( "$(date +%s)" - start >= timeout_seconds )); then
      return 1
    fi
    sleep 0.25
  done
}

wait_for_file() {
  local path=$1
  local timeout_seconds=$2
  local start

  start=$(date +%s)
  while [[ ! -f "${path}" ]]; do
    if (( "$(date +%s)" - start >= timeout_seconds )); then
      printf 'timed out waiting for %s\n' "${path}" >&2
      return 1
    fi
    sleep 0.25
  done
}

wait_pid_state() {
  local pid=$1
  local expected_prefix=$2
  local timeout_seconds=$3
  local start state saw_state

  start=$(date +%s)
  saw_state=0
  while true; do
    state=$(ps -o state= -p "${pid}" 2>/dev/null | tr -d ' ' || true)
    [[ -n "${state}" ]] && saw_state=1
    [[ "${state}" == "${expected_prefix}"* ]] && return 0
    if (( "$(date +%s)" - start >= timeout_seconds )); then
      if [[ "${saw_state}" == "0" ]]; then
        log "ps state unavailable for pid ${pid}; skipping OS-state assertion"
        return 0
      fi
      printf 'pid %s state never matched %s* (last=%s)\n' \
        "${pid}" "${expected_prefix}" "${state}" >&2
      return 1
    fi
    sleep 0.1
  done
}

wait_task_status() {
  local task_id=$1
  local expected=$2
  local timeout_seconds=$3

  wait_sql_match "wait_task_${task_id}_${expected}" "${timeout_seconds}" \
    "select status from tasks where id=$(sql_quote "${task_id}");" \
    "^${expected}$"
}

assert_jsonl_contract() {
  local jsonl=$1
  JSONL_PATH="${jsonl}" python3 - <<'PY'
import json
import os
import pathlib

path = pathlib.Path(os.environ["JSONL_PATH"])
events = [json.loads(line) for line in path.read_text().splitlines() if line.strip()]
metrics = [event for event in events if event.get("type") == "metric"]
checkpoints = [event for event in events if event.get("type") == "checkpoint"]

if len(metrics) != 2:
    raise SystemExit(f"expected 2 metric events, got {len(metrics)}: {events}")
if len(checkpoints) != 1:
    raise SystemExit(f"expected 1 checkpoint event, got {len(checkpoints)}: {events}")

def metric_value(event, key):
    if event.get("key") == key:
        return event.get("value")
    values = event.get("metrics")
    if isinstance(values, dict):
        return values.get(key)
    values = event.get("values")
    if isinstance(values, dict):
        return values.get(key)
    return None

expected_metrics = [(0, 1.0), (1, 0.5)]
for event, (step, loss) in zip(metrics, expected_metrics):
    if event.get("step") != step or metric_value(event, "loss") != loss:
        raise SystemExit(
            f"expected metric loss={loss} step={step}, got {event}"
        )

checkpoint = checkpoints[0]
if checkpoint.get("step") != 0:
    raise SystemExit(f"expected checkpoint step=0, got {checkpoint}")
ckpt_path = str(checkpoint.get("path") or "")
if pathlib.PurePosixPath(ckpt_path).name != "ckpt.pt":
    raise SystemExit(f"expected checkpoint path to point at ckpt.pt, got {checkpoint}")
PY
}

assert_manifest_contract() {
  local manifest=$1
  MANIFEST_PATH="${manifest}" python3 - <<'PY'
import json
import os
import pathlib

path = pathlib.Path(os.environ["MANIFEST_PATH"])
data = json.loads(path.read_text())
entries = data.get("entries")
if data.get("version") != 1:
    raise SystemExit(f"expected version=1, got {data}")
if not isinstance(entries, list) or len(entries) != 1:
    raise SystemExit(f"expected 1 manifest entry, got {data}")
entry = entries[0]
if entry.get("path") != "ckpt.pt":
    raise SystemExit(f"expected path=ckpt.pt, got {entry}")
if entry.get("step") != 0:
    raise SystemExit(f"expected step=0, got {entry}")
if entry.get("is_best") is not False:
    raise SystemExit(f"expected is_best=false, got {entry}")
PY
}

assert_no_runq_tmp_files() {
  local task_dir=$1
  local leftovers

  leftovers=$(find "${task_dir}" -name '.runq-tmp-*' -print)
  if [[ -n "${leftovers}" ]]; then
    printf 'found leftover .runq-tmp-* files:\n%s\n' "${leftovers}" >&2
    return 1
  fi
}

t10_sdk_freeze_thaw_roundtrip() {
  local job task_count pair task pid task_dir out calls
  local jsonl manifest metrics_rows checkpoint_rows

  (
    cd "${RUNQ_PROJECT_DIR}"
    run_log T10_project_add "${RUNQ_CMD[@]}" project add --file project.yaml
  )

  job=$(submit_job T10_submit sdk_job.yaml)
  [[ -n "${job}" ]] || { echo "submit returned no job id" >&2; return 1; }

  wait_sql_match T10_one_task 20 \
    "select count(*) from tasks where job_id=$(sql_quote "${job}");" '^1$'
  task_count=$(sqlite_value "select count(*) from tasks where job_id=$(sql_quote "${job}");")
  [[ "${task_count}" == "1" ]]

  pair=$(wait_running_pid T10_wait_running "${job}" 30)
  task=$(awk '{print $1}' <<<"${pair}")
  pid=$(awk '{print $2}' <<<"${pair}")
  [[ -n "${task}" && -n "${pid}" ]]
  log "task=${task} pid=${pid}"

  task_dir=$(task_field "${task}" task_dir)
  [[ -d "${task_dir}" ]] || { echo "no task_dir for ${task}" >&2; return 1; }
  [[ "${task_dir}" == "${RUNQ_PROJECT_DIR}/.runq/${job}/${task}" ]]

  wait_for_file "${task_dir}/started" 20
  grep -Fq "task_id=${task}" "${task_dir}/started"

  wait_pid_state "${pid}" T 10

  out=$(capture T10_thaw "${RUNQ_CMD[@]}" thaw)
  grep -Fq "thawed ${task}" <<<"${out}"

  wait_for_file "${task_dir}/finished" 30
  grep -Fq "task_id=${task}" "${task_dir}/finished"
  calls=$(grep -Eo 'disk_calls=[0-9]+' "${task_dir}/finished" | cut -d= -f2)
  (( calls >= 2 )) || {
    printf 'disk_usage was called %s times; expected >= 2\n' "${calls}" >&2
    return 1
  }

  wait_task_status "${task}" "success" 20

  jsonl="${task_dir}/metrics.jsonl"
  manifest="${task_dir}/checkpoints/.runq_manifest.json"
  [[ -f "${jsonl}" ]] || { echo "missing metrics jsonl: ${jsonl}" >&2; return 1; }
  [[ -f "${task_dir}/checkpoints/ckpt.pt" ]] || {
    echo "missing checkpoint file: ${task_dir}/checkpoints/ckpt.pt" >&2
    return 1
  }
  [[ -f "${manifest}" ]] || { echo "missing manifest: ${manifest}" >&2; return 1; }

  assert_jsonl_contract "${jsonl}"
  assert_manifest_contract "${manifest}"
  assert_no_runq_tmp_files "${task_dir}"

  wait_sql_match T10_metrics_reaped 20 \
    "select count(*) from metrics where task_id=$(sql_quote "${task}");" '^2$'
  wait_sql_match T10_checkpoints_reaped 20 \
    "select count(*) from checkpoints where task_id=$(sql_quote "${task}");" '^1$'

  metrics_rows=$(sqlite_value \
    "select group_concat(row, ',') from (select key || ':' || step || ':' || value as row from metrics where task_id=$(sql_quote "${task}") order by step);")
  checkpoint_rows=$(sqlite_value \
    "select group_concat(row, ',') from (select path || ':' || step || ':' || is_best as row from checkpoints where task_id=$(sql_quote "${task}") order by step);")
  [[ "${metrics_rows}" == "loss:0:1.0,loss:1:0.5" ]]
  [[ "${checkpoint_rows}" == *"ckpt.pt:0:0"* ]]
}

trap cleanup EXIT

prepare_workspace
build_runq
start_daemon

log "L2-C stage2 root: ${RUNQ_L2C_STAGE2_ROOT}"
log "RUNQ_DATA_DIR: ${RUNQ_DATA_DIR}"
log "RUNQ_SOCKET: ${RUNQ_SOCKET}"
log "fake GPUs: ${RUNQ_FAKE_GPU_COUNT}"

run_case T10 "SDK daemon-mode task freezes on first safe_save, runq thaw resumes it, artifacts and DB rows match" t10_sdk_freeze_thaw_roundtrip

write_report
printf '\nReport: %s\n' "${REPORT}"

if (( ${#SUMMARY_ROWS[@]} > 0 )) && printf '%s\n' "${SUMMARY_ROWS[@]}" | grep -q '| FAIL |'; then
  exit 1
fi
