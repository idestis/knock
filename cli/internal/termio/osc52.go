package termio

import "io"

// ClipboardFilter drops OSC 52 (set or query the clipboard) from a byte
// stream and passes everything else through byte for byte. The guest runs
// host output through it, so the host's shell can't write to, or read, the
// guest's clipboard.
//
// It recognises the 7-bit introducer ESC ], the 8-bit C1 introducer 0x9d
// (when it isn't part of a UTF-8 character) and its UTF-8 spelling C2 9D,
// with the number written any way a terminal would parse it ("52", "052").
// The sequence ends at BEL, ST (ESC \, 0x9c), CAN or SUB; an ESC that
// starts anything else also ends it, and that ESC is kept. Sequences split
// across reads are handled: only the few bytes of an introducer that might
// still turn out to be OSC 52 are held back until the next read.
//
// The zero value is ready to use.
type ClipboardFilter struct {
	st      filterState
	pending []byte // held-back introducer bytes, at most maxPending
	num     int    // OSC number parsed so far
	digits  int
	dropped int // bytes of the current OSC 52 dropped so far
	utf8    int // UTF-8 continuation bytes still expected
}

type filterState uint8

const (
	stGround  filterState = iota
	stEsc                 // saw ESC
	stC2                  // saw C2, maybe the UTF-8 form of 0x9d
	stNum                 // inside an OSC introducer, reading its number
	stDrop                // inside an OSC 52, dropping
	stDropEsc             // saw ESC inside an OSC 52
)

const (
	maxOSCDigits = 6
	// MaxClipboardOSC caps how much of one OSC 52 is dropped. Past it the
	// filter gives up on finding the end and goes back to passing bytes
	// through; the introducer is long gone, so what follows is plain text
	// to the terminal, never a clipboard write.
	MaxClipboardOSC = 1 << 20
)

// Filter returns p without OSC 52 sequences, as a new slice. Bytes that
// might start one are held back and come out with the next call (or Flush).
func (f *ClipboardFilter) Filter(p []byte) []byte {
	out := make([]byte, 0, len(p)+len(f.pending))
	for i := 0; i < len(p); {
		b := p[i]
		switch f.st {
		case stGround:
			switch {
			case b == 0x1b:
				f.hold(stEsc, b)
			case b == 0xc2:
				f.hold(stC2, b)
			case b == 0x9d && f.utf8 == 0:
				f.hold(stNum, b)
			default:
				out = append(out, b)
				f.trackUTF8(b)
			}
			i++
		case stEsc:
			if b == ']' {
				f.pending = append(f.pending, b)
				f.st, f.num, f.digits = stNum, 0, 0
				i++
				continue
			}
			out = f.release(out) // not an OSC: reconsider b from scratch
		case stC2:
			if b == 0x9d {
				f.pending = append(f.pending, b)
				f.st, f.num, f.digits = stNum, 0, 0
				i++
				continue
			}
			out = f.release(out)
			f.utf8 = 1 // C2 was a lead byte after all
		case stNum:
			switch {
			case b >= '0' && b <= '9' && f.digits < maxOSCDigits:
				f.pending = append(f.pending, b)
				f.num = f.num*10 + int(b-'0')
				f.digits++
				i++
			case b == ';' && f.digits > 0 && f.num == 52:
				f.pending = f.pending[:0]
				f.st, f.dropped = stDrop, 0
				i++
			default:
				out = f.release(out) // another OSC, or not one at all
			}
		case stDrop:
			i++
			f.dropped++
			switch b {
			case 0x07, 0x9c, 0x18, 0x1a: // BEL, 8-bit ST, CAN, SUB
				f.st = stGround
				f.utf8 = 0
			case 0x1b:
				f.st = stDropEsc
			}
			if f.st == stDrop && f.dropped > MaxClipboardOSC {
				f.st, f.utf8 = stGround, 0
			}
		case stDropEsc:
			f.st, f.utf8 = stGround, 0
			if b == '\\' {
				i++ // ST
				continue
			}
			// Some other sequence starts here and ends the OSC.
			f.hold(stEsc, 0x1b)
		}
	}
	return out
}

// Flush returns bytes held back at the end of the stream. A partial OSC 52
// that never ended is dropped.
func (f *ClipboardFilter) Flush() []byte {
	var out []byte
	switch f.st {
	case stEsc, stC2, stNum:
		out = append(out, f.pending...)
	}
	*f = ClipboardFilter{}
	return out
}

func (f *ClipboardFilter) hold(st filterState, b byte) {
	f.st = st
	f.pending = append(f.pending[:0], b)
}

// release writes the held-back bytes as they were and returns to ground.
func (f *ClipboardFilter) release(out []byte) []byte {
	out = append(out, f.pending...)
	f.pending = f.pending[:0]
	f.st, f.utf8 = stGround, 0
	return out
}

// trackUTF8 follows UTF-8 so a 0x9d continuation byte is never taken for C1.
func (f *ClipboardFilter) trackUTF8(b byte) {
	switch {
	case f.utf8 > 0 && b&0xc0 == 0x80:
		f.utf8--
	case b >= 0xc2 && b <= 0xdf:
		f.utf8 = 1
	case b >= 0xe0 && b <= 0xef:
		f.utf8 = 2
	case b >= 0xf0 && b <= 0xf4:
		f.utf8 = 3
	default:
		f.utf8 = 0
	}
}

// ClipboardWriter is an io.Writer that drops OSC 52 before writing to W.
// Call Flush when the stream ends.
type ClipboardWriter struct {
	W io.Writer
	f ClipboardFilter
}

// NewClipboardWriter wraps w.
func NewClipboardWriter(w io.Writer) *ClipboardWriter { return &ClipboardWriter{W: w} }

func (c *ClipboardWriter) Write(p []byte) (int, error) {
	if out := c.f.Filter(p); len(out) > 0 {
		if _, err := c.W.Write(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush writes anything held back.
func (c *ClipboardWriter) Flush() error {
	if out := c.f.Flush(); len(out) > 0 {
		_, err := c.W.Write(out)
		return err
	}
	return nil
}
