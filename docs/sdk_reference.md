# Python SDK Reference

> Internal reference for the runq Python SDK (`sdk/python/runq/`).
> For user-facing API overview, see the "Python SDK" section in `README.md`.
> For setup guidance, see the skill's [setup reference](../.skills/runq/references/setup.md).

## Architecture

**Tri-mode:** daemon (Unix socket to resident daemon), no_daemon (file-only,
for HPC), manual (no runq infrastructure).

**Context** (`_context.py`): `runq.context()` initialises from env vars
(`RUNQ_TASK_ID`, `RUNQ_SOCKET`, etc.) or params passed directly. Returns a
`Context` dataclass.

**ParamDict** (`_context.py`): dict subclass with fuzzy-match suggestions on
KeyError (difflib).

**Seed** (`_context.py`): `runq.seed` — deterministic per-task seed via
SHA-256 of task_id, mod 2^32.

## Module Map

| File | Purpose |
|---|---|
| `_config.py` | `@runq.dataclass` — typed param class with auto-merge from sweep params |
| `_range.py` | `runq.range()` + shared iterator core (`_check_break`, `_init_iterator`) |
| `_loop.py` | `runq.loop()` for arbitrary iterables + `@epoch` + `log_group()` |
| `_safe_save.py` | Atomic checkpoint writes + ENOSPC freeze flow + decorator form |
| `_manifest.py` | Checkpoint manifest for `keep_last_n` / `keep_best` cleanup |
| `_report.py` | `runq.report()` — early-stop evaluation with pluggable policies |
| `_record.py` | `runq.record(metrics, **axes)` — result records → `results.jsonl` (full ingest, feeds `runq results`) |
| `_policies.py` | Built-in policies: `patience`, `threshold`, `convergence` |
| `_events.py` | `log_metric()` + three-file jsonl routing (`metric` → metrics.jsonl, lifecycle → events.jsonl) |
| `utils.py` | `runq.utils.atomic_write()` — tmp + fsync + rename context manager |
| `_transport.py` | httpx Unix socket client for daemon communication |
| `_sync.py` | `sync_now()` — push metrics to daemon on demand |

## Installation

```bash
cd sdk/python && pip install -e .
```

## Where to Look

| When you need to... | Use |
|---|---|
| Understand the public API | `sdk/python/examples/` + README "Python SDK" section |
| See usage patterns | `sdk/python/examples/` |

Same rule as the rest of runq: prefer user-facing surfaces over source-diving.
