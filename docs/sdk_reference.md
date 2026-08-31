# Python SDK reference

`pip install runq-sdk`

The SDK runs inside your training script, not beside it. It gives you
typed parameters from the sweep, a preemption-aware training loop,
metrics that feed `runq best`, atomic checkpoints, and early stopping —
without locking you into runq. Every function works standalone.

## Modes

The SDK auto-detects its environment from `RUNQ_TASK_ID` and
`RUNQ_SOCKET`:

| Mode | When | What works |
|---|---|---|
| **daemon** | `runqd` is running, task was submitted by runq | Everything: params, metrics, sync, freeze |
| **no_daemon** | `RUNQ_TASK_ID` is set but no socket (HPC) | Params, metrics, checkpoints — all file-based |
| **manual** | Neither env var is set | Everything except daemon sync — use it for local debugging |

## Quick example

```python
import runq

@runq.dataclass(auto_overwrite=True)
class Params:
    lr: float = 0.001
    batch_size: int = 32

cfg = Params()                          # sweep params auto-merged

for step in runq.range(1000):           # preemption + early-stop aware
    loss = train_step(model, cfg)
    runq.log_metric("loss", loss)       # step auto-incremented
    runq.report({"val_loss": val})      # early-stop check
    runq.safe_save("ckpt.pt", model.state_dict(), keep_last_n=3)
```

## Context and parameters

```python
ctx = runq.context()       # Context dataclass: task_id, params, seed, mode, paths
runq.params                # ParamDict — dict with fuzzy-match suggestions on KeyError
runq.seed                  # deterministic per-task seed (SHA-256 of task_id, mod 2^32)
```

### Typed parameters with `@runq.dataclass`

```python
@runq.dataclass(auto_overwrite=True)
class Params:
    lr: float = 0.001
    optimizer: str = "adam"

cfg = Params()  # defaults overwritten by sweep values automatically
```

`auto_overwrite=True` merges sweep parameters into the dataclass fields
by name and type. Missing sweep params keep their defaults. Extra sweep
params not in the class are ignored.

## Training loop

### `runq.range(n)`

Drop-in replacement for `range()` that checks for preemption and
early-stop signals between iterations.

```python
for step in runq.range(1000):
    train_step()
# loop exits early on preempt or early-stop — no exception handling needed
```

### `runq.loop(iterable)`

Same preemption awareness for arbitrary iterables:

```python
for batch in runq.loop(dataloader):
    train_step(batch)
```

### `runq.is_preempted()` / `runq.preempted`

Check whether the current task has been asked to stop (signal from
scheduler or `runq kill`).

## Metrics

### `runq.log_metric(key, value, step=None)`

Log a scalar metric. Step auto-increments per key if omitted.

```python
runq.log_metric("loss", 0.42)
runq.log_metric("accuracy", 0.91)
```

Writes to `metrics.jsonl` in the task workspace. Feeds `runq best` and
`runq collect` on the CLI side, and the dashboard's metric charts.

### `runq.log_group(name)`

Context manager for grouping metrics (e.g., per-epoch):

```python
with runq.log_group("epoch_5"):
    runq.log_metric("train_loss", 0.3)
    runq.log_metric("val_loss", 0.35)
```

## Results

### `runq.record(metrics, **axes)`

Write a result record — a bounded fact about one evaluation, stored in
full in `results.jsonl`. Unlike metrics (a stream), records are
complete rows that feed `runq results` and the dashboard results view.

```python
runq.record({"accuracy": 0.91, "f1": 0.88}, dataset="mmlu", split="test")
```

## Early stopping

### `runq.report(metrics, policies=None)`

Evaluate metrics against early-stop policies. Returns a `Decision`; if
the decision is to stop, `runq.range` / `runq.loop` exit on the next
iteration.

```python
decision = runq.report({"val_loss": val_loss})
```

### `runq.early_stop(key, value, policies=None)`

Convenience wrapper — logs the metric and evaluates in one call.

### Built-in policies

```python
from runq import patience, threshold, convergence

# Stop if val_loss hasn't improved for 10 reports
runq.report({"val_loss": v}, policies=[patience(10)])

# Stop if val_loss drops below 0.01
runq.report({"val_loss": v}, policies=[threshold("val_loss", 0.01)])

# Stop if improvement < 0.1% over last 5 reports
runq.report({"val_loss": v}, policies=[convergence(window=5, min_delta=0.001)])
```

Policies are composable — pass a list and any trigger stops the task.

## Checkpoints

### `runq.safe_save(path, data, keep_last_n=None, keep_best=None)`

Atomic checkpoint write with fsync. If disk is full (`ENOSPC`), the task
is frozen (`SIGSTOP`) until `runq thaw` frees space — no silent
corruption.

```python
runq.safe_save("ckpt.pt", model.state_dict(), keep_last_n=3)
```

- `keep_last_n`: keep only the N most recent checkpoints
- `keep_best`: keep the checkpoint with the best metric value (requires
  prior `log_metric` calls)
- Relative paths resolve to `checkpoints/` in the task workspace

### `runq.latest_checkpoint()` / `runq.best_checkpoint()`

Find the most recent or best-metric checkpoint path for resume:

```python
ckpt_path = runq.latest_checkpoint()
if ckpt_path:
    model.load_state_dict(torch.load(ckpt_path))
```

## Utilities

```python
with runq.utils.atomic_write("output.json") as f:
    json.dump(data, f)
# tmp + fsync + rename — no partial writes
```

## File layout per task

| File | Written by | Read by |
|---|---|---|
| `params.json` | runq (at submit) | SDK `context()` |
| `metrics.jsonl` | SDK `log_metric` | `runq best`, `runq collect`, dashboard |
| `results.jsonl` | SDK `record` | `runq results`, dashboard |
| `events.jsonl` | SDK lifecycle events | dashboard timeline |
| `checkpoints/` | SDK `safe_save` | your resume code |

## Installation

```bash
pip install runq-sdk          # from PyPI
cd sdk/python && pip install -e .   # development
```
