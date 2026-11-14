package termio

import (
	"strings"
	"unicode/utf8"
)

// KeyReader turns raw input bytes into one printable token per key, so the
// host can see what the guest typed even when the shell doesn't echo it.
// Printable characters come through as themselves; named keys match
// Knock.app's key ribbon (⏎ ⇥ ⌫ ⎋ ⌃C ↑ ↓ → ←). Every escape sequence is one
// token, and the bytes after it are read as keys again, so an Enter inside a
// bracketed paste still shows as ⏎. Tokens never hold control or format
// characters.
type KeyReader struct {
	pending []byte // an escape sequence or UTF-8 rune split across reads
}

// maxPending bounds a split sequence; anything longer is dropped.
const maxPending = 32

// Feed returns the keys in b, in order.
func (k *KeyReader) Feed(b []byte) []string {
	data := b
	if len(k.pending) > 0 {
		data = append(k.pending, b...)
		k.pending = nil
	}
	var out []string
	for i := 0; i < len(data); {
		c := data[i]
		switch {
		case c == 0x1b:
			tok, n, partial := escapeKey(data[i:])
			if partial {
				if len(data)-i <= maxPending {
					k.pending = append([]byte(nil), data[i:]...)
				}
				return out
			}
			if tok != "" {
				out = append(out, tok)
			}
			i += n
		case c == '\r' || c == '\n':
			out = append(out, "⏎")
			if c == '\r' && i+1 < len(data) && data[i+1] == '\n' {
				i++
			}
			i++
		case c == '\t':
			out = append(out, "⇥")
			i++
		case c == 0x7f || c == 0x08:
			out = append(out, "⌫")
			i++
		case c < 0x20:
			out = append(out, "⌃"+string(rune(c+0x40)))
			i++
		case c < 0x80:
			out = append(out, string(rune(c)))
			i++
		default:
			if !utf8.FullRune(data[i:]) {
				if len(data)-i <= maxPending {
					k.pending = append([]byte(nil), data[i:]...)
				}
				return out
			}
			r, n := utf8.DecodeRune(data[i:])
			if r == utf8.RuneError {
				out = append(out, "�")
			} else if visibleRune(r) {
				out = append(out, string(r))
			}
			i += n
		}
	}
	return out
}

// escapeKey reads one escape sequence at the start of b (b[0] is ESC). It
// returns the token (empty for bracketed-paste markers), the bytes used, and
// partial when the sequence continues in the next read.
func escapeKey(b []byte) (tok string, n int, partial bool) {
	if len(b) == 1 {
		return "⎋", 1, false // a lone Esc arrives on its own
	}
	switch b[1] {
	case '[':
		j := 2
		for j < len(b) && b[j] >= 0x20 && b[j] <= 0x3f {
			j++
		}
		if j >= len(b) {
			return "", 0, true
		}
		final := b[j]
		if final < 0x40 || final > 0x7e {
			// Not a CSI after all: show Esc and read the rest as keys.
			return "⎋", 1, false
		}
		return csiKey(string(b[2:j]), final), j + 1, false
	case 'O':
		if len(b) < 3 {
			return "", 0, true
		}
		if arrow := arrowKey(b[2]); arrow != "" {
			return arrow, 3, false
		}
		if b[2] >= 0x21 && b[2] <= 0x7e {
			return "⎋O" + string(rune(b[2])), 3, false
		}
		return "⎋", 1, false
	case 0x1b:
		return "⎋", 1, false
	}
	if b[1] >= 0x21 && b[1] <= 0x7e {
		return "⎋" + string(rune(b[1])), 2, false // Option/Alt + key
	}
	return "⎋", 1, false
}

func csiKey(params string, final byte) string {
	if a := arrowKey(final); a != "" {
		return a
	}
	switch {
	case final == '~' && (params == "200" || params == "201"):
		return "" // bracketed paste start and end
	case final == '~' && params == "3":
		return "⌦"
	case final == 'Z':
		return "⇤"
	}
	// Params and final are printable ASCII by construction.
	return "⎋" + strings.TrimSpace(params) + string(rune(final))
}

func arrowKey(b byte) string {
	switch b {
	case 'A':
		return "↑"
	case 'B':
		return "↓"
	case 'C':
		return "→"
	case 'D':
		return "←"
	}
	return ""
}

// KeyRibbon keeps the last few keys for the status line.
type KeyRibbon struct {
	Max  int
	keys []string
}

// Add appends keys, dropping the oldest past Max.
func (r *KeyRibbon) Add(keys []string) {
	r.keys = append(r.keys, keys...)
	if r.Max > 0 && len(r.keys) > r.Max {
		r.keys = append(r.keys[:0], r.keys[len(r.keys)-r.Max:]...)
	}
}

// String is the keys side by side, oldest first.
func (r *KeyRibbon) String() string { return strings.Join(r.keys, "") }
