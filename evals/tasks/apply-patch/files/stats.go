package multifix

// Average returns the mean of a and b.
func Average(a, b int) float64 {
	return float64(Add(a, b)) / 2
}