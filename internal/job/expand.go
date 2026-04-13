package job

import (
	"fmt"
	"maps"
	"slices"
)

// TaskParams is one fully-instantiated set of parameters for a single task.
// e.g. {"lr": 0.001, "batch_size": 32, "optimizer": "adam"}
type TaskParams map[string]any

// Expand takes a JobConfig and returns all TaskParams after expanding
// every SweepBlock and computing the cross-product across blocks.
//
// Each block expands independently (grid → cartesian, list → zip),
// then blocks are combined via cross-product.
func Expand(cfg *JobConfig) ([]TaskParams, error) {
	blocks := make([][]TaskParams, 0, len(cfg.Sweep))
	for _, sweep := range cfg.Sweep {
		tasks, err := expandBlock(sweep)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, tasks)
	}
	return cartesianProduct(blocks)
}

// expandBlock dispatches to the appropriate expansion strategy.
func expandBlock(block SweepBlock) ([]TaskParams, error) {
	switch block.Method {
	case "list":
		return expandList(block)
	case "grid":
		return expandGrid(block)
	default:
		return nil, fmt.Errorf("unknown sweep method %q, expected \"grid\" or \"list\"", block.Method)
	}
}

// expandList zips parameters 1-to-1. All parameter lists must have the same length.
func expandList(block SweepBlock) ([]TaskParams, error) {
	if len(block.Parameters) == 0 {
		return []TaskParams{}, nil
	}
	keys := slices.Sorted(maps.Keys(block.Parameters))
	length := len(block.Parameters[keys[0]].Values)
	for _, key := range keys[1:] {
		if n := len(block.Parameters[key].Values); n != length {
			return nil, fmt.Errorf(
				"list sweep: parameter %q has %d values, but %q has %d (all must be the same length)",
				key, n, keys[0], length,
			)
		}
	}
	results := make([]TaskParams, 0, length)
	for i := 0; i < length; i++ {
		params := make(TaskParams, len(keys))
		for _, key := range keys {
			params[key] = block.Parameters[key].Values[i]
		}
		results = append(results, params)
	}
	return results, nil
}

// expandGrid computes the cartesian product of all parameters within a block.
// Keys are sorted to guarantee deterministic output order.
func expandGrid(block SweepBlock) ([]TaskParams, error) {
	if len(block.Parameters) == 0 {
		return []TaskParams{}, nil
	}
	keys := slices.Sorted(maps.Keys(block.Parameters))
	caps := 1
	for _, p := range block.Parameters {
		caps *= len(p.Values)
	}
	results := make([]TaskParams, 0, caps)
	var dfs func(idx int, curr TaskParams)
	dfs = func(idx int, curr TaskParams) {
		if idx == len(keys) {
			result := make(TaskParams, len(curr))
			maps.Copy(result, curr)
			results = append(results, result)
			return
		}
		key := keys[idx]
		for _, value := range block.Parameters[key].Values {
			curr[key] = value
			dfs(idx+1, curr)
			delete(curr, key)
		}
	}
	dfs(0, make(TaskParams))
	return results, nil
}

// cartesianProduct computes the cross-product of N []TaskParams slices.
// Empty blocks are skipped. Returns error if any parameter key appears in
// more than one block.
func cartesianProduct(xs [][]TaskParams) ([]TaskParams, error) {
	// Filter out empty blocks (e.g. block with empty parameters)
	filtered := make([][]TaskParams, 0, len(xs))
	for _, x := range xs {
		if len(x) > 0 {
			filtered = append(filtered, x)
		}
	}
	xs = filtered
	if len(xs) == 0 {
		return []TaskParams{}, nil
	}

	// Check for duplicate parameter keys across blocks
	seen := map[string]bool{}
	totalLen := 1
	for _, x := range xs {
		totalLen *= len(x)
		for k := range x[0] {
			if seen[k] {
				return nil, fmt.Errorf("duplicate parameter %q found across sweep blocks", k)
			}
			seen[k] = true
		}
	}
	results := make([]TaskParams, 0, totalLen)

	var dfs func(idx int, curr TaskParams)
	dfs = func(idx int, curr TaskParams) {
		if idx == len(xs) {
			result := make(TaskParams, len(curr))
			maps.Copy(result, curr)
			results = append(results, result)
			return
		}
		x := xs[idx]
		for _, p := range x {
			maps.Copy(curr, p)
			dfs(idx+1, curr)
			for k := range p {
				delete(curr, k)
			}
		}
	}
	dfs(0, make(TaskParams))
	return results, nil
}
