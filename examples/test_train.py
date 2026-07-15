"""Example training script for runq.

Two purposes:
  1. `runq init test_train.py` scans these argparse arguments into a
     project.yaml catalog (note: positional args are skipped by design —
     sweep params must be flag-style).
  2. It writes metrics.jsonl into the task workspace, so
     `runq best <job> --key loss` and `runq collect` work end to end.

Run it standalone:  python test_train.py --lr 0.01 --batch-size 64
"""
import argparse
import json
import math
import os
import random

parser = argparse.ArgumentParser()
parser.add_argument("--lr", type=float, default=0.001, help="learning rate")
parser.add_argument("--batch-size", type=int, default=32)
parser.add_argument("--optimizer", default="adam", choices=["adam", "sgd"])
parser.add_argument("--epochs", type=int, default=5)
parser.add_argument("--resume", action="store_true",
                    help="used by resume.extra_args in project.yaml")
parser.add_argument("checkpoint_dir", nargs="?", default=None,
                    help="positional → runq init skips it on purpose")
args = parser.parse_args()

# Metrics: with the SDK, prefer `runq.log_metric("loss", v)`. Without it,
# appending JSON lines to metrics.jsonl in the working directory has the
# same effect — runq's best/collect read this file per task.
metrics_path = os.environ.get("RUNQ_METRICS_FILE", "metrics.jsonl")

rng = random.Random(f"{args.lr}-{args.batch_size}-{args.optimizer}")
loss = 2.5
with open(metrics_path, "a") as f:
    for epoch in range(1, args.epochs + 1):
        # A fake but well-behaved curve: lr and batch size shape the floor.
        floor = 0.3 + abs(math.log10(args.lr / 0.01)) * 0.1 + (32 / args.batch_size) * 0.05
        loss = max(floor, loss * 0.7 + rng.uniform(-0.02, 0.02))
        print(f"epoch {epoch}/{args.epochs} lr={args.lr} "
              f"bs={args.batch_size} opt={args.optimizer} loss={loss:.4f}")
        f.write(json.dumps({"step": epoch, "loss": round(loss, 4)}) + "\n")

print("done.")
