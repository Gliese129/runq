package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/gliese129/runq-lab/internal/backend"
	"github.com/gliese129/runq-lab/internal/utils"
)

// ── runq clean（issue 397203ea…7caa7c）──
//
// 删除语义 all-or-nothing：task dir 整目录 + DB 记录，不可撤销。不提供
// 部分删除——目录布局自解释，要外科手术自己 cd。交互路径是层级 TUI
// （project → job → 确认），预览尺寸全部来自账本（checkpoints 表 +
// ingest mark），选择阶段零 FS/SSH 接触。--yes 是脚本/cron 的非交互出口。

var cleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Delete tasks and ALL their artifacts (irreversible)",
	Long: `Delete tasks matching the given selectors: the whole task directory,
log files, DB records, and jobs left empty. THIS CANNOT BE UNDONE.

Selectors (at least one required):
  --older-than <dur>   Tasks finished before this threshold
  --orphan             Tasks whose workspace directory is missing
  --archived           Tasks belonging to archived jobs
  --job <id>           All tasks in a specific job
  --task <id>          A specific task

Modifiers:
  --show               Preview what would be deleted (non-interactive)
  --yes                Skip the interactive flow and confirmations

Without --yes on a terminal, an interactive picker walks
project → job → confirmation.

Duration format: additive segments like 7d, 1m2w, 2w3d4h
  h = hours, d = days, w = weeks (7d), m = months (30d), y = years (365d)

Examples:
  runq clean --older-than 7d           # interactive picker
  runq clean --older-than 7d --show    # preview only
  runq clean --older-than 7d --yes     # delete everything matched (cron)
  runq clean --job <id> --yes`,
	RunE: runClean,
}

func init() {
	cleanCmd.Flags().String("older-than", "", "Age threshold, e.g. 7d, 1m2w, 2w3d4h")
	cleanCmd.Flags().Bool("orphan", false, "Select orphan tasks (workspace directory missing)")
	cleanCmd.Flags().Bool("archived", false, "Select tasks from archived jobs")
	cleanCmd.Flags().String("job", "", "Select all tasks in a specific job")
	cleanCmd.Flags().String("task", "", "Select a specific task")
	cleanCmd.Flags().Bool("show", false, "Preview what would be deleted without deleting")
	cleanCmd.Flags().BoolP("yes", "y", false, "Skip the interactive flow and confirmations")
	cleanCmd.Flags().StringP("target", "t", "", "Scope clean to a specific compute target")

	cleanCmd.GroupID = groupDiag
	rootCmd.AddCommand(cleanCmd)
}

func buildCleanOptions(cmd *cobra.Command) (backend.CleanOptions, error) {
	var opts backend.CleanOptions

	olderThan, _ := cmd.Flags().GetString("older-than")
	if olderThan != "" {
		dur, err := utils.ParseHumanDuration(olderThan)
		if err != nil {
			return opts, err
		}
		cutoff := time.Now().Add(-dur)
		opts.OlderThan = &cutoff
	}

	opts.Orphan, _ = cmd.Flags().GetBool("orphan")
	opts.Archived, _ = cmd.Flags().GetBool("archived")
	opts.JobID, _ = cmd.Flags().GetString("job")
	opts.TaskID, _ = cmd.Flags().GetString("task")
	showOnly, _ := cmd.Flags().GetBool("show")
	opts.DryRun = showOnly

	// At least one selector must be given.
	if !opts.Orphan && !opts.Archived && opts.JobID == "" && opts.TaskID == "" && opts.OlderThan == nil {
		return opts, fmt.Errorf("at least one selector required: --older-than, --orphan, --archived, --job, --task")
	}

	return opts, nil
}

func runClean(cmd *cobra.Command, args []string) error {
	opts, err := buildCleanOptions(cmd)
	if err != nil {
		return err
	}

	return withBackend(cmd, func(be backend.Backend) error {
		// Always preview first — the ledger-backed dry run is cheap.
		previewOpts := opts
		previewOpts.DryRun = true
		result, err := be.Clean(cmd.Context(), previewOpts)
		if err != nil {
			return err
		}

		if len(result.Preview) == 0 {
			fmt.Println("Nothing to clean.")
			return nil
		}

		// --show: print and stop.
		if opts.DryRun {
			printCleanPreview(result.Preview)
			return nil
		}

		yes, _ := cmd.Flags().GetBool("yes")
		execOpts := opts
		if !yes {
			if term.IsTerminal(int(os.Stdin.Fd())) {
				ids, ok := cleanPickerFlow(result.Preview, opts.OlderThan)
				if !ok {
					fmt.Println("Aborted.")
					return nil
				}
				// Exact-set execute: only what the user confirmed.
				execOpts = backend.CleanOptions{TaskIDs: ids, Target: opts.Target}
			} else {
				printCleanPreview(result.Preview)
				if !confirmYN(fmt.Sprintf("Delete %d tasks and ALL their files? This cannot be undone.", len(result.Preview))) {
					fmt.Println("Aborted.")
					return nil
				}
			}
		}

		result, err = be.Clean(cmd.Context(), execOpts)
		if err != nil {
			return err
		}

		fmt.Printf("Deleted %d tasks, %d jobs", result.Tasks, result.Jobs)
		if result.FreedBytes > 0 {
			fmt.Printf(", freed %s", humanBytes(result.FreedBytes))
		}
		fmt.Println()
		return nil
	})
}

// ── hierarchical picker: project → job → confirm ────────────────────────

type cleanJobGroup struct {
	jobID   string
	tasks   []backend.CleanPreviewItem
	ckptN   int
	ckptB   int64
	metricB int64
}

func (g cleanJobGroup) bytes() int64 { return g.ckptB + g.metricB }

// cleanPickerFlow walks project → job (or Clear All) → confirmation and
// returns the exact task-ID set to delete.
func cleanPickerFlow(preview []backend.CleanPreviewItem, olderThan *time.Time) ([]string, bool) {
	// Group: project → jobs (insertion-ordered, then sorted for stability).
	byProject := map[string]map[string]*cleanJobGroup{}
	for _, p := range preview {
		proj := p.Project
		if proj == "" {
			proj = "(unknown project)"
		}
		if byProject[proj] == nil {
			byProject[proj] = map[string]*cleanJobGroup{}
		}
		g := byProject[proj][p.JobID]
		if g == nil {
			g = &cleanJobGroup{jobID: p.JobID}
			byProject[proj][p.JobID] = g
		}
		g.tasks = append(g.tasks, p)
		g.ckptN += p.CkptFiles
		g.ckptB += p.CkptBytes
		g.metricB += p.MetricsBytes
	}

	// Level 1: project (skipped when there is only one).
	projects := make([]string, 0, len(byProject))
	for name := range byProject {
		projects = append(projects, name)
	}
	sort.Strings(projects)
	project := projects[0]
	if len(projects) > 1 {
		lines := make([]string, len(projects))
		for i, name := range projects {
			n := 0
			for _, g := range byProject[name] {
				n += len(g.tasks)
			}
			lines[i] = fmt.Sprintf("%-24s (%d tasks found)", name, n)
		}
		idx, ok := selectOne("Select project", lines)
		if !ok {
			return nil, false
		}
		project = projects[idx]
	}

	// Level 2: jobs of the project, plus Clear All on top.
	groups := make([]*cleanJobGroup, 0, len(byProject[project]))
	for _, g := range byProject[project] {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].jobID < groups[j].jobID })

	totalTasks, totalBytes := 0, int64(0)
	for _, g := range groups {
		totalTasks += len(g.tasks)
		totalBytes += g.bytes()
	}

	lines := make([]string, 0, len(groups)+1)
	lines = append(lines, fmt.Sprintf("Clear All  (%d tasks, %s)", totalTasks, humanBytes(totalBytes)))
	for _, g := range groups {
		note := jobNoteOf(g)
		lines = append(lines, fmt.Sprintf("%-10s %-20s ckpt: %d files %s; metrics: %s; %d tasks",
			shortID(g.jobID), note, g.ckptN, humanBytes(g.ckptB), humanBytes(g.metricB), len(g.tasks)))
	}
	idx, ok := selectOne(fmt.Sprintf("Delete from %s — pick a job, or Clear All", project), lines)
	if !ok {
		return nil, false
	}

	// Level 3: confirmation (defaults to No; irreversibility spelled out).
	if idx == 0 {
		cutoff := ""
		if olderThan != nil {
			cutoff = fmt.Sprintf(" older than the cutoff (before %s)", olderThan.Format("2006-01-02 15:04"))
		}
		warn := fmt.Sprintf("WARNING: this cannot be undone!\nDelete ALL %d tasks of %s%s — every file and record (%s)?",
			totalTasks, project, cutoff, humanBytes(totalBytes))
		if !confirmYN(warn) {
			return nil, false
		}
		ids := make([]string, 0, totalTasks)
		for _, g := range groups {
			for _, p := range g.tasks {
				ids = append(ids, p.TaskID)
			}
		}
		return ids, true
	}

	g := groups[idx-1]
	path := "(no files on disk)"
	if dir := firstTaskDir(g); dir != "" {
		path = filepath.Dir(dir) // the job's workspace dir
	}
	warn := fmt.Sprintf("Delete job %s (%d tasks, %s, path: %s)?\nThis cannot be undone — all files will be removed.",
		shortID(g.jobID), len(g.tasks), humanBytes(g.bytes()), path)
	if !confirmYN(warn) {
		return nil, false
	}
	ids := make([]string, 0, len(g.tasks))
	for _, p := range g.tasks {
		ids = append(ids, p.TaskID)
	}
	return ids, true
}

func jobNoteOf(g *cleanJobGroup) string {
	// Reason doubles as a terse status hint; keep the line compact.
	if len(g.tasks) > 0 && g.tasks[0].Reason != "" {
		return g.tasks[0].Reason
	}
	return ""
}

func firstTaskDir(g *cleanJobGroup) string {
	for _, p := range g.tasks {
		if p.TaskDir != "" {
			return p.TaskDir
		}
	}
	return ""
}

func shortID(id string) string {
	if len(id) > 10 {
		return id[:10]
	}
	return id
}

func printCleanPreview(items []backend.CleanPreviewItem) {
	fmt.Printf("Detected %d tasks:\n", len(items))
	for _, p := range items {
		detail := "all files"
		if p.Action == backend.CleanActionDBOnly {
			detail = "no files"
		}
		orphanTag := ""
		if p.Orphan {
			orphanTag = " [orphan]"
		}
		finished := ""
		if p.FinishedAt != nil {
			finished = " finished=" + time.Unix(*p.FinishedAt, 0).Format("2006-01-02 15:04")
		}
		size := ""
		if b := p.CkptBytes + p.MetricsBytes; b > 0 {
			size = " ~" + humanBytes(b)
		}
		fmt.Printf("  %s  %-8s  (%s)%s%s  reason=%s%s\n",
			shortID(p.TaskID), p.Status, detail, size, orphanTag, p.Reason, finished)
	}
}

// confirmYN prints a warning and reads a y/N answer (default No).
func confirmYN(prompt string) bool {
	fmt.Printf("%s [y/N] ", prompt)
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}
