package multifix

import "testing"

func TestSum(t *testing.T) {
	if got := Sum(2, 3); got != 5 {
		t.Fatalf("Sum(2, 3) = %d, want 5", got)
	}
	if got := Label(2, 3); got != "2+3=5" {
		t.Fatalf("Label(2, 3) = %q, want 2+3=5", got)
	}
	if got := Average(2, 4); got != 3 {
		t.Fatalf("Average(2, 4) = %v, want 3", got)
	}
}