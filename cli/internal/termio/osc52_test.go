package termio

import (
	"bytes"
	"strings"
	"testing"
)

// filterAll runs chunks through one filter, as separate reads.
func filterAll(chunks ...string) string {
	var f ClipboardFilter
	var out []byte
	for _, c := range chunks {
		out = append(out, f.Filter([]byte(c))...)
	}
	return string(append(out, f.Flush()...))
}

// everySplit feeds s one byte at a time and at every two-way split.
func everySplit(t *testing.T, s, want string) {
	t.Helper()
	if got := filterAll(s); got != want {
		t.Errorf("whole: %q -> %q, want %q", s, got, want)
	}
	bytewise := make([]string, len(s))
	for i := 0; i < len(s); i++ {
		bytewise[i] = s[i : i+1]
	}
	if got := filterAll(bytewise...); got != want {
		t.Errorf("byte by byte: %q -> %q, want %q", s, got, want)
	}
	for i := 1; i < len(s); i++ {
		if got := filterAll(s[:i], s[i:]); got != want {
			t.Errorf("split at %d: %q -> %q, want %q", i, s, got, want)
		}
	}
}

func TestClipboardFilterDropsOSC52(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"BEL", "a\x1b]52;c;aGVsbG8=\x07b", "ab"},
		{"ST", "a\x1b]52;c;aGVsbG8=\x1b\\b", "ab"},
		{"8-bit ST", "a\x1b]52;c;aGVsbG8=\x9cb", "ab"},
		{"query", "a\x1b]52;c;?\x07b", "ab"},
		{"empty selection", "a\x1b]52;;aGk=\x07b", "ab"},
		{"leading zero", "a\x1b]052;c;aGk=\x07b", "ab"},
		{"C1 introducer", "a\x9d52;c;aGk=\x07b", "ab"},
		{"UTF-8 C1 introducer", "a\xc2\x9d52;c;aGk=\xc2\x9cb", "ab"},
		{"two in a row", "\x1b]52;c;YQ==\x07\x1b]52;p;Yg==\x1b\\x", "x"},
		{"tmux passthrough", "\x1bPtmux;\x1b\x1b]52;c;aGk=\x07\x1b\\x", "\x1bPtmux;\x1b\x1b\\x"},
		{"CAN aborts", "a\x1b]52;c;aGk=\x18b", "ab"},
		{"ESC starts something else", "a\x1b]52;c;aGk=\x1b[31mred", "a\x1b[31mred"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { everySplit(t, tt.in, tt.want) })
	}
}

func TestClipboardFilterPassesEverythingElse(t *testing.T) {
	for _, s := range []string{
		"plain text\r\n",
		"\x1b]0;my title\x07prompt$ ",
		"\x1b]2;title\x1b\\",
		"\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\",
		"\x1b]5;0;red\x07",         // OSC 5, not 52
		"\x1b]520;x\x07",           // OSC 520
		"\x1b]52\x07",              // no ';', no payload
		"\x1b]133;A\x07",           // shell integration
		"\x1b[2J\x1b[H\x1b[?1049h", // CSI
		"\x1b\x1b]0;t\x07",         // ESC ESC
		"Ν Ý ʝ \u009d? no: ",       // UTF-8 with 0x9d continuation bytes
		"\xce\x9d52;c;aGk=\x07 ok", // Greek capital Nu then "52;": text, not C1
		"\xc2\xa0nbsp \xc2",        // C2 lead bytes that aren't C1
		"trailing esc \x1b",
		"\x1b]",
		"\x1b]52",
	} {
		everySplit(t, s, s)
	}
}

func TestClipboardFilterUnterminated(t *testing.T) {
	// Held back until the stream ends, then dropped.
	if got := filterAll("a\x1b]52;c;aGVsbG8="); got != "a" {
		t.Fatalf("got %q", got)
	}
	// A runaway sequence is dropped up to the cap and then the filter
	// resyncs: later output reaches the screen, and nothing before the cap
	// was ever passed through.
	var f ClipboardFilter
	out := f.Filter([]byte("x\x1b]52;c;"))
	junk := bytes.Repeat([]byte("A"), MaxClipboardOSC-len("c;"))
	out = append(out, f.Filter(junk)...)
	if string(out) != "x" {
		t.Fatalf("payload leaked before the cap: %d bytes", len(out))
	}
	out = append(out, f.Filter([]byte("AAAAhello\x1b]0;t\x07"))...)
	if !strings.HasSuffix(string(out), "hello\x1b]0;t\x07") {
		t.Fatalf("didn't resync: %q", out[len(out)-20:])
	}
	if strings.Contains(string(out), "\x1b]52") {
		t.Fatal("an OSC 52 introducer got through")
	}
}

func TestClipboardWriter(t *testing.T) {
	var buf bytes.Buffer
	w := NewClipboardWriter(&buf)
	for _, c := range []string{"hi \x1b]5", "2;c;aGk=", "\x07there \x1b"} {
		if n, err := w.Write([]byte(c)); err != nil || n != len(c) {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "hi there \x1b" {
		t.Fatalf("got %q", buf.String())
	}
}
