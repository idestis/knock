package termio

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/idestis/knock/protocol"
)

// maxNameRunes caps a name from the relay before it reaches the screen.
const maxNameRunes = 64

// CleanText drops control and format characters (escape sequences, bidi
// overrides, zero-width joiners, line separators) from text the relay or a
// peer chose, so it can't redraw the terminal or disguise itself. Invalid
// UTF-8 becomes U+FFFD. Long names are cut short.
func CleanText(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= maxNameRunes {
			b.WriteRune('…')
			break
		}
		if !visibleRune(r) {
			continue
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// visibleRune reports whether r is safe to print as-is.
func visibleRune(r rune) bool {
	switch {
	case r == utf8.RuneError:
		return true // shown as the replacement character
	case unicode.Is(unicode.Cc, r), unicode.Is(unicode.Cf, r),
		unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r),
		unicode.Is(unicode.Co, r), unicode.Is(unicode.Cs, r):
		return false
	}
	return true
}

// CleanIdentity returns id with every printable field cleaned.
func CleanIdentity(id protocol.Identity) protocol.Identity {
	id.Name = CleanText(id.Name)
	id.Email = CleanText(id.Email)
	id.Login = CleanText(id.Login)
	id.Provider = CleanText(id.Provider)
	return id
}

// CleanStart cleans the names in a session.start before anything prints
// them or hands them to the shell.
func CleanStart(st protocol.SessionStart) protocol.SessionStart {
	st.Peer = CleanIdentity(st.Peer)
	st.PeerDevice = CleanText(st.PeerDevice)
	return st
}
