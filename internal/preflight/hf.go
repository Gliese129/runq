package preflight

import (
	"regexp"
	"strings"
)

// HuggingFace reference detection (RQ-76 ②, feedback group 2).
//
// The single biggest class of "queued 2h, then died in 10s" failures the
// preflight missed: the script references a Hub repo (`from_pretrained`,
// `load_dataset`, ...) that is not cached locally and cannot be resolved
// from the compute node (no internet / wrong id / gated without token).
//
// Detection is regex-over-source, same discipline as import extraction:
// only STRING LITERALS are picked up. A repo id built at runtime
// (f-strings, variables) is invisible — that is a documented limitation,
// not a bug; the run-time evidence chain (RQ-74) still catches it.

// HFRef is one Hub repository the script references.
type HFRef struct {
	RepoID   string `json:"repo_id"`
	RepoType string `json:"repo_type"` // "model" | "dataset"
}

// hfModelCallRegex catches model-repo loads:
//
//	AutoModel.from_pretrained("org/name")
//	snapshot_download("org/name") / snapshot_download(repo_id="org/name")
//	hf_hub_download(repo_id="org/name", filename=...)
var hfModelCallRegex = regexp.MustCompile(
	`(?:from_pretrained|snapshot_download|hf_hub_download)\(\s*(?:repo_id\s*=\s*)?["']([^"'\s]+)["']`)

// hfDatasetCallRegex catches dataset loads: load_dataset("org/name") /
// load_dataset(path="org/name").
var hfDatasetCallRegex = regexp.MustCompile(
	`load_dataset\(\s*(?:path\s*=\s*)?["']([^"'\s]+)["']`)

// hfRepoIDRegex is the shape of a plausible Hub repo id: `name` or
// `org/name`. Everything else the call regexes matched (local paths,
// URLs, format strings) is discarded.
var hfRepoIDRegex = regexp.MustCompile(`^[A-Za-z0-9][\w.\-]*(?:/[A-Za-z0-9][\w.\-]*)?$`)

// looksLikeHFRepoID filters call-site matches down to real Hub ids.
// Local paths (absolute, relative, home-relative) and templated strings
// are the caller's business, not the Hub's.
func looksLikeHFRepoID(s string) bool {
	if s == "" || strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "~") {
		return false
	}
	if strings.Contains(s, "{") || strings.Contains(s, "$") {
		return false // f-string / template fragment — not a literal id
	}
	return hfRepoIDRegex.MatchString(s)
}

// ExtractHFRefs scans python source for Hub references. Deduped, in
// order of first appearance. Model and dataset namespaces are distinct
// on the Hub, so the same id may legitimately appear as both.
func ExtractHFRefs(src []byte) []HFRef {
	var refs []HFRef
	seen := map[string]bool{}
	add := func(id, repoType string) {
		if !looksLikeHFRepoID(id) {
			return
		}
		key := repoType + ":" + id
		if seen[key] {
			return
		}
		seen[key] = true
		refs = append(refs, HFRef{RepoID: id, RepoType: repoType})
	}
	for _, m := range hfModelCallRegex.FindAllStringSubmatch(string(src), -1) {
		add(m[1], "model")
	}
	for _, m := range hfDatasetCallRegex.FindAllStringSubmatch(string(src), -1) {
		add(m[1], "dataset")
	}
	return refs
}

// DownloadCommand renders the ready-made pre-download command for a ref.
// Surfaced verbatim in warnings so the CLI user can copy-paste it and the
// WebUI can inject it into the project's setup command in one click.
func (r HFRef) DownloadCommand() string {
	if r.RepoType == "dataset" {
		return "huggingface-cli download --repo-type dataset " + r.RepoID
	}
	return "huggingface-cli download " + r.RepoID
}
