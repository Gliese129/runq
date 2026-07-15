# examples/

Copy-paste starting points for runq configs. Every file is fully annotated —
reading them IS the schema reference (also see
[docs/configuration.md](../docs/configuration.md)).

| You want to... | Copy |
|---|---|
| Submit your first sweep with minimum ceremony | [`job_simple.yaml`](./job_simple.yaml) |
| See every job.yaml feature: grid × zip blocks, note, overrides | [`job.yaml`](./job.yaml) |
| See every project.yaml feature: catalog, strict choices, env, resume | [`project.yaml`](./project.yaml) |
| Run model × benchmark evals on an HPC cluster, each benchmark with its own walltime | [`hpc/`](./hpc/) |
| A training script that runq init can scan and whose metrics feed `runq best` | [`test_train.py`](./test_train.py) |

Quick start from zero (local GPU machine):

```bash
runq init train.py        # or copy project.yaml + job_simple.yaml and edit
runq project add .
runq submit job.yaml --dry   # preview the exact expansion — free
runq submit job.yaml
```

Two rules that explain most of the schema:

- **project.yaml is the catalog** (what CAN vary, with types and defaults);
  **job.yaml is one selection** over it (what varies THIS time). Keep the
  catalog right once; job.yamls stay short.
- **`grid` = cartesian product, `list` = zip 1-to-1.** Values that belong
  together (benchmark + its walltime, model + its parser) go in one `list`
  block; independent dimensions go in `grid`. Blocks combine by
  cross-product.
