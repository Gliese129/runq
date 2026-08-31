package remote

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gliese129/runq-lab/internal/config"
)

// opLog appends one timestamped entry to the HPC operation log
// (~/.runq/logs/runq.log): every submit/kill records the rendered scheduler
// command, its output and any error — a rejected qsub stays diagnosable
// after the terminal scrolled away.
//
// Strictly best-effort: logging must never fail an operation, so errors are
// swallowed. Multi-line output is indented to keep entries grep-able by
// prefix.
func opLog(format string, args ...any) {
	if err := os.MkdirAll(config.HPCLogDir(), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(config.HPCOpLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	msg := fmt.Sprintf(format, args...)
	msg = strings.TrimRight(msg, "\n")
	msg = strings.ReplaceAll(msg, "\n", "\n    ")
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
}
