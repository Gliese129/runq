package gpu

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
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
// Returns an error if nvidia-smi is not found or returns a non-zero exit code.
func Detect() ([]Info, error) {
	cmd := exec.Command("nvidia-smi",
		"--query-gpu=index,name,memory.free,memory.used,utilization.gpu",
		"--format=csv,noheader,nounits",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi failed: %w", err)
	}
	return Parse(string(out))
}

// Parse parses the CSV output of nvidia-smi into []Info.
// This is a pure function — Detect calls it internally, tests call it directly.
//
// Expected input format (one line per GPU):
//
//	0, NVIDIA A100-SXM4-80GB, 79000, 1000, 5
//	1, NVIDIA A100-SXM4-80GB, 80000, 0, 0
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
			Index:   index,
			Name:    name,
			MemFree: memFree,
			MemUsed: memUsed,
			UtilPct: utilPct,
		})
	}

	return infos, nil
}

// parseIntField is a helper that wraps strconv.Atoi with a contextual error message.
func parseIntField(s string, fieldName string, lineNum int) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("line %d: invalid %s %q: %w", lineNum+1, fieldName, s, err)
	}
	return v, nil
}
