// Package termio holds the raw-terminal pieces used during a session: the
// Ctrl-] escape, the host's status line and terminal state.
package termio

// EscapeKey is Ctrl-].
const EscapeKey = 0x1d

// Escaper finds "Ctrl-] then <key>" in a raw input stream. Bytes that are
// not part of an escape pass through unchanged; Ctrl-] twice sends one
// literal Ctrl-].
type Escaper struct {
	// Commands are the keys that mean something after Ctrl-].
	Commands string
	armed    bool
}

// Feed splits p into bytes to pass on and commands seen, in order. Each
// returned Chunk is either data (Cmd == 0) or a command.
func (e *Escaper) Feed(p []byte) []Chunk {
	var out []Chunk
	var data []byte
	flush := func() {
		if len(data) > 0 {
			out = append(out, Chunk{Data: data})
			data = nil
		}
	}
	for _, b := range p {
		if !e.armed {
			if b == EscapeKey {
				e.armed = true
				continue
			}
			data = append(data, b)
			continue
		}
		e.armed = false
		switch {
		case b == EscapeKey:
			data = append(data, EscapeKey)
		case isCommand(e.Commands, b):
			flush()
			out = append(out, Chunk{Cmd: lower(b)})
		default:
			data = append(data, EscapeKey, b)
		}
	}
	flush()
	return out
}

// Armed reports whether a Ctrl-] is waiting for its second key.
func (e *Escaper) Armed() bool { return e.armed }

// Chunk is a run of input bytes or one escape command.
type Chunk struct {
	Data []byte
	Cmd  byte
}

func lower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

func isCommand(cmds string, b byte) bool {
	for i := 0; i < len(cmds); i++ {
		if cmds[i] == lower(b) {
			return true
		}
	}
	return false
}
