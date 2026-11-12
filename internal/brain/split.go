package brain

import "unicode"

// splitReply preserves the complete reply. A conservative UTF-16 budget also
// accounts for emoji outside the BMP; splitting prefers line and word boundaries.
func splitReply(text string) []string {
	const limit = 4000
	var parts []string
	for len(text) > 0 {
		end, units, line, space := 0, 0, 0, 0
		for i, r := range text {
			n := 1
			if r > 0xffff {
				n = 2
			}
			if units+n > limit {
				break
			}
			units += n
			end = i + len(string(r))
			if r == '\n' {
				line = end
			}
			if unicode.IsSpace(r) {
				space = end
			}
		}
		if end < len(text) {
			if line >= end/2 {
				end = line
			} else if space >= end/2 {
				end = space
			}
		}
		parts = append(parts, text[:end])
		text = text[end:]
	}
	return parts
}
