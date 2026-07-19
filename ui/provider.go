package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	pending = iota
	resolved
	failed
)

const (
	stageProvider = iota
	stageVariant
)

type labelMsg struct {
	i     int
	label string
	ok    bool
	subs  bool
}

type picker struct {
	title   string
	codes   []string
	labels  []string
	state   []int
	subs    []bool
	cursor  int
	choice  int
	stage   int
	vcursor int // 0 soft, 1 hard
	variant string
	ch      chan tea.Msg
}

func (m picker) Init() tea.Cmd { return listen(m.ch) }

func (m picker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case labelMsg:
		m.labels[msg.i] = msg.label
		m.subs[msg.i] = msg.subs
		if msg.ok {
			m.state[msg.i] = resolved
		} else {
			m.state[msg.i] = failed
		}
		return m, listen(m.ch)
	case tea.KeyMsg:
		if m.stage == stageVariant {
			return m.variantKey(msg)
		}
		return m.providerKey(msg)
	}
	return m, nil
}

func (m picker) providerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.codes)-1 {
			m.cursor++
		}
	case "enter":
		// only a provider that resolved with a subtitle file needs the choice
		// everything else pins soft, which attaches nothing when there is nothing
		if m.state[m.cursor] == resolved && m.subs[m.cursor] {
			m.stage = stageVariant
			m.vcursor = 0
			return m, nil
		}
		m.choice, m.variant = m.cursor, "soft"
		return m, tea.Quit
	case "esc", "ctrl+c", "q":
		m.choice = -1
		return m, tea.Quit
	}
	return m, nil
}

func (m picker) variantKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		m.vcursor = 0
	case "down", "j":
		m.vcursor = 1
	case "enter":
		m.choice = m.cursor
		m.variant = "soft"
		if m.vcursor == 1 {
			m.variant = "hard"
		}
		return m, tea.Quit
	case "esc":
		m.stage = stageProvider
	case "ctrl+c", "q":
		m.choice = -1
		return m, tea.Quit
	}
	return m, nil
}

func (m picker) View() string {
	if m.stage == stageVariant {
		return m.variantView()
	}

	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(nord8)).Bold(true).Render(m.title))
	b.WriteString("\n\n")

	cursorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(nord13))
	codeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(nord4))
	for i, code := range m.codes {
		point := "  "
		if i == m.cursor {
			point = cursorStyle.Render("> ")
		}
		b.WriteString(point)
		b.WriteString(codeStyle.Render(fmt.Sprintf("%-14s", code)))
		b.WriteString("  ")
		b.WriteString(detail(m.state[i], m.labels[i]))
		b.WriteByte('\n')
	}
	return b.String()
}

func (m picker) variantView() string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(nord8)).Bold(true).
		Render("Subtitles for " + m.codes[m.cursor]))
	b.WriteString("\n\n")

	rows := [...]string{"soft, attach subtitle file", "hard, subtitles already in the picture"}
	cursorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(nord13))
	rowStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(nord4))
	for i, r := range rows {
		point := "  "
		if i == m.vcursor {
			point = cursorStyle.Render("> ")
		}
		b.WriteString(point)
		b.WriteString(rowStyle.Render(r))
		b.WriteByte('\n')
	}
	return b.String()
}

func detail(state int, label string) string {
	switch state {
	case resolved:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(nord14)).Render(label)
	case failed:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(nord11)).Render(label)
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(nord3)).Render("resolving...")
	}
}

// PickProvider lists provider codes immediately and fills each row's label as
// probe returns, then returns the selected index and subtitle variant
// probe runs once per provider in its own goroutine and reports a label, whether
// the provider is usable, and whether it ships a subtitle file
// the soft or hard choice lives in this one program so the select keypress
// cannot leak into a second prompt and auto-answer it
func PickProvider(title string, codes []string, probe func(i int) (label string, usable, subs bool)) (int, string, error) {
	m := picker{
		title:  title,
		codes:  codes,
		labels: make([]string, len(codes)),
		state:  make([]int, len(codes)),
		subs:   make([]bool, len(codes)),
		choice: -1,
		ch:     make(chan tea.Msg, 2*len(codes)+1),
	}
	for i := range codes {
		go func(i int) {
			label, ok, subs := probe(i)
			m.ch <- labelMsg{i, label, ok, subs}
		}(i)
	}

	final, err := tea.NewProgram(m).Run()
	if err != nil {
		return 0, "", err
	}
	fm, ok := final.(picker)
	if !ok || fm.choice < 0 {
		return 0, "", ErrAborted
	}
	return fm.choice, fm.variant, nil
}
