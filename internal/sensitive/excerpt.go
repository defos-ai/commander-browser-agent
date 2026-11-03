package sensitive

import (
	"context"
	"strings"
	"unicode/utf8"
)

// Excerpt protects a bounded source window before whitespace normalization and
// truncation. Callers must include MaxValueBytes-1 bytes of lookahead beyond the
// intended excerpt, so a known credential crossing its boundary is still whole.
func Excerpt(ctx context.Context, value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	end := min(len(value), limit)
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	// Retain only the common protected prefix: shrinking earlier matches must
	// not expose an unprotected tail from the additional source window.
	full := Redact(ctx, value)
	prefix := Redact(ctx, value[:end])
	common := 0
	for common < len(full) && common < len(prefix) && full[common] == prefix[common] {
		common++
	}
	common = protectedBoundary(full, common)
	text := strings.Join(strings.Fields(full[:common]), " ")
	if len(text) > limit {
		text = text[:protectedBoundary(text, limit)]
	}
	return text
}

// A credential prefix can coincide with the public replacement marker. Keeping
// only part of that marker would expose the prefix instead of a redaction.
func protectedBoundary(text string, end int) int {
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	start := max(0, end-len(replacement)+1)
	stop := min(len(text), end+len(replacement)-1)
	if offset := strings.Index(text[start:stop], replacement); offset >= 0 {
		marker := start + offset
		if marker < end && marker+len(replacement) > end {
			return marker
		}
	}
	return end
}
