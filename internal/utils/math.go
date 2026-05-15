package utils

// Normalization scales all values in the map to sum to 1.0.
// Returns a new map; the input is not modified.
// Uses a small epsilon (1e-8) to avoid division by zero when all values are 0.
func Normalization[T comparable](xs map[T]float64) map[T]float64 {
	sum := 1e-8
	for _, v := range xs {
		sum += v
	}
	res := make(map[T]float64, len(xs))
	for k, v := range xs {
		res[k] = v / sum
	}
	return res
}
