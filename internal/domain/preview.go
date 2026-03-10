package domain

// DefaultPreviewLen is the default max length for log previews.
const DefaultPreviewLen = 80

// Preview trunca el string a maxLen caracteres y agrega "…" si fue recortado.
func Preview(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "…"
}
