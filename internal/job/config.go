package job

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// JobConfig represents a parsed job.yaml file.
type JobConfig struct {
	Project     string       `yaml:"project"`
	Description string       `yaml:"description,omitempty"`
	Sweep       []SweepBlock `yaml:"sweep"`
	Overrides   *Overrides   `yaml:"overrides,omitempty"`
}

// SweepBlock is one block inside the sweep array.
// Blocks expand independently, then cross-product across blocks.
type SweepBlock struct {
	Method     string                   `yaml:"method"` // "grid" or "list"
	Parameters map[string]ParameterSpec `yaml:"parameters"`
}

// ParameterSpec supports three input forms via custom UnmarshalYAML:
//
//	shorthand:  lr: [0.001, 0.01, 0.1]          → Values = [0.001, 0.01, 0.1]
//	scalar:     lr: 0.01                         → Values = [0.01]
//	full:       lr: { values: [0.001, 0.01, 0.1] } → Values = [0.001, 0.01, 0.1]
type ParameterSpec struct {
	Values []any `yaml:"values"`
	// Reserved for future sweep methods (random, bayesian):
	// Distribution string  `yaml:"distribution,omitempty"`
	// Min          float64 `yaml:"min,omitempty"`
	// Max          float64 `yaml:"max,omitempty"`
	// Count        int     `yaml:"count,omitempty"`
}

// Overrides are job-level overrides for project defaults.
type Overrides struct {
	GPUsPerTask *int              `yaml:"gpus_per_task,omitempty"`
	MaxRetry    *int              `yaml:"max_retry,omitempty"`
	Env         map[string]string `yaml:"env,omitempty"`
}

func (p *ParameterSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.SequenceNode:
		return value.Decode(&p.Values)
	case yaml.ScalarNode:
		var result any
		if err := value.Decode(&result); err != nil {
			return err
		}
		p.Values = []any{result}
		return nil
	case yaml.MappingNode:
		// Shadow type breaks recursion: raw has the same fields but no
		// UnmarshalYAML method, so yaml.v3 uses default struct decoding.
		type raw ParameterSpec
		var r raw
		if err := value.Decode(&r); err != nil {
			return err
		}
		*p = ParameterSpec(r)
		return nil
	default:
		return fmt.Errorf("unsupported YAML node kind %d for parameter spec", value.Kind)
	}
}
