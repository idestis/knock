package config

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// parseTOML reads the subset of TOML that config.toml uses: [table] headers
// and flat key = value lines (strings, integers, booleans), with # comments.
// Keys come back as "table.key".
func parseTOML(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	table := ""
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			end := strings.Index(line, "]")
			if end < 0 || strings.HasPrefix(line, "[[") {
				return nil, fmt.Errorf("line %d: bad table header", n)
			}
			table = strings.TrimSpace(line[1:end])
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected key = value", n)
		}
		key = strings.Trim(strings.TrimSpace(key), `"`)
		val, err := parseValue(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		if table != "" {
			key = table + "." + key
		}
		out[key] = val
	}
	return out, sc.Err()
}

func parseValue(s string) (string, error) {
	switch {
	case strings.HasPrefix(s, `"`):
		end := closingQuote(s)
		if end < 0 {
			return "", fmt.Errorf("unterminated string")
		}
		v, err := strconv.Unquote(s[:end+1])
		if err != nil {
			return "", fmt.Errorf("bad string: %w", err)
		}
		return v, nil
	case strings.HasPrefix(s, "'"):
		end := strings.Index(s[1:], "'")
		if end < 0 {
			return "", fmt.Errorf("unterminated string")
		}
		return s[1 : end+1], nil
	}
	if i := strings.Index(s, "#"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if s == "" {
		return "", fmt.Errorf("missing value")
	}
	return s, nil
}

// closingQuote finds the closing double quote, skipping escaped ones.
func closingQuote(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}
