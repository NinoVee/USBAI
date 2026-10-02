package rag

import "strings"

// Chunk splits text into pieces of roughly size characters, preferring
// paragraph, then sentence, then word boundaries, with overlap characters of
// context repeated between neighbouring chunks.
func Chunk(text string, size, overlap int) []string {
	if size <= 0 {
		size = 1200
	}
	if overlap < 0 || overlap >= size {
		overlap = size / 6
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	runes := []rune(text)
	if len(runes) <= size {
		return []string{text}
	}

	var chunks []string
	start := 0
	for start < len(runes) {
		end := start + size
		if end >= len(runes) {
			end = len(runes)
		} else {
			end = breakPoint(runes, start, end)
		}
		c := strings.TrimSpace(string(runes[start:end]))
		if c != "" {
			chunks = append(chunks, c)
		}
		if end == len(runes) {
			break
		}
		next := end - overlap
		if next <= start {
			next = end
		}
		// Start the overlap on a word boundary.
		for next < end && !isSpace(runes[next]) {
			next++
		}
		start = next
	}
	return chunks
}

// breakPoint finds a natural place to end a chunk in the last half of
// runes[start:end].
func breakPoint(runes []rune, start, end int) int {
	min := start + (end-start)/2
	for i := end - 1; i > min; i-- {
		if runes[i] == '\n' && runes[i-1] == '\n' {
			return i + 1
		}
	}
	for i := end - 1; i > min; i-- {
		if (runes[i-1] == '.' || runes[i-1] == '?' || runes[i-1] == '!' || runes[i-1] == '\n') && isSpace(runes[i]) {
			return i
		}
	}
	for i := end - 1; i > min; i-- {
		if isSpace(runes[i]) {
			return i
		}
	}
	return end
}

func isSpace(r rune) bool { return r == ' ' || r == '\n' || r == '\t' }
