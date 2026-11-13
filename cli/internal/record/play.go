package record

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// PlayOptions control replay speed.
type PlayOptions struct {
	Speed   float64       // 1 is real time
	MaxIdle time.Duration // longer pauses are shortened to this; 0 keeps them
	Sleep   func(context.Context, time.Duration) error
}

// Play replays an asciicast v2 stream to w with its timings.
func Play(ctx context.Context, r io.Reader, w io.Writer, opt PlayOptions) (Header, error) {
	if opt.Speed <= 0 {
		opt.Speed = 1
	}
	if opt.Sleep == nil {
		opt.Sleep = sleepCtx
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	var h Header
	if !sc.Scan() {
		return h, fmt.Errorf("the recording is empty")
	}
	if err := json.Unmarshal(sc.Bytes(), &h); err != nil || h.Version != 2 {
		return h, fmt.Errorf("that isn't an asciicast v2 recording")
	}
	last := 0.0
	for sc.Scan() {
		var ev []json.RawMessage
		if json.Unmarshal(sc.Bytes(), &ev) != nil || len(ev) < 3 {
			continue
		}
		var t float64
		var kind, data string
		if json.Unmarshal(ev[0], &t) != nil || json.Unmarshal(ev[1], &kind) != nil || json.Unmarshal(ev[2], &data) != nil {
			continue
		}
		gap := time.Duration((t - last) / opt.Speed * float64(time.Second))
		if opt.MaxIdle > 0 && gap > opt.MaxIdle {
			gap = opt.MaxIdle
		}
		last = t
		if gap > 0 {
			if err := opt.Sleep(ctx, gap); err != nil {
				return h, err
			}
		}
		if kind == "o" {
			if _, err := io.WriteString(w, data); err != nil {
				return h, err
			}
		}
	}
	return h, sc.Err()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
