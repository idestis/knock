package ui

import (
	"context"
	"errors"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

// ErrCancelled is returned when the user presses Ctrl-C at a spinner.
var ErrCancelled = errors.New("cancelled")

type doneMsg struct {
	v   any
	err error
}

type spin struct {
	s     spinner.Model
	label string
	done  bool
	res   doneMsg
}

func (m spin) Init() tea.Cmd { return m.s.Tick }

func (m spin) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" || msg.String() == "esc" {
			m.done, m.res = true, doneMsg{err: ErrCancelled}
			return m, tea.Quit
		}
	case doneMsg:
		m.done, m.res = true, msg
		return m, tea.Quit
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.s, cmd = m.s.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m spin) View() tea.View {
	if m.done {
		return tea.NewView("")
	}
	return tea.NewView(m.s.View() + " " + m.label + "\n")
}

// Spin shows label with a spinner while work runs. Ctrl-C cancels the
// context given to work and returns ErrCancelled.
func Spin[T any](ctx context.Context, label string, work func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := spin{s: spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(Brand)), label: label}
	p := tea.NewProgram(m, tea.WithOutput(tty))
	go func() {
		v, err := work(ctx)
		p.Send(doneMsg{v: v, err: err})
	}()
	var zero T
	res, err := p.Run()
	if err != nil {
		return zero, err
	}
	r := res.(spin).res
	if r.err != nil {
		return zero, r.err
	}
	v, _ := r.v.(T)
	return v, nil
}
