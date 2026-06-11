package job

import "testing"

func TestSanitizeJobName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"3385434e", "rq-3385434e"},          // SGE: cannot start with a digit
		{"eval llama 3/8B", "eval-llama-3-8B"}, // spaces, slash → '-'
		{"--weird--", "weird"},               // trim + collapse separators
		{"", "rq"},                           // empty stays valid
		{"模型eval", "eval"},                   // non-ASCII → '-' then trimmed
		{"42", "rq-42"},                      // all-digit → prefixed
		{"ok_name.v2", "ok_name.v2"},         // already valid: untouched
	}
	for _, c := range cases {
		if got := SanitizeJobName(c.in); got != c.want {
			t.Errorf("SanitizeJobName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	long := make([]byte, 100)
	for i := range long {
		long[i] = 'a'
	}
	if got := SanitizeJobName(string(long)); len(got) != 64 {
		t.Errorf("length cap: got %d", len(got))
	}
}

func TestRenderJobName(t *testing.T) {
	params := TaskParams{"model": "llama-3", "h_rt": "4:00:00"}
	builtins := map[string]string{"project": "evaluate", "job_id": "1a2b", "task_id": "3385434e"}

	// default template — never digit-first
	if got := RenderJobName("", params, builtins); got != "rq-3385434e" {
		t.Errorf("default: %q", got)
	}
	// params + builtins
	if got := RenderJobName("{{project}}-{{model}}", params, builtins); got != "evaluate-llama-3" {
		t.Errorf("mixed: %q", got)
	}
	// unknown placeholder renders empty, separators collapse — cosmetic, never an error
	if got := RenderJobName("eval-{{nope}}-x", params, builtins); got != "eval-x" {
		t.Errorf("unknown: %q", got)
	}
	// values needing sanitization (h_rt contains ':')
	if got := RenderJobName("t{{h_rt}}", params, builtins); got != "t4-00-00" {
		t.Errorf("sanitize: %q", got)
	}
}
