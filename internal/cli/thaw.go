package cli

import (
	"fmt"
	"os"
	"sort"

	"github.com/gliese129/runq/internal/api"
	"github.com/gliese129/runq/internal/backend"
	"github.com/spf13/cobra"
)

var thawForce bool

// thawCmd implements `runq thaw` — release the SDK-driven disk freeze for
// the caller's tasks.
//
//	runq thaw            checked thaw: only release tasks whose mount has
//	                    enough free space for their per-task NeededBytes
//	runq thaw --force    bypass the disk check (caller accepts ENOSPC risk)
//
// Owner scope: defaults to the caller's UID. Blocked tasks come back with
// a mount-mate listing so the user can see who's hogging the disk.
var thawCmd = &cobra.Command{
	Use:   "thaw",
	Short: "Release SIGSTOPped tasks that the SDK froze on low-disk",
	Long: `Release tasks that the SDK paused via /api/internal/freeze-self
when their checkpoint write would have exceeded free disk space.

Default mode runs a per-task safety check: a task only resumes when its
mount has free_bytes >= NeededBytes (set by the SDK at freeze time as
upcoming_ckpt_size × safety_factor). Tasks that don't pass come back with
a per-mount listing of who else is writing to that disk, so you can find
the cleanup target.

--force skips the per-task check and SIGCONTs every owned task. The task
will likely re-trigger the freeze immediately if disk hasn't recovered —
use this only when you know what you're doing (or want to kill the task
afterward with 'runq kill').`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Freeze/thaw is a machine-local concern: frozen processes live
		// under THIS machine's executor. Plumbing socket → runqd.
		p := api.NewProxy(getPlumbingSocketPath())
		result, err := p.ThawTasks(cmd.Context(), os.Getuid(), thawForce)
		if err != nil {
			return err
		}
		printThawResult(result)
		return nil
	},
}

// printThawResult renders the structured response. Layout:
//
//	✓ thawed t3 (mount /data2)
//
//	✗ t1 blocked (mount /data1: free 1.2 GB < needed 50 GB)
//	✗ t2 blocked (mount /data1: free 1.2 GB < needed 50 GB)
//
//	Tasks using /data1 (sorted by total ckpt size):
//	  bob/job-B456 · t9 — 350 GB (latest 35 GB)
//	  alice/job-A123 · t7 — 120 GB (latest 12 GB)
//
// Empty thaw → "nothing was frozen for you".
func printThawResult(r *backend.ThawResponse) {
	if len(r.Thawed) == 0 && len(r.Blocked) == 0 {
		fmt.Println("nothing was frozen for you")
		return
	}

	for _, id := range r.Thawed {
		fmt.Printf("✓ thawed %s\n", id)
	}

	if len(r.Blocked) == 0 {
		return
	}

	// Group blocked entries by mount so we render the disk-users section
	// once per mount. blocked entries are unsorted in the JSON; sort task
	// IDs for deterministic output.
	type blockedEntry struct {
		TaskID string
		Detail backend.BlockedDetail
	}
	byMount := make(map[string][]blockedEntry)
	for tid, br := range r.Blocked {
		byMount[br.Mount] = append(byMount[br.Mount], blockedEntry{tid, br})
	}

	if len(r.Thawed) > 0 {
		fmt.Println()
	}
	for _, entries := range byMount {
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].TaskID < entries[j].TaskID
		})
		for _, e := range entries {
			fmt.Printf("✗ %s blocked (mount %s: free %s < needed %s)\n",
				e.TaskID, e.Detail.Mount,
				humanBytes(e.Detail.FreeBytes), humanBytes(e.Detail.Threshold))
		}
	}

	// Disk users per mount (only print each mount once; data is identical
	// across entries on the same mount).
	seen := make(map[string]bool)
	for _, br := range r.Blocked {
		if seen[br.Mount] {
			continue
		}
		seen[br.Mount] = true
		if len(br.DiskUsers) == 0 {
			continue
		}
		fmt.Printf("\nTasks using %s (sorted by total ckpt size):\n", br.Mount)
		for _, m := range br.DiskUsers {
			who := m.User
			if who == "" {
				who = "(unknown)"
			}
			fmt.Printf("  %s/%s · %s — %s",
				who, m.JobID, m.TaskID, humanBytes(m.TotalCkptBytes))
			if m.LatestCkptBytes > 0 {
				fmt.Printf(" (latest %s)", humanBytes(m.LatestCkptBytes))
			}
			fmt.Println()
		}
	}

	fmt.Println("\nHint: clean disk space, then re-run 'runq thaw'.")
	fmt.Println("Or 'runq thaw --force' to release without the disk check (will likely re-freeze).")
}

func init() {
	thawCmd.Flags().BoolVarP(&thawForce, "force", "f", false,
		"bypass per-task disk safety check (SIGCONT every owned frozen task)")
	thawCmd.GroupID = groupManagement
	rootCmd.AddCommand(thawCmd)
}
