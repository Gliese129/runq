package submitplan

import (
	"strings"
	"testing"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
)

// Typed submit validation: an int/float param carrying an unparseable
// value ("None" replayed from an old job) fails AT SUBMIT with the param
// named — never two hours later in argparse on a compute node.
func TestValidateParamTypes(t *testing.T) {
	proj := &project.Config{Params: []project.ParamDef{
		{Name: "max_new_tokens", Type: "int"},
		{Name: "temperature", Type: "float"},
		{Name: "sample", Type: "bool"},
		{Name: "note", Type: "str"},
	}}

	ok := []job.TaskParams{{"max_new_tokens": 128, "temperature": "0.6", "sample": "false", "note": "None"}}
	if err := validateParamTypes(proj, ok); err != nil {
		t.Fatalf("valid values rejected: %v", err)
	}

	bad := []job.TaskParams{{"max_new_tokens": "None"}}
	err := validateParamTypes(proj, bad)
	if err == nil || !strings.Contains(err.Error(), "max_new_tokens") || !strings.Contains(err.Error(), "OMITTED") {
		t.Fatalf("int None: %v", err)
	}

	if err := validateParamTypes(proj, []job.TaskParams{{"temperature": "warm"}}); err == nil {
		t.Fatal("float garbage passed")
	}
	if err := validateParamTypes(proj, []job.TaskParams{{"sample": "None"}}); err == nil {
		t.Fatal("bool None passed")
	}
	// Absent params are fine — omission is the correct way to not set one.
	if err := validateParamTypes(proj, []job.TaskParams{{"note": "x"}}); err != nil {
		t.Fatalf("absent typed params rejected: %v", err)
	}
}
