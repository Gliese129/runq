# Repository split: runq-lab and runq-executor

Status: code and protocol separation implemented for the independent
`runq-lab` and `runq-executor` repositories (2026-08-31)

## Current result

The split is represented by two independent Go modules:

- The current repository is named and imported as `github.com/gliese129/runq-lab`. Its product command remains `runq`.
- `runq-executor` is a standalone `github.com/gliese129/runq-executor` module. Its shipped command remains `runqd`, with its own API, CLI adapter, daemon assembly, execution engine, model, paths, process control, resource pool, service, store, installer, and release workflow.
- Neither module imports the other. Their intended boundary is protocol and command compatibility, not shared Go packages or a third common module.
- runq-lab's daemon assembly is client/dashboard-oriented and does not construct the runqd execution engine.
- runqd has an independent SQLite execution ledger and independent default data directory.

The mixed implementation has been pruned from runq-lab: `cmd/runqd`, the
machine HTTP server, local backend, and in-process executor are gone. The
standalone daemon source lives in
[`runq-executor`](https://github.com/Gliese129/runq-executor).

## Authority boundary

The separation follows authority over transitions, not command-directory names.

| Area | runq-lab | runqd |
|---|---|---|
| Product role | Experiment coordinator and user-facing lab | One machine's execution daemon |
| Commands | `runq`: projects, jobs, tasks, targets, HPC, dashboard | `runqd`: serve, submit, list, cancel, GPU, health, capabilities |
| Planning | Sweep expansion, preflight, workspace/wrapper generation, retry policy | Validate and admit one immutable execution-attempt specification |
| Routing | Local runqd target plus SSH/HPC schedulers and target generations | No SSH/HPC routing or scheduler templates |
| Runtime | Client/dashboard process and target reconciliation | Queue, GPU allocation, gated spawn, PID supervision, signals, timeout, recovery |
| Persistence | Experiment jobs/tasks, projections, metrics, results, checkpoints | Execution jobs/attempts, process identity, ownership, terminal reason |
| UI/SDK | Dashboard and Python SDK | No dashboard, analytics, or experiment SDK |
| Release | `runq`, dashboard build, SDK, runq installer | Linux `runqd`, checksums, runqd installer |

runq-lab owns policy and user-visible projections. runqd owns machine-local admission and execution truth. A request accepted by runq-lab is intent; a runqd attempt record and its observations are execution evidence.

## Implemented runqd boundary

The daemon has these repository-local components:

- `cmd/runqd`: foreground server and machine-control commands.
- `internal/api`: versioned HTTP over a Unix socket plus temporary migration aliases.
- `internal/service`: publication boundary between committed ledger transitions and runtime wake/signal effects.
- `internal/store`: runqd-only schema with atomic, idempotent attempt admission.
- `internal/engine`: pending/starting/running/unknown/terminal flow, gated process start, timeout, cancellation, freeze/thaw, and restart monitoring.
- `internal/resource` and `internal/process`: machine capacity and process-group control.
- `internal/paths`: runqd-owned data, database, socket, PID, and log paths.

The canonical machine protocol is `/api/v1` and publishes protocol version `1` through `X-Runqd-Protocol`. Explicit unsupported versions receive HTTP 426. Temporary `/api/*` aliases support migration from the mixed repository.

The runqd CLI is also a compatibility adapter for current runq-lab scheduler templates:

- `RUNQ_SUBMIT_HANDLE` is accepted as the immutable attempt ID when `--attempt-id` is absent.
- `--name` and `--project` are accepted as controller metadata but do not become execution authority.
- submit receipts remain parse-stable as `submitted <attempt_id>` for creation and idempotent replay.
- list output maps internal `starting` to `PENDING` and `canceled` to `KILLED` for the current parser.

The standalone `runq-executor` repository contains its smaller transition
model at `docs/dfa.md`.

## Persistence and paths

runq-lab and runqd use separate ledgers and migration streams:

| Repository | User data default | Root data default | Database |
|---|---|---|---|
| runq-lab | `~/.local/share/runq` | `/var/lib/runq` | `runq.db` |
| runqd | `~/.local/share/runqd` | `/var/lib/runqd` | `runqd.db` |

runqd accepts `RUNQD_DATA_DIR` for its complete data root and `RUNQD_SOCKET` for the socket alone. runq-lab uses the same precedence and root/user defaults when connecting to the runqd endpoint; it never derives the machine socket from `RUNQ_DATA_DIR`. The client runtime does not locate, launch, supervise, log, or own the runqd process. `runq doctor` may report whether an independently installed binary/socket exists, but diagnostics perform no lifecycle action.

No schema package is shared. Cross-repository compatibility belongs in protocol fixtures and integration tests, not Go imports.

## Installer and release separation

The runq-lab installer downloads and verifies only a `runq` or `runq-dashboard` asset, stages it in the destination directory, and atomically replaces the client binary. Starting/restarting the runq client daemon is opt-in through `RUNQ_START_DAEMON`; it does not install or control runqd.

The `runq-executor` repository has its own Linux installer. It downloads the architecture-specific tarball and `checksums.txt`, verifies SHA-256 before replacement, installs only `runqd`, and never starts or restarts the daemon. Its release workflow builds Linux amd64/arm64 archives independently.

The release streams are independent:

1. Release a compatible runqd first.
2. Exercise protocol and CLI-adapter compatibility against runq-lab.
3. Release runq-lab with the supported runqd protocol/version range.

## DFA-driven split decisions

The transition audit changed the boundary in concrete ways:

- Attempt identity is immutable and idempotency is scoped to an attempt, not a reusable business task ID.
- Remote submit uses durable `submitting` intent before the external effect; recovery projects an interrupted submit to `unknown`, never directly to launchable pending.
- `unknown` is active and retains ownership.
- runqd uses `starting` as a durable gated-spawn phase. User code cannot run until PID and GPU ownership are durable.
- Terminal persistence precedes resource release and is retried while capacity remains owned.
- Cancellation intent and timeout reason are persisted before process signals and remain distinct.
- Freeze/thaw reports signal failures and compensates a failed persistence step.
- Job state in runq-lab is reduced by one `store.ProjectJobStatus` function rather than separate local/remote/scheduler copies.

The full projection, implemented resolutions, and remaining findings are in [dfa-ir.md](dfa-ir.md).

## Release gates

The code split is complete. Each release requires:

1. Add compatibility-window fixtures when supporting older released runqd protocol versions; current/current socket integration is covered now.
2. Verify release artifacts from clean checkouts in the hosted CI environment.
3. Keep an explicit license file in each repository.

## Documentation policy

`README.md` and versioned files under `docs/` are the source of truth for installation, protocol, CLI/configuration, HPC integration, SDK behavior, troubleshooting, and release compatibility. A hosted wiki may link to these files for onboarding, but it should not duplicate version-sensitive facts.

Poster redesign is outside this split. Keep the existing `assets/poster-en.png` unchanged until a replacement is supplied.

## Publication checklist

1. Preserve a baseline tag/commit for the mixed implementation.
2. Commit the explicit runq-lab boundary change and the independent
   `runq-executor` root separately.
3. Configure repository ownership, visibility, branch protection, release permissions, and secrets explicitly.
4. Publish `runq-executor`, run compatibility tests, then publish runq-lab.

The filesystem move is routine. Semantic safety comes from the tested protocol and transition boundary.
