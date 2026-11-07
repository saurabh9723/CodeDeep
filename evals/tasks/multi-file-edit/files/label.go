package multifix

import "fmt"

// Label renders a short equation for a and b.
func Label(a, b int) string {
	return fmt.Sprintf("%d+%d=%d", a, b, Add(a, b))
}