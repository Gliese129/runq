#!/usr/bin/env python3

import argparse
import json
import os
import signal
import subprocess
import sys
import time


SHOULD_EXIT = False


def handle_sigusr1(signum, frame):
    del signum, frame
    global SHOULD_EXIT
    SHOULD_EXIT = True


def parse_args():
    parser = argparse.ArgumentParser()
    parser.add_argument("--name", default="probe")
    parser.add_argument("--sleep", type=float, default=3)
    parser.add_argument("--fail", type=int, default=0)
    parser.add_argument("--marker", default="")
    return parser.parse_known_args()


def main():
    args, _ = parse_args()
    signal.signal(signal.SIGUSR1, handle_sigusr1)

    visible = os.environ.get("CUDA_VISIBLE_DEVICES", "")
    print(
        json.dumps(
            {
                "name": args.name,
                "marker": args.marker,
                "cuda_visible_devices": visible,
                "pid": os.getpid(),
                "cwd": os.getcwd(),
            },
            sort_keys=True,
        ),
        flush=True,
    )

    try:
        out = subprocess.check_output(
            [
                "nvidia-smi",
                "--query-gpu=index,memory.used,utilization.gpu",
                "--format=csv,noheader,nounits",
            ],
            text=True,
            timeout=10,
        )
        print("NVIDIA_SMI_BEGIN", flush=True)
        print(out.strip(), flush=True)
        print("NVIDIA_SMI_END", flush=True)
    except Exception as exc:
        print(f"NVIDIA_SMI_ERROR {exc}", flush=True)

    if args.fail:
        print("intentional failure", flush=True)
        return args.fail

    deadline = time.time() + args.sleep
    while time.time() < deadline:
        if SHOULD_EXIT:
            print("checkpoint saved", flush=True)
            return 0
        print(f"heartbeat name={args.name} visible={visible}", flush=True)
        time.sleep(1)

    return 0


if __name__ == "__main__":
    sys.exit(main())
