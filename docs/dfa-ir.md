# DFA projection for workflow auditing

Status: implemented audit snapshot for the runq-lab/runqd split (2026-08-28)

This document projects the business flow onto a deliberately abstract finite-state model. It is not a runtime state-machine framework, an XState-style machine, or a bijection with Go types and database rows. Several implementation states may map to one abstract state, while pause, freeze, cancellation intent, evidence strength, and resource ownership remain orthogonal.

The purpose is narrower: make the transition function `t` explicit enough to find duplicated reducers, illegal transitions, missing error paths, and unsafe effect order.

## Audit model

```text
t       : Q × Σ → Q
effects : Q × Σ → ordered effects | REJECT(reason)
```

An input symbol includes the resolved outcome of an operation. For example, remote submission produces `REMOTE_SUBMIT_ACCEPTED`, `REMOTE_SUBMIT_PRESTART_ERROR`, `REMOTE_SUBMIT_AMBIGUOUS`, or `REMOTE_SUBMIT_REJECTED`. Including the outcome keeps `t` deterministic without pretending that SSH, process signals, or storage are deterministic.

Every meaningful state/event pair not listed below is a side-effect-free rejection:

```text
t(q, e)       = q
effects(q, e) = REJECT(illegal_transition)
```

An impossible product state rejects with `corrupt_state`; it must not be projected to terminal merely because a switch statement has a default branch.

## Abstract phases

| Symbol | Phase | Meaning |
|---|---|---|
| `Ø` | Unaccepted | No durable work has been admitted. |
| `R` | Ready | Durable pending work is eligible for dispatch. |
| `LD` | Local dispatching | Capacity is held and running intent is durable; process identity is not yet confirmed. |
| `LX` | Local executing | Local PID/start identity and capacity ownership are durable. |
| `RS` | Remote submitting | A fresh attempt epoch and submission intent are durable; the external ID is not yet confirmed. |
| `EQ` | External queued | The external ID is durable; the remote scheduler has not reported execution. |
| `RX` | Remote executing | The external scheduler reports execution. |
| `UL` | Local outcome unknown | A process may exist, but identity, signal result, or ownership is uncertain. |
| `UR` | Remote outcome unknown | Submission or execution may exist, but its outcome is uncertain. |
| `ST(o, source)` | Soft terminal | Terminal evidence may be corrected by explicitly stronger evidence. |
| `HT(o, source)` | Hard terminal | Authoritative terminal outcome. |

`o` is `success`, `failure`, or `user_cancel`. Timeout, daemon shutdown, transport loss, and submission rejection are reasons/sources, even where an older storage schema projects them onto the same coarse status.

The phase combines with orthogonal dimensions:

```text
gate   ∈ {open, paused}
proc   ∈ {running, stopped}
kill   ∈ {clear, requested}
budget ∈ {available, exhausted}
lease  ∈ {free, held}
owner  ∈ {active_generation, retiring_generation}
```

Attempt IDs and retry counters are registers. A manual or automatic retry creates a new attempt epoch even though the business task identity remains stable.

## Effect vocabulary and ordering

```text
P[x]  durable persistence transaction
M[x]  in-memory queue, gate, or scheduler mutation
L+/-  acquire or release execution capacity
A[x]  external effect: spawn, signal, SSH submit, scheduler cancel
O[x]  successful observation/freshness record
```

The audit requires these effect-order invariants:

1. Persist before publish: `P → M`. An API success or in-memory transition must not precede the durable transition it reports.
2. Persist intent before an external effect: `P[intent] → A`. If `A` may have happened and its result cannot be persisted, move to `UL`/`UR`; never silently restore `R`.
3. Do not release ownership before terminal durability: `P[terminal] → M[terminal] → L-`. A failed terminal write keeps the lease.
4. A process may be created before its PID is durable only behind a closed start gate. Release user code after `P[pid/start identity]`; a persistence failure kills the gated process.
5. Record `O[fresh]` only after every required read/probe and resulting persistence succeeds.
6. Freeze/thaw is compensated: `SIGSTOP → P[frozen]`, with `SIGCONT` on persistence failure; thaw uses the inverse. Failed compensation enters `UL`.
7. Recovery establishes ownership before readiness. Every live recovered attempt has its capacity reserved and exactly one monitor, or startup visibly fails.
8. Idempotent replay may return an existing attempt only when immutable ID and normalized specification match. Same ID plus different specification is a conflict.

## Primary transition function

| ID | Source × input | Target | Required effects |
|---|---|---|---|
| `A1` | `Ø × ADMISSION_ACCEPT(N>0)` | `Rⁿ` | Validate, then one `P[job + exactly N tasks]`, then `M[enqueue]`. |
| `A2` | `Ø × ADMISSION_REJECT` | `Ø` | Leave no durable residue. |
| `D0` | `R/open × TICK_BLOCKED` | `R` | None. |
| `L1` | `R/open × LOCAL_DISPATCH` | `LD` | `L+ → P[running intent] → M[running]`. |
| `L2` | `LD × SPAWN_ACCEPTED(pid)` | `LX` | `A[gated spawn] → P[pid/start identity] → A[open gate]`. |
| `L3` | `LD × SPAWN_REJECTED` | `R` or `HT(failure)` | Persist retry/terminal decision, mutate queue, then `L-`. |
| `R1` | `R/open × REMOTE_DISPATCH` | `RS` | `L+ → P[attempt epoch + submitting intent] → M[submitting]`. |
| `R2` | `RS × REMOTE_SUBMIT_ACCEPTED(id)` | `EQ` | `A[submit] → P[external ID] → M[external queued]`. |
| `R3` | `RS × REMOTE_SUBMIT_PRESTART_ERROR` | `R` | `P[pending] → M[pending] → L-`. |
| `R4` | `RS × REMOTE_SUBMIT_AMBIGUOUS` | `UR` | `P[unknown + evidence] → M[unknown]`; retain ownership. |
| `R5` | `RS × REMOTE_SUBMIT_REJECTED` | `ST(failure, submit)` | `P[failed + evidence] → M[failed] → L-`. |
| `O1` | `EQ/UR × OBSERVE_PENDING` | `EQ` | `P[pending/id] → M[pending] → O[fresh]`. |
| `O2` | `EQ/UR × OBSERVE_RUNNING` | `RX` | `P[running/id] → M[running] → O[fresh]`. |
| `O3` | `EQ/RX/UR/ST × OBSERVE_TERMINAL(o, strength)` | `ST` or `HT` | Persist/publish evidence, release ownership, then mark fresh. |
| `F1` | `LX/RX × EXECUTION_SUCCESS` | `HT/ST(success)` | `P[terminal] → M[terminal] → L-`. |
| `F2` | `LX/RX × EXECUTION_FAILURE`, budget available | `R`, new epoch | `P[pending + budget + epoch] → M[requeue] → L-`. |
| `F3` | `LX/RX × EXECUTION_FAILURE`, budget exhausted | `HT/ST(failure)` | `P[terminal] → M[terminal] → L-`. |
| `F4` | `LX × TIMEOUT` | Retry or failure, reason `timeout` | Persist timeout intent before signalling; never imply `user_cancel`. |
| `F5` | `LX × DAEMON_SHUTDOWN` | Explicit infrastructure policy | Never imply `user_cancel`. |
| `K1` | `R × USER_KILL` | `HT(user_cancel)` | `P[canceled] → M[canceled] → L-`. |
| `K2` | active attempt `× USER_KILL` | Same phase, `kill=requested` | `P[kill intent] → M[kill intent] → A[signal/cancel]`; retain ownership and replay after restart. |
| `K3` | `kill=requested × CANCEL_CONFIRMED` | `HT(user_cancel)` | `P[canceled] → M[canceled] → L-`. |
| `K4` | `kill=requested × CANCEL_ERROR` | Active or unknown | Retain intent/ownership and return the error. |
| `M1` | failed/canceled terminal `× MANUAL_RETRY` | `R`, new epoch | One `P[retry intent + epoch + task-stream unfreeze] → A[wrapper reset] → P[pending] → M[pending]`. |
| `P1` | nonterminal/open `× PAUSE` | Same phase, paused | `M[close dispatch gate] → P[paused]`; reopen the gate if persistence fails. |
| `P2` | nonterminal/paused `× RESUME` | Same phase, open | `P[derived job status] → M[open]`. |
| `V1` | `LX/running × FREEZE_OK` | `LX/stopped` | `A[SIGSTOP] → P[stopped identity]`; compensate or enter `UL`. |
| `V2` | `LX/running × FREEZE_ERROR` | Unchanged | Return error; do not claim frozen. |
| `V3` | `LX/stopped × THAW_ERROR` | Still stopped | Retain stopped ownership and return error. |
| `C1` | durable local running `× RECOVER_ALIVE_IDENTIFIED` | `LX` | Validate identity, reserve capacity, register exactly one monitor. |
| `C2` | durable local running `× RECOVER_DEAD` | `R` or terminal | Persist decision, restore queue if applicable, recompute job projection. |
| `C3` | durable remote nonterminal `× RECOVER` | `UR`, `EQ/RX`, or safe `R` | Repair every durable row first, then restore ownership and the pause gate; fail readiness on any repair error. |
| `G1` | nonterminal `× GENERATION_REPLACED` | Same phase, retiring owner | Persist retirement before routing new work; retain old lane until terminal. |
| `G2` | settled generation `× RETIREMENT_COMPLETE` | Historical owner | Stop scheduling/sensors, retain the generation's original filesystem endpoint for artifacts, and exclude it from live control/refresh fanout. |
| `Z1` | `HT × CLEAN` | `Ø` | Delete authoritative terminal data only. |
| `Z2` | nonterminal or `ST × CLEAN` | Unchanged | `REJECT(task_live_or_correctable)`. |

## State and projection invariants

1. `job.total_tasks` equals the number of durably admitted task rows.
2. `unknown` and `submitting` are active and retain ownership until resolved.
3. Exactly one attempt epoch is current; every launchable retry receives a fresh epoch.
4. No remote submission occurs without durable `RS` intent for that epoch.
5. Every active/unknown attempt has exactly one owner and, where applicable, one lease.
6. Hard terminal evidence is absorbing except for an explicit legal manual retry.
7. Soft terminal evidence is corrected only by strictly stronger evidence from a closed precedence relation.
8. Pause is a dispatch overlay; freeze is process control. Neither is a task phase.
9. Timeout, infrastructure shutdown, and user cancellation remain distinguishable.
10. Job status is one pure projection of task rows plus the pause overlay.
11. SDK metrics, results, and event files are task-lifetime append streams. Retry unfreezes their existing ingest offsets; it never rewinds them or erases already-projected history.
12. A task control or artifact operation resolves one generation and stays linearized to that topology through its effect. Point operations re-check ownership under the attempt lock; long-lived followers retain a transport lease until close.

The shared runq-lab implementation of the last rule is `store.ProjectJobStatus`; invalid task states return an error rather than falling through to a terminal projection.

## Transition audit results

| Finding from `t` | Resolution and evidence |
|---|---|
| Manual remote retry reused a terminal attempt handle and could race retry, recovery, or retiring-lane evidence. | `Store.BeginTaskRetry` atomically records a new epoch, generation, retry intent, and task-stream unfreeze before wrapper mutation. Interactive retry, retry-intent cancellation, and recovery use the same cross-generation task lock; durable status updates use complete attempt CAS predicates. An interrupted reset resumes the same epoch. Every no-ID `submitting` phase rejects wrapper/stream observations until handoff is classified, closing the dispatch-before-wrapper-reset window. Covered by `TestSSHManualRetryLaunchesFreshAttemptHandle`, `TestSSHManualRetryIsSerializedAndConsumesOneEpoch`, `TestSSHManualRetryResumesDurableResetIntent`, `TestSSHRestoreRetryResetWaitsForSharedTaskAttemptLock`, `TestTaskStatusAttemptFencesPreserveManualRetry`, `TestPersistDecisionDoesNotOverwriteManualRetryEpoch`, `TestPreHandoffSubmittingSkipsAllIngestionVariants`, and `TestAttemptHandleIncludesDurableAttemptEpoch`. |
| Retry deleted SQL metrics/results even though the SDK appends later attempts to the same raw files, and a prior attempt's pyramid could answer while the new attempt was still appending. | SDK streams now have one explicit task-lifetime model: retry preserves projections, offsets, and dropped-count bookkeeping while clearing only terminal freeze bits. Background ingestion reloads the current attempt under the shared task lock, chart reads are pure lock-protected file reads, and only a normal wrapper-completed success/failure may use the freshly rebuilt pyramid. Covered by `TestBeginTaskRetryPreservesTaskLifetimeIngest`, `TestRetryUnfreezeContinuesResultsAtExistingOffset`, `TestSSHTaskMetricsReadIsPure`, and `TestSSHTaskMetricBucketsIgnorePriorAttemptPyramid`. |
| Remote submission had no durable pre-effect state. | `scheduler.dispatch` persists `submitting` before invoking the launcher. Restore converts durable in-flight submission to `unknown`, retains the slot, and never relaunches. Covered by `TestRemoteDispatchPersistsSubmittingBeforeExternalEffect`, `TestSubmittingPersistenceFailurePreventsExternalLaunch`, and `TestSSHRestoreNeverResubmitsDurableSubmittingAttempt`. |
| runq-lab dropped task timeouts at the runqd handoff. | The runqd preset requires `{{timeout}}`, rendering preserves zero as unlimited, and submission passes `--timeout`. Covered by `TestRunqPresetIncludesTimeoutPlaceholder` and `TestRunqdSubmitTemplatePropagatesTimeout`. |
| `unknown` could be treated as terminal/cleanable. | `store.ActiveStatuses` includes `submitting` and `unknown`; exact cleanup retains unknown rows. Covered by `TestPerformCleanExactSelectionKeepsUnknownTask`. |
| Memory could advance after failed persistence. | Unknown publication, requeue, retry, PID publication, terminal completion, pause/resume, and reconciliation use persistence-first paths. Fault tests include `TestMarkUnknownPersistenceFailureLeavesQueueRunning`, `TestAutomaticRetryPersistenceFailureLeavesQueueRunning`, `TestTransientRequeuePersistenceFailureRetainsSubmittingOwnership`, `TestTerminalPersistenceFailureRetainsSubmissionSlot`, runqd's `TestTerminalPersistenceFailureRetainsResourceCapacity`, and `TestUnknownResumePublishesOnlyAfterPersistence`. |
| PID persistence failure allowed execution to continue. | The original local path stopped the process and did not publish identity; standalone runqd now releases gated user code only after `MarkRunning`. Covered by the historical runq-lab fault test and runqd's `TestCancellationDuringStartingNeverReleasesUserCommand`. |
| Failed or disabled observations advanced freshness, and synchronous list/task reads could hide their failures behind an older successful sync row or a later job's success. | Disabled marker detection produces no successful observation; marker directory, per-marker ownership, status-file, and scheduler-probe errors propagate. Background, forced, direct-lane, and multi-lane read-triggered failures update the generation-specific sync ledger. Multi-job list reads publish one final per-generation aggregate, so iteration order cannot wash away a failure. Covered by `TestScanDoneMarkersDisabledIsNotSuccessfulObservation`, `TestScanDoneMarkersPropagatesOwnershipLookupFailure`, `TestScanDoneMarkersPropagatesReadDirFailure`, `TestFailedSchedulerProbeDoesNotAdvanceFreshness`, `TestFailedStatusReadDoesNotAdvanceFreshness`, `TestSSHListObservationFailureMarksReturnedSnapshotStale`, `TestMultiListObservationFailureMarksReturnedSnapshotStale`, and `TestJobListAggregatesEveryObservationBeforePublishingFreshness`. |
| Timeout was dropped or conflated with user cancellation. | runq-lab preserves timeout in the runqd attempt specification; runqd persists `termination_reason=timeout`, which cannot overwrite cancellation. Covered by `TestRunqdSubmitTemplatePropagatesTimeout` and runqd's `TestTimeoutAndUserCancelHaveDistinctDurableCauses`. |
| Pre-timeout databases could not be queried after upgrade. | The idempotent column migration now adds `tasks.timeout` before task SELECTs run. Covered by `TestMigrateAddsTimeoutToPreTimeoutTasksTable`. |
| Pause was unavailable through the SSH-style lane and disappeared on restart. | `SSHBackend.PauseJob` and `ResumeJob` persist before changing the gate; recovery reinstates every paused gate before readiness; whole-job kill explicitly clears it. Resume and kill use `ProjectJobStatus`. Covered by `TestSSHRestoreReinstatesPausedDispatchGate` and `TestSSHKillJobClearsPausedDispatchGate`. |
| Freeze/thaw could claim success after signal failure. | Standalone runqd returns signal errors, retains source state, and compensates persistence failures. `TestFreezeAndThawFailuresDoNotAdvanceDurableState` covers failed STOP/CONT. |
| Whole-job kill disagreed with task kill for unknown no-ID attempts. | `SSHBackend.KillJob` applies the same escape hatch and releases the slot. Covered by `TestSSHKillJobSettlesUnknownAttemptWithoutExternalID`. |
| Kill intent was memory-only and cancellation could report success after a failed durable transition. | `tasks.kill_requested` is persisted before publication, restored on startup, replayed for externally-owned attempts, and cleared only by terminal/retry transitions. Pending and unknown kill paths return persistence errors. Covered by `TestSSHKillTaskReportsPersistenceFailure`, `TestSSHRestoreReplaysDurableKillIntentIntoLifecycle`, and scheduler kill-race tests. |
| Scheduler-confirmed cancellation used an unclassified `kill` provenance token. | All confirmed user cancellation now persists the authoritative `runq` evidence class, so weaker wrapper/probe evidence cannot reopen it. Covered by `TestKillFlagSettlesFailureAsKilled`. |
| Automatic retry readback looked like an illegal terminal regression, and its failed-to-pending reducer dropped the observation's full attempt fence and retained stale evidence fields. | Observation persistence accepts only the exact failed-to-pending transition with the next retry epoch; the scheduler carries status/source/epoch/generation/external-ID predicates into that reducer and clears source/native-state/queue fields for the new pending epoch. Unrelated terminal regressions remain rejected. Covered by `TestPersistDecisionAcceptsDurableAutomaticRetry`, `TestAutomaticRetryCarriesCompleteAttemptFence`, and `TestAutomaticRetryClearsPriorAttemptEvidence`. |
| Job-state derivation was duplicated. | `store.ProjectJobStatus` is the canonical task-to-job reducer with table-driven coverage for pending, submitting, unknown, mixed, terminal, and corrupt inputs. |
| runqd admission could partially create an execution record. | `runqd/store.Admit` inserts execution job and attempt in one transaction, replays an identical normalized spec, and rejects ID/spec conflict. Covered by `TestAdmissionIsAtomicIdempotentAndConflicting`. |
| runq-lab sweep admission inserted tasks incrementally. | `remote.Backend.Prepare` builds the complete plan first and commits the job plus every task through `Store.InsertJobWithTasks`. An injected failure on task two leaves no job or task rows. Covered by `TestPrepareRollsBackWholeSweepWhenAnyTaskInsertFails`. |
| runqd recovery could publish monitors before all recovered leases were valid. | Recovery now classifies first, reserves every live lease second, and starts monitors only after the complete set is valid. A conflict rolls back provisional reservations and fails readiness. Covered by `TestRecoveryCapacityConflictRollsBackBeforeStartingMonitors`. |
| runqd recovery made every durable `starting` attempt dispatchable. | Recovery now terminalizes persisted cancel/timeout intent before considering an intent-free attempt for requeue. Covered by `TestRecoveryStartingAttemptsHonorTerminationIntent`. |
| The client and daemon derived different default sockets. | Both now resolve `RUNQD_SOCKET`, then `RUNQD_DATA_DIR`, then the same root/user defaults. `TestRunqLabAdapterEndToEnd` exercises the real Unix-socket adapter, idempotent receipt grammar, status mapping, GPU compatibility shape, and cancellation. |
| A scheduler terminal could regress to pending/running on a later probe. | Reconciliation now classifies current and candidate evidence and applies one closed acceptance matrix. Scheduler live evidence can advance pending to running but cannot reopen terminal or regress running to pending; wrapper/runq terminal evidence is absorbing. Covered by `TestAcceptCandidateEvidenceMatrix` and the focused `TestReconcile` cases. |
| Whole-job kill hid failures from retiring target generations, and concurrent retry/config rotation could move ownership after a control or read resolved its lane. | `MultiBackend` serializes retry/kill/pause/resume, and routed operations hold a generation-topology read lease while config rotation and retirement hold the write lease. Whole-job kill attempts every live generation and joins all failures. Lane point reads and controls re-check exact target/generation ownership under the shared attempt lock before touching an endpoint. Covered by `TestKillJobAttemptsEveryGenerationAndJoinsErrors`, `TestRetryAndKillReResolveUnderOneControlOrder`, `TestRoutingUpdateLinearizesForceRefreshToReplacement`, `TestLaneScopePointOwnershipIsGenerationExact`, `TestSingleTaskKillResolutionRejectsAnotherGeneration`, `TestSSHPointArtifactReadsRejectMovedGeneration`, and `TestMultiPointReadRechecksOwnershipAfterResolution`. |
| Removing a target made its durable history disappear or left a listed terminal job impossible to open/archive after the last retiring lane closed. | Global, target-scoped, and archived job lists plus terminal detail/results/archive operations render directly from SQLite and use live lanes only to refresh nonterminal owners. Removed targets therefore remain usable without a live lane representative. Covered by `TestListJobsKeepsRemovedTargetHistoryWithoutLiveLane`. |
| A terminal task from a settled generation fell through to the current target endpoint for point logs, job-wide logs/activity, metric files, and cleanup; the active SQL scope also treated a settled recorded generation as an orphan during restart. | Retirement now moves real lanes into a quiesced artifact-only registry. Restart rebuilds cold historical lanes from persisted target snapshots, active adoption is limited to generations with no history record at all, and point/job-wide/cleanup artifact access partitions durable rows by exact generation. Long-lived log followers pin their owning transport until close. A missing snapshot/endpoint returns an explicit unavailable-generation error and never falls through. Covered by `TestSettledTaskArtifactsStayOnOwningGeneration`, `TestJobArtifactsPartitionEveryTaskByOwningGeneration`, `TestCleanUsesTaskGenerationArtifactEndpoint`, `TestLogFollowerPinsLaneTransportUntilClose`, `TestCompleteRetiringLaneMovesItOutOfLiveRefresh`, `TestRetiringSweepPreservesArtifactCapableGeneration`, `TestRestartRebuildsSettledGenerationAsColdArtifactLane`, and `TestActiveScopeDoesNotAdoptSettledRecordedGeneration`. |
| Shutdown could snapshot lanes while config reconciliation or retirement quiesce was still publishing/moving them, leaking a lane backed by a closed store. | Shutdown rejects new lifecycle callbacks, closes ingress, drains the shared reconcile/sweep barrier, and only then closes the final forward/lane registries and store. Lane close is idempotent and transport teardown waits for follower leases. Covered by `TestShutdownWaitsForReconcilePublication` and `TestShutdownDoesNotCloseLaneDuringRetirementQuiesce`. |
| Restart could report ready with a live retiring generation unowned. | Retiring-generation recovery now attempts every stored owner and returns a joined startup error for unreadable snapshots, failed lane construction, or store failures. The client shuts down instead of falling through to the active endpoint. Covered by `TestRebuildRetiringLanesFailsWhenLiveSnapshotIsUnreadable` and `TestRebuildRetiringLanesPublishesOwnerBeforeSuccess`. |
| Active-lane recovery could publish a partial in-memory snapshot and still report ready. | `restoreLane` repairs every durable row first and publishes queue entries, leases, kill intents, and pause gates only after the repair set succeeds. Startup and dynamic replacement fail closed. Covered by `TestSSHRestoreFailsClosedWhenStoreIsUnreadable`, `TestReconcileAddedLaneRecoveryFailureStaysUnrouted`, and `TestReconcileReplacementRecoveryFailureRestoresOldLane`. |

### Remaining findings

No known transition violation remains in the audited set. Compatibility-window fixtures and broader fault injection remain hardening work, not hidden exceptions to `t`.

## Consolidation and test targets

- Keep `store.ProjectJobStatus` as the only task-to-job lifecycle reducer.
- Use one transition vocabulary across scheduler, storage guards, cleanup, reconciliation, and API validation.
- Keep observation precedence as a closed table; every new evidence class must define every matrix cell.
- Keep recovery two-phase: classify all records, acquire every lease, then start monitors and report readiness.
- Keep machine-attempt transitions in runqd; runq-lab owns experiment policy and projections.
- Extend atomic-admission fault injection to before task one and at commit; every rejection must leave zero residue.
- Inject failure at every `P`, `M`, `L`, and `A` boundary and require the source state or explicit `UL`/`UR`.

These remain classifiers and ordered effect executors. The DFA is an audit IR, not a replacement runtime framework.
