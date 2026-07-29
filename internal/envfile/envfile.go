// Package envfile parses dotenv-style KEY=VALUE files. It is shared by the
// wyze-headless env loader (which applies the result to os.Environ) and the
// Viam module creds reader (which parses straight into a Credentials value),
// so the two stay in sync on comment/quote/export handling.
package envfile

import (
	"bufio"
	"io"
	"strings"
)

// Parse reads KEY=VALUE lines into a map. Blank lines and `#` comments are
// skipped, an optional leading `export ` is stripped, and surrounding single
// or double quotes are trimmed from values. Lines without `=` or with an empty
// key are ignored.
func Parse(r io.Reader) (map[string]string, error) {
	kv := make(map[string]string)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		key, val, ok := ParseLine(scanner.Text())
		if !ok {
			continue
		}
		kv[key] = val
	}
	return kv, scanner.Err()
}

// ParseLine parses a single dotenv line, returning ok=false for blanks,
// comments, and malformed lines the caller should skip.
func ParseLine(raw string) (key, val string, ok bool) {
	line := strings.TrimSpace(raw)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")
	key, val, ok = strings.Cut(line, "=")
	if !ok {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", "", false
	}
	val = strings.Trim(strings.TrimSpace(val), `"'`)
	return key, val, true
}
