package ui

import (
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
)

// Choice is one option in a picker.
type Choice struct {
	Key, Label, Hint string
}

func (c Choice) Title() string       { return c.Label }
func (c Choice) Description() string { return c.Hint }
func (c Choice) FilterValue() string { return c.Label }

type picker struct {
	l      list.Model
	chosen *Choice
	quit   bool
}

func (m picker) Init() tea.Cmd { return nil }

func (m picker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "enter":
			if c, ok := m.l.SelectedItem().(Choice); ok {
				m.chosen = &c
			}
			return m, tea.Quit
		case "esc", "ctrl+c", "q":
			m.quit = true
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.l.SetWidth(msg.Width)
	}
	var cmd tea.Cmd
	m.l, cmd = m.l.Update(msg)
	return m, cmd
}

func (m picker) View() tea.View {
	if m.chosen != nil || m.quit {
		return tea.NewView("")
	}
	return tea.NewView(m.l.View())
}

// Pick shows a list and returns the chosen key, or ErrCancelled.
func Pick(title string, choices []Choice) (string, error) {
	items := make([]list.Item, len(choices))
	for i, c := range choices {
		items[i] = c
	}
	d := list.NewDefaultDelegate()
	d.ShowDescription = true
	d.Styles.SelectedTitle = d.Styles.SelectedTitle.Foreground(Accent).BorderForeground(Accent)
	d.Styles.SelectedDesc = d.Styles.SelectedDesc.BorderForeground(Accent)
	l := list.New(items, d, 50, len(choices)*3+5)
	l.Title = title
	l.Styles.Title = Brand.Padding(0, 1)
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false)
	l.SetShowPagination(false)
	res, err := tea.NewProgram(picker{l: l}, tea.WithOutput(tty)).Run()
	if err != nil {
		return "", err
	}
	p := res.(picker)
	if p.chosen == nil {
		return "", ErrCancelled
	}
	return p.chosen.Key, nil
}
