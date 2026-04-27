package job

import (
	"os"
	"regexp"
	"strings"
)

// ArgInfo represents one argument extracted from a Python argparse script.
type ArgInfo struct {
	Name    string // flag name without "--", e.g. "lr", "batch_size"
	Type    string // python type: "float", "int", "str", "bool", "" if unknown
	Default string // default value as string, "" if none
}

// ScanArgparse scans a Python file for argparse add_argument calls and extracts
// argument metadata (name, type, default).
//
// Strategy:
//  1. Read the file as text.
//  2. Find each `add_argument(` or `.add_argument(` occurrence.
//  3. Use bracket matching to extract the full call (handles multi-line).
//  4. From the extracted call text, use regex to pull:
//     - The flag name: first string arg starting with "--"
//     - type=xxx keyword argument
//     - default=xxx keyword argument
//
// Edge cases to handle:
//   - Multi-line calls (the whole point of bracket matching)
//   - Positional args (no "--") → skip, not sweep-able
//   - action="store_true" → treat as type="bool", default="false"
//   - Quoted vs unquoted defaults: default=0.001 and default="adam"
//   - Nested parentheses in default values, e.g. default=dict(a=1) → best-effort
//
// Returns arguments in order of appearance. Duplicate names are kept (caller decides).
func ScanArgparse(filePath string) ([]ArgInfo, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	argReg := regexp.MustCompile(`\.add_argument\(`)
	flagReg := regexp.MustCompile(`"--([\w-]+)"|'--([\w-]+)'`)
	typeReg := regexp.MustCompile(`type\s*=\s*(\w+)`)
	defaultReg := regexp.MustCompile(`default\s*=\s*(.+?)\s*[,)]`)
	storeReg := regexp.MustCompile(`action="store_true"`)
	matches := argReg.FindAllIndex(content, -1)

	args := make([]ArgInfo, 0)
	for _, match := range matches {
		st := match[1]
		ed := st
		cnt := 1

		for i := st; i < len(content); i++ {
			switch content[i] {
			case '(':
				cnt++
			case ')':
				cnt--
			}
			if cnt == 0 {
				ed = i + 1
				break
			}
		}
		c := string(content[st:ed])
		var name_, type_, default_ string
		if m := flagReg.FindStringSubmatch(c); len(m) >= 3 {
			// Two capture groups: m[1] for double-quoted, m[2] for single-quoted.
			name_ = m[1]
			if name_ == "" {
				name_ = m[2]
			}
		}
		if m := typeReg.FindStringSubmatch(c); len(m) >= 2 {
			type_ = m[1]
		}
		if m := defaultReg.FindStringSubmatch(c); len(m) >= 2 {
			default_ = stripQuotes(m[1])
		}
		if storeReg.MatchString(c) {
			type_ = "bool"
			default_ = "false"
		}

		arg := ArgInfo{
			Name:    name_,
			Type:    type_,
			Default: default_,
		}

		if arg.Name != "" {
			args = append(args, arg)
		}
	}

	return args, nil
}

// stripQuotes removes surrounding single or double quotes from a string.
func stripQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}
