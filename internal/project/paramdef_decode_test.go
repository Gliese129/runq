package project

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

// Tolerant Default decode: hand-written `default: 42` (yaml) and js-yaml
// number payloads (json) must land as strings instead of erroring
// (feedback group 2: "cannot unmarshal number into ... Default").
func TestParamDefTolerantDecode(t *testing.T) {
	var fromYAML struct {
		Params []ParamDef `yaml:"params"`
	}
	src := `
params:
  - name: seed
    type: int
    default: 42
  - name: temperature
    type: float
    default: 0.6
  - name: sample
    type: bool
    default: false
    style: flag
  - name: lr
    type: float
    default: 1e-4
  - name: opt
    type: str
    default: adam
  - name: none_default
    type: int
    default: null
`
	if err := yaml.Unmarshal([]byte(src), &fromYAML); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	wantDefaults := []string{"42", "0.6", "false", "1e-4", "adam", ""}
	for i, w := range wantDefaults {
		if fromYAML.Params[i].Default != w {
			t.Errorf("yaml param %d default = %q, want %q", i, fromYAML.Params[i].Default, w)
		}
	}
	if fromYAML.Params[2].Style != "flag" {
		t.Errorf("style lost through custom unmarshal: %+v", fromYAML.Params[2])
	}

	var fromJSON []ParamDef
	j := `[
	  {"name":"seed","type":"int","default":42},
	  {"name":"t","type":"float","default":0.6},
	  {"name":"s","type":"bool","default":false},
	  {"name":"m","type":"str","default":"adam"},
	  {"name":"n","type":"int","default":null},
	  {"name":"x","type":"int"}
	]`
	if err := json.Unmarshal([]byte(j), &fromJSON); err != nil {
		t.Fatalf("json: %v", err)
	}
	for i, w := range []string{"42", "0.6", "false", "adam", "", ""} {
		if fromJSON[i].Default != w {
			t.Errorf("json param %d default = %q, want %q", i, fromJSON[i].Default, w)
		}
	}
}
