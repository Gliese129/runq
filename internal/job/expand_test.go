package job

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestExpandGrid(t *testing.T) {
	cfg := &JobConfig{
		Project: "test",
		Sweep: []SweepBlock{{
			Method: "grid",
			Parameters: map[string]ParameterSpec{
				"lr":         {Values: []any{0.001, 0.01}},
				"batch_size": {Values: []any{32, 64}},
			},
		}},
	}
	tasks, err := Expand(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 4 {
		t.Fatalf("expected 4 tasks, got %d", len(tasks))
	}

	// Keys are sorted, so expansion order is: batch_size first, then lr.
	// batch_size=[32,64] × lr=[0.001,0.01]
	expected := []TaskParams{
		{"batch_size": 32, "lr": 0.001},
		{"batch_size": 32, "lr": 0.01},
		{"batch_size": 64, "lr": 0.001},
		{"batch_size": 64, "lr": 0.01},
	}
	for i, want := range expected {
		if !reflect.DeepEqual(tasks[i], want) {
			t.Errorf("task[%d] = %v, want %v", i, tasks[i], want)
		}
	}
}

func TestExpandList(t *testing.T) {
	cfg := &JobConfig{
		Project: "test",
		Sweep: []SweepBlock{{
			Method: "list",
			Parameters: map[string]ParameterSpec{
				"lr":         {Values: []any{0.001, 0.01, 0.1}},
				"batch_size": {Values: []any{32, 64, 128}},
			},
		}},
	}
	tasks, err := Expand(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(tasks))
	}
	expected := []TaskParams{
		{"batch_size": 32, "lr": 0.001},
		{"batch_size": 64, "lr": 0.01},
		{"batch_size": 128, "lr": 0.1},
	}
	for i, want := range expected {
		if !reflect.DeepEqual(tasks[i], want) {
			t.Errorf("task[%d] = %v, want %v", i, tasks[i], want)
		}
	}
}

func TestExpandListMismatchLength(t *testing.T) {
	cfg := &JobConfig{
		Project: "test",
		Sweep: []SweepBlock{{
			Method: "list",
			Parameters: map[string]ParameterSpec{
				"lr":         {Values: []any{0.001, 0.01}},
				"batch_size": {Values: []any{32, 64, 128}},
			},
		}},
	}
	_, err := Expand(cfg)
	if err == nil {
		t.Fatal("expected error for mismatched list lengths, got nil")
	}
	t.Logf("got expected error: %v", err)
}

func TestExpandCrossBlockProduct(t *testing.T) {
	cfg := &JobConfig{
		Project: "test",
		Sweep: []SweepBlock{
			{
				Method: "grid",
				Parameters: map[string]ParameterSpec{
					"lr":        {Values: []any{0.001, 0.01, 0.1}},
					"optimizer": {Values: []any{"adam", "sgd"}},
				},
			},
			{
				Method: "list",
				Parameters: map[string]ParameterSpec{
					"batch_size":  {Values: []any{32, 64, 128}},
					"num_workers": {Values: []any{4, 8, 16}},
				},
			},
		},
	}
	tasks, err := Expand(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// grid: 3 lr × 2 optimizer = 6, list: 3 pairs → 6 × 3 = 18
	if len(tasks) != 18 {
		t.Fatalf("expected 18 tasks, got %d", len(tasks))
	}

	// Spot-check first task: sorted keys → batch_size comes first in list block,
	// lr comes first in grid block. Cross-product order: grid[0] × list[0].
	first := tasks[0]
	if first["lr"] != 0.001 || first["optimizer"] != "adam" ||
		first["batch_size"] != 32 || first["num_workers"] != 4 {
		t.Errorf("unexpected first task: %v", first)
	}
}

func TestParameterSpecShorthand(t *testing.T) {
	shorthand := `lr: [0.001, 0.01, 0.1]`
	full := `lr:
  values: [0.001, 0.01, 0.1]`

	var short map[string]ParameterSpec
	if err := yaml.Unmarshal([]byte(shorthand), &short); err != nil {
		t.Fatalf("failed to parse shorthand: %v", err)
	}

	var long map[string]ParameterSpec
	if err := yaml.Unmarshal([]byte(full), &long); err != nil {
		t.Fatalf("failed to parse full form: %v", err)
	}

	if !reflect.DeepEqual(short["lr"].Values, long["lr"].Values) {
		t.Errorf("shorthand %v != full %v", short["lr"].Values, long["lr"].Values)
	}
}

func TestParameterSpecScalar(t *testing.T) {
	input := `lr: 0.01`
	var parsed map[string]ParameterSpec
	if err := yaml.Unmarshal([]byte(input), &parsed); err != nil {
		t.Fatalf("failed to parse scalar: %v", err)
	}
	if len(parsed["lr"].Values) != 1 {
		t.Fatalf("expected 1 value, got %d", len(parsed["lr"].Values))
	}
	if parsed["lr"].Values[0] != 0.01 {
		t.Errorf("expected 0.01, got %v", parsed["lr"].Values[0])
	}
}

func TestExpandUnknownMethod(t *testing.T) {
	cfg := &JobConfig{
		Project: "test",
		Sweep: []SweepBlock{{
			Method: "bayesian",
			Parameters: map[string]ParameterSpec{
				"lr": {Values: []any{0.01}},
			},
		}},
	}
	_, err := Expand(cfg)
	if err == nil {
		t.Fatal("expected error for unknown method, got nil")
	}
	t.Logf("got expected error: %v", err)
}

func TestExpandDuplicateKeysAcrossBlocks(t *testing.T) {
	cfg := &JobConfig{
		Project: "test",
		Sweep: []SweepBlock{
			{
				Method: "grid",
				Parameters: map[string]ParameterSpec{
					"lr": {Values: []any{0.001, 0.01}},
				},
			},
			{
				Method: "grid",
				Parameters: map[string]ParameterSpec{
					"lr": {Values: []any{0.1, 0.2}}, // duplicate key across blocks
				},
			},
		},
	}
	_, err := Expand(cfg)
	if err == nil {
		t.Fatal("expected error for duplicate keys across blocks, got nil")
	}
	t.Logf("got expected error: %v", err)
}

func TestExpandEmptyParameters(t *testing.T) {
	cfg := &JobConfig{
		Project: "test",
		Sweep: []SweepBlock{{
			Method:     "grid",
			Parameters: map[string]ParameterSpec{},
		}},
	}
	tasks, err := Expand(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks for empty parameters, got %d", len(tasks))
	}
}

func TestExpandSingleParamGrid(t *testing.T) {
	cfg := &JobConfig{
		Project: "test",
		Sweep: []SweepBlock{{
			Method: "grid",
			Parameters: map[string]ParameterSpec{
				"lr": {Values: []any{0.001, 0.01, 0.1}},
			},
		}},
	}
	tasks, err := Expand(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(tasks))
	}
	for i, v := range []any{0.001, 0.01, 0.1} {
		if tasks[i]["lr"] != v {
			t.Errorf("task[%d][lr] = %v, want %v", i, tasks[i]["lr"], v)
		}
	}
}
