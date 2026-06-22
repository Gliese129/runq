package utils

import (
	"fmt"
	"regexp"
	"strings"
)

// ShellQuote turns an arbitrary string into a single POSIX-shell-safe token.
//
// THREAT MODEL: values passed here are untrusted — sweep parameter values,
// user environment values, and text parsed out of external scheduler output.
// A value like `0.1; rm -rf ~` must NOT be able to break out of its token and
// inject commands.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	s = strings.ReplaceAll(s, "'", `'\''`)
	return "'" + s + "'"
}

// ParamRegex matches {{name}} placeholders. Dots are allowed so namespaced
// vars like {{param.h_rt}} (task params exposed to submit_template) resolve.
const ParamRegex = `\{\{\s*([\w.\-]+)\s*}}`

// Render fills {{name}} placeholders in tmpl with the matching value from vars.
// Every substituted value passes through ShellQuote so the rendered string is
// safe to hand to a shell.
//
// Contract:
//   - An unknown placeholder (no matching key in vars) is a hard error
//     (fail closed) — never leave a literal {{...}} or silently blank it.
//   - A key in vars with no placeholder is ignored (not an error).
func Render(tmpl string, vars map[string]string) (string, error) {
	reg := regexp.MustCompile(ParamRegex)
	var err error
	result := reg.ReplaceAllStringFunc(tmpl, func(match string) string {
		submatches := reg.FindStringSubmatch(match)
		if len(submatches) < 2 {
			return match
		}
		param := submatches[1]
		val, ok := vars[param]
		if !ok {
			err = fmt.Errorf("param %s not found", param)
			return match
		}
		return ShellQuote(val)
	})
	if err != nil {
		return "", err
	}
	return result, nil
}

// ExtractSubmitID pulls the external scheduler job id out of a submit command's
// stdout/stderr using the user-configured regex.
//
// regex must contain exactly one capture group = the id.
func ExtractSubmitID(output, regex string) (string, error) {
	reg, err := regexp.Compile(regex)
	if err != nil {
		return "", fmt.Errorf("invalid submit_id_regex %q: %w", regex, err)
	}
	result := reg.FindAllStringSubmatch(output, 1)
	if len(result) == 0 {
		return "", fmt.Errorf("no match found")
	}
	if len(result[0]) <= 1 {
		return "", fmt.Errorf("no sub group found")
	}
	return result[0][1], nil
}
