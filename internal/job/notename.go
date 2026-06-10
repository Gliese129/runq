package job

import (
	"fmt"
	"os"
	"os/user"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RenderNote resolves {{placeholder}}s in a job note (display name).
//
// Vocabulary:
//   - any param name     fixed → its value; swept → "name(N)" (N = value count)
//   - {{project}} {{user}}                — stable context
//   - {{date}} {{time}} {{datetime}}      — volatile (now)
//   - {{sweep}}                            — compact sweep summary
//   - {{version}}                          — see below
//
// {{version}}: the run counter within a "name family". The family is found
// by scanning ExistingNotes with a pattern built from the template where
// volatile placeholders match ANY timestamp (a re-run tomorrow still belongs
// to yesterday's family — version follows experiment identity, not wall
// clock). v1 renders as nothing and swallows one preceding separator
// ("foo-{{version}}" → "foo"); later runs render "foo-v2", "foo-v3", ...
//
// The CALLER decides what to persist: store the template in ConfigJSON (so
// re-runs keep incrementing) and the resolved string in the job row.
type NoteContext struct {
	Project       string
	User          string // empty → resolved via $USER / os/user
	Now           time.Time
	ExistingNotes []string // notes of jobs already in this project
}

const (
	markVersion  = "\x00V\x00"
	markDate     = "\x00D\x00"
	markTime     = "\x00T\x00"
	markDatetime = "\x00DT\x00"
)

func RenderNote(cfg *JobConfig, nc NoteContext) (string, error) {
	note := cfg.Note
	if !strings.Contains(note, "{{") {
		return note, nil
	}
	if nc.User == "" {
		nc.User = osUser()
	}

	stable, volatileCounts := noteValues(cfg, nc)

	// Pass 1: stable placeholders → concrete values; volatile + version → markers.
	var unknown []string
	marked := placeholderRe.ReplaceAllStringFunc(note, func(match string) string {
		key := match[2 : len(match)-2]
		switch key {
		case "version":
			return markVersion
		case "date":
			return markDate
		case "time":
			return markTime
		case "datetime":
			return markDatetime
		}
		if v, ok := stable[key]; ok {
			return v
		}
		unknown = append(unknown, key)
		return ""
	})
	if len(unknown) > 0 {
		return "", fmt.Errorf(
			"note references unknown placeholder(s): %s\nAvailable: %s",
			strings.Join(unknown, ", "), strings.Join(noteVocabulary(stable), ", "))
	}
	_ = volatileCounts

	// Version scan (template may not use {{version}} at all).
	version := 0
	if strings.Contains(marked, markVersion) {
		version = nextVersion(marked, nc.ExistingNotes)
	}

	// Pass 2: materialize markers.
	out := marked
	out = strings.ReplaceAll(out, markDate, nc.Now.Format("20060102"))
	out = strings.ReplaceAll(out, markTime, nc.Now.Format("1504"))
	out = strings.ReplaceAll(out, markDatetime, nc.Now.Format("20060102-1504"))
	out = materializeVersion(out, version)

	return out, nil
}

// noteValues builds the stable placeholder map: params, project, user, sweep.
func noteValues(cfg *JobConfig, nc NoteContext) (map[string]string, int) {
	vals := map[string]string{
		"project": nc.Project,
		"user":    nc.User,
	}

	// Swept params: name(N).
	var sweepParts []string
	for _, block := range cfg.Sweep {
		keys := make([]string, 0, len(block.Parameters))
		for k := range block.Parameters {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if block.Method == "list" && len(keys) >= 2 {
			n := len(block.Parameters[keys[0]].Values)
			sweepParts = append(sweepParts, fmt.Sprintf("[%s](%d)", strings.Join(keys, "+"), n))
		}
		for _, k := range keys {
			n := len(block.Parameters[k].Values)
			vals[k] = fmt.Sprintf("%s(%d)", k, n)
			if block.Method != "list" || len(keys) < 2 {
				sweepParts = append(sweepParts, fmt.Sprintf("%s(%d)", k, n))
			}
		}
	}
	// Fixed params: literal value (sweep wins on collision, set above).
	for k, v := range cfg.FixedParams {
		if _, swept := vals[k]; !swept {
			vals[k] = formatValue(v)
		}
	}
	vals["sweep"] = strings.Join(sweepParts, "x")
	return vals, len(sweepParts)
}

func noteVocabulary(stable map[string]string) []string {
	keys := make([]string, 0, len(stable)+4)
	for k := range stable {
		keys = append(keys, k)
	}
	keys = append(keys, "version", "date", "time", "datetime")
	sort.Strings(keys)
	return keys
}

// nextVersion builds the family pattern (volatiles → format wildcards,
// version → optional "vN" group) and returns max(existing)+1.
func nextVersion(marked string, existing []string) int {
	pattern := regexp.QuoteMeta(marked)
	pattern = strings.ReplaceAll(pattern, markDatetime, `\d{8}-\d{4}`) // before date/time
	pattern = strings.ReplaceAll(pattern, markDate, `\d{8}`)
	pattern = strings.ReplaceAll(pattern, markTime, `\d{4}`)

	// {{version}} with its optional preceding separator becomes one optional
	// group, so the v1 form (no suffix at all) matches the same family.
	prefix, suffix, sep := splitAtVersion(pattern)
	re, err := regexp.Compile("^" + prefix + "(?:" + sep + "v(\\d+))?" + suffix + "$")
	if err != nil {
		return 1 // defensive: malformed template never blocks a submit
	}

	maxV := 0
	for _, note := range existing {
		m := re.FindStringSubmatch(note)
		if m == nil {
			continue
		}
		if m[1] == "" {
			if maxV < 1 {
				maxV = 1 // the bare form is v1
			}
		} else if n, err := strconv.Atoi(m[1]); err == nil && n > maxV {
			maxV = n
		}
	}
	return maxV + 1
}

// splitAtVersion cuts a (quoted) pattern at the first version marker,
// extracting the immediately preceding separator (which QuoteMeta may have
// escaped, e.g. "." → "\.").
func splitAtVersion(pattern string) (prefix, suffix, sep string) {
	i := strings.Index(pattern, markVersion)
	if i < 0 {
		return pattern, "", ""
	}
	prefix = pattern[:i]
	suffix = pattern[i+len(markVersion):]
	for _, s := range []string{`\.`, "-", "_", " "} {
		if strings.HasSuffix(prefix, s) {
			sep = s
			prefix = prefix[:len(prefix)-len(s)]
			break
		}
	}
	return prefix, suffix, sep
}

// materializeVersion renders version markers in the display string:
// v1 disappears and swallows one preceding separator; v≥2 renders "vN".
func materializeVersion(s string, version int) string {
	for {
		i := strings.Index(s, markVersion)
		if i < 0 {
			return s
		}
		start := i
		hasSep := i > 0 && strings.ContainsRune("-_. ", rune(s[i-1]))
		if hasSep {
			start = i - 1
		}
		var repl string
		if version <= 1 {
			repl = "" // swallow separator + marker
		} else if hasSep {
			repl = string(s[i-1]) + "v" + strconv.Itoa(version)
		} else {
			repl = "v" + strconv.Itoa(version)
		}
		s = s[:start] + repl + s[i+len(markVersion):]
	}
}

func osUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "unknown"
}
