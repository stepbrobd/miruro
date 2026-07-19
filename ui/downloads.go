package ui

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type progressMsg struct {
	i           int
	done, total int64
}

type doneMsg struct {
	i   int
	err error
}

type downloads struct {
	labels []string
	width  int
	bars   []progress.Model
	done   []int64
	total  []int64
	errs   []error
	fin    []bool
	remain int
	ch     chan tea.Msg
}

func (m downloads) Init() tea.Cmd { return listen(m.ch) }

func listen(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

func (m downloads) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case progressMsg:
		m.done[msg.i], m.total[msg.i] = msg.done, msg.total
		return m, listen(m.ch)
	case doneMsg:
		if !m.fin[msg.i] {
			m.fin[msg.i] = true
			m.errs[msg.i] = msg.err
			m.remain--
		}
		if m.remain == 0 {
			return m, tea.Quit
		}
		return m, listen(m.ch)
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m downloads) View() string {
	var b strings.Builder
	for i, label := range m.labels {
		name := fmt.Sprintf("%-*s", m.width, label)
		switch {
		case m.fin[i] && m.errs[i] != nil:
			b.WriteString(fmt.Sprintf("  %s %s  %s\n", ok(false), name, m.errs[i]))
		case m.fin[i]:
			b.WriteString(fmt.Sprintf("  %s %s  done\n", ok(true), name))
		case m.total[i] > 0:
			b.WriteString(fmt.Sprintf("  %s %s  %s\n", name, m.bars[i].ViewAs(float64(m.done[i])/float64(m.total[i])), size(m.done[i], m.total[i])))
		default:
			b.WriteString(fmt.Sprintf("  %s %s\n", name, human(m.done[i])))
		}
	}
	return b.String()
}

// Downloads runs labelled tasks with a worker limit, rendering one live progress
// bar per line. task receives a reporter for bytes done and total.
func Downloads(labels []string, workers int, task func(i int, report func(done, total int64)) error) []error {
	n := len(labels)
	width := 0
	for _, l := range labels {
		if len(l) > width {
			width = len(l)
		}
	}

	m := downloads{
		labels: labels,
		width:  width,
		bars:   make([]progress.Model, n),
		done:   make([]int64, n),
		total:  make([]int64, n),
		errs:   make([]error, n),
		fin:    make([]bool, n),
		remain: n,
		ch:     make(chan tea.Msg, 256),
	}
	for i := range m.bars {
		m.bars[i] = progress.New(progress.WithWidth(30), progress.WithoutPercentage())
	}

	go schedule(labels, workers, task, m.ch)

	final, err := tea.NewProgram(m).Run()
	if err != nil {
		return drain(m.ch, n)
	}
	if fm, ok := final.(downloads); ok {
		return fm.errs
	}
	return make([]error, n)
}

func schedule(labels []string, workers int, task func(int, func(int64, int64)) error, ch chan tea.Msg) {
	if workers < 1 {
		workers = 1
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := range labels {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()

			var last time.Time
			report := func(done, total int64) {
				now := time.Now()
				if now.Sub(last) < 80*time.Millisecond {
					return
				}
				last = now
				ch <- progressMsg{i, done, total}
			}
			ch <- doneMsg{i, task(i, report)}
		}(i)
	}
	wg.Wait()
}

// drain collects results when no interactive terminal is available.
func drain(ch chan tea.Msg, n int) []error {
	errs := make([]error, n)
	for remain := n; remain > 0; {
		if d, ok := (<-ch).(doneMsg); ok {
			errs[d.i] = d.err
			remain--
		}
	}
	return errs
}

func ok(good bool) string {
	if good {
		return lipgloss.NewStyle().Foreground(lipgloss.Color(nord14)).Render("+")
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(nord11)).Render("x")
}

func size(done, total int64) string { return human(done) + " / " + human(total) }

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
