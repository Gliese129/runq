package resource

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Info holds parsed data from nvidia-smi for one GPU.
type Info struct {
	Index   int
	Name    string
	MemFree int // MB
	MemUsed int // MB
	UtilPct int // %
}

// Detect queries nvidia-smi and returns info for all GPUs on this machine.
// 10s timeout prevents daemon startup from hanging if the GPU driver is stuck.
func Detect() ([]Info, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=index,name,memory.free,memory.used,utilization.gpu",
		"--format=csv,noheader,nounits",
	)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("nvidia-smi timed out after 10s (GPU driver may be stuck)")
		}
		return nil, fmt.Errorf("nvidia-smi failed: %w", err)
	}
	return Parse(string(out))
}

// Parse parses the CSV output of nvidia-smi into []Info.
// Pure function — Detect calls it internally, tests call it directly.
func Parse(output string) ([]Info, error) {
	output = strings.TrimSpace(output)
	if output == "" {
		return []Info{}, nil
	}

	lines := strings.Split(output, "\n")
	infos := make([]Info, 0, len(lines))

	for lineNum, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		fields := strings.Split(line, ",")
		if len(fields) != 5 {
			return nil, fmt.Errorf("line %d: expected 5 fields, got %d: %q", lineNum+1, len(fields), line)
		}

		index, err := parseIntField(fields[0], "index", lineNum)
		if err != nil {
			return nil, err
		}
		name := strings.TrimSpace(fields[1])
		memFree, err := parseIntField(fields[2], "mem_free", lineNum)
		if err != nil {
			return nil, err
		}
		memUsed, err := parseIntField(fields[3], "mem_used", lineNum)
		if err != nil {
			return nil, err
		}
		utilPct, err := parseIntField(fields[4], "util_pct", lineNum)
		if err != nil {
			return nil, err
		}

		infos = append(infos, Info{
			Index: index, Name: name,
			MemFree: memFree, MemUsed: memUsed, UtilPct: utilPct,
		})
	}
	return infos, nil
}

func parseIntField(s string, fieldName string, lineNum int) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("line %d: invalid %s %q: %w", lineNum+1, fieldName, s, err)
	}
	return v, nil
}
