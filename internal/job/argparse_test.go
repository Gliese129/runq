package job

import (
	"fmt"
	"reflect"
	"testing"
)

func TestPyArgParse(t *testing.T) {
	file := "../../examples/test_train.py"
	args, err := ScanArgparse(file)
	if err != nil {
		fmt.Printf("%v", err)
	}

	expected := []ArgInfo{
		{"lr", "float", "0.001"},
		{"batch-size", "int", "32"},
		{"optimizer", "", "adam"},
		{"resume", "bool", "false"},
	}
	for i, want := range expected {
		if !reflect.DeepEqual(args[i], want) {
			t.Errorf("task[%d] = %v, want %v", i, args[i], want)
		}
	}
}
