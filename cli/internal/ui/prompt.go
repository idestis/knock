package ui

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Knocker is who is at the door, as the relay told us.
type Knocker struct {
	Name     string
	Detail   string // verified email, or @login for GitHub without one
	Provider string // google, github, anonymous
	Verified bool
	Device   string
}

// Answer is the host's decision.
type Answer int

const (
	AnswerNo Answer = iota
	AnswerYes
	AnswerTimeout
	AnswerGone // the knock was withdrawn while we asked
)

type tickMsg time.Time

type letIn struct {
	k        Knocker
	deadline time.Time
	now      time.Time
	answer   Answer
	done     bool
}

func (m letIn) Init() tea.Cmd { return tick() }

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m letIn) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "y", "Y":
			m.answer, m.done = AnswerYes, true
			return m, tea.Quit
		case "n", "N", "esc", "enter", "ctrl+c", "q":
			// No default-yes: Return means "Not now".
			m.answer, m.done = AnswerNo, true
			return m, tea.Quit
		}
	case tickMsg:
		m.now = time.Time(msg)
		if !m.now.Before(m.deadline) {
			m.answer, m.done = AnswerTimeout, true
			return m, tea.Quit
		}
		return m, tick()
	}
	return m, nil
}

func (m letIn) View() tea.View {
	name := Bold.Render(m.k.Name)
	if m.done {
		switch m.answer {
		case AnswerYes:
			return tea.NewView(Good.Render("✓ ") + "Letting " + name + " in.\n")
		case AnswerTimeout:
			return tea.NewView(Dim.Render("No answer in time, so " + m.k.Name + " wasn't let in.\n"))
		default:
			return tea.NewView(Dim.Render("Not now. " + m.k.Name + " wasn't let in.\n"))
		}
	}
	left := int(m.deadline.Sub(m.now).Round(time.Second) / time.Second)
	var who string
	if m.k.Verified {
		who = Good.Render("✓ verified") + Dim.Render(" with "+providerName(m.k.Provider))
		if m.k.Detail != "" {
			who += Dim.Render(" · " + m.k.Detail)
		}
	} else {
		who = Warn.Render("⚠ not verified") + Dim.Render(": they didn't sign in, so the name is whatever they typed")
	}
	body := fmt.Sprintf("%s\n%s wants to come in from %s.\n%s\n\nLet them in? %s %s",
		Brand.Render("Knock, knock."), name, Bold.Render(m.k.Device), who,
		Bold.Render("[y/N]"), Dim.Render(fmt.Sprintf("%ds", max(left, 0))))
	return tea.NewView(lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(Accent).Padding(0, 2).Render(body) + "\n")
}

func providerName(p string) string {
	switch p {
	case "google":
		return "Google"
	case "github":
		return "GitHub"
	}
	return p
}

// AskLetIn shows the prompt until the host answers, the timeout passes, or
// ctx ends (AnswerGone).
func AskLetIn(ctx context.Context, k Knocker, timeout time.Duration) Answer {
	now := time.Now()
	m := letIn{k: k, deadline: now.Add(timeout), now: now}
	res, err := tea.NewProgram(m, tea.WithContext(ctx), tea.WithOutput(tty)).Run()
	if ctx.Err() != nil {
		return AnswerGone
	}
	if err != nil {
		return AnswerNo
	}
	return res.(letIn).answer
}
