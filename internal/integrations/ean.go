package integrations

import (
	"strings"
	"unicode"
)

// NormalizeEAN returns digits only. It is deliberately shared by the linker
// and worker so a task is validated using exactly the same key as linking.
func NormalizeEAN(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) {
			return r
		}
		return -1
	}, value)
}
