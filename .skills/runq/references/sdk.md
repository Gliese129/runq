# Python SDK — instrumenting training code

`pip install runq`, then `import runq` in the training script. The SDK
handles parameter injection, metrics logging, checkpoint safety, cooperative
preemption, and early stopping — without changing the training loop
structure.

**Tri-mode:** daemon (Unix socket) · no_daemon (file-only, for HPC) ·
manual (no runq infrastructure at all). The same script works in all three.

```python
import runq

ctx = runq.context()                      # init from env vars or params

@runq.dataclass(auto_overwrite=True)      # typed params, auto-merged
class Params:                             # from sweep parameters
    lr: float = 0.001
    batch_size: int = 32

cfg = Params()                            # cfg.lr may be overridden by the sweep

for step in runq.range(1000):             # preemption + early-stop aware
    loss = train(model, cfg)
    runq.log_metric("loss", loss)
    runq.report({"val_loss": evaluate(model)})   # early-stop check
    runq.safe_save("ckpt.pt", model.state_dict(), keep_last_n=3)

ckpt = runq.latest_checkpoint()           # or runq.best_checkpoint()
```

Key API:

- `runq.range()` / `runq.loop()` — drop-in iterators with SIGTERM preemption
  and early-stop; `range()` for numeric loops, `loop()` for iterables.
- `@runq.dataclass` — typed param class with to/from json+yaml;
  `auto_overwrite=True` merges sweep params at instantiation.
- `runq.safe_save()` — atomic checkpoint writes (tmp + fsync + rename),
  ENOSPC handling, manifest-scoped cleanup via `keep_last_n` / `keep_best`.
  Relative paths resolve via `ctx.checkpoint_dir`. Cleanup only ever touches
  files the SDK created.
- `runq.report()` — early-stop with pluggable policies
  (`patience`, `threshold`, `convergence`).
- `runq.seed` — deterministic per-task seed derived from task ID.
- Metrics land in `<task_dir>/metrics.jsonl` — this is what
  `runq best/collect --key` reads. wandb forwarding: if project.yaml has a
  `wandb:` block, use `wandb.init(**ctx.wandb_cfg)`.

Usage patterns: `sdk/python/examples/` in the runq repo; architecture:
`docs/sdk_reference.md`.
