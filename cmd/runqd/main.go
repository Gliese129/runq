// runqd is the headless execution daemon for machines that own GPUs
// (lab servers, workstations). Linux-only deployment target.
//
// It schedules and runs tasks on THIS machine only: store + queue +
// scheduler + executor + the Unix-socket API. No dashboard, no remote
// targets — clients (the cross-platform `runq` binary on your laptop) reach
// it over SSH by invoking the runq CLI here, exactly like they drive Slurm
// with sbatch. SSH is both the transport and the auth boundary: your unix
// account is your identity, and runqd never grows auth code of its own.
//
// Runs in the foreground (systemd-friendly). Installation is: copy the
// binary, run it.
package main

import (
	"fmt"
	"os"

	"github.com/gliese129/runq/internal/app"
)

func main() {
	d, err := app.NewDaemonWith(app.DaemonOptions{Headless: true})
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "runqd:", err)
		os.Exit(1)
	}
	if err := d.Run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "runqd:", err)
		os.Exit(1)
	}
}
