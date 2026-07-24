package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ErrCancelled marks a download the user interrupted before it finished, so
// the caller counts it as a failure and never as a silent success
var ErrCancelled = errors.New("cancelled")

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
	// width sizes the label column
	// term is the full terminal width
	width  int
	term   int
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
	case tea.WindowSizeMsg:
		// a resize arrives outside the ch stream, so it must not re-arm listen
		// and start a second reader
		m.term = msg.Width
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
			b.WriteString(errLine(ok(false), name, m.errs[i].Error(), m.term))
			b.WriteByte('\n')
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
// bar per line
// each task receives a context that is cancelled when the user quits and a
// reporter for bytes done and total
// a task the user interrupts before it finishes is returned as ErrCancelled
// by the time Downloads returns no goroutine it spawned is still running and
// every child process a task started has been reaped
func Downloads(ctx context.Context, labels []string, workers int, task func(ctx context.Context, i int, report func(done, total int64)) error) []error {
	n := len(labels)
	width := 0
	for _, l := range labels {
		if len(l) > width {
			width = len(l)
		}
	}

	dctx, cancel := context.WithCancel(ctx)
	defer cancel()

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

	results := make([]error, n)
	done := make(chan struct{})
	go func() {
		schedule(dctx, labels, workers, task, m.ch, results)
		// schedule returns only after its wg.Wait, so this close is the
		// happens-before edge publishing every results write to the loop below
		close(done)
	}()

	_, err := tea.NewProgram(m, tea.WithContext(dctx)).Run()

	if err == nil || errors.Is(err, tea.ErrInterrupted) || ctx.Err() != nil {
		// the UI exited on a finish, a quit key, or an interrupt, so stop the
		// workers
		// any other error means no interactive terminal, and the workers keep
		// the still-live context and run to completion
		cancel()
	}
	// consuming ch keeps senders unblocked until every worker has been joined
	// the model's errs and fin only ever fed the render, results is the truth
	for {
		select {
		case <-m.ch:
		case <-done:
			// the workers are joined, so closing ch releases a listen command
			// tea may still have parked on it
			close(m.ch)
			return results
		}
	}
}

// schedule runs every task under the worker limit
// worker i writes results[i] exactly once before its wg.Done, so the slice is
// fully populated when schedule returns
func schedule(ctx context.Context, labels []string, workers int, task func(context.Context, int, func(int64, int64)) error, ch chan tea.Msg, results []error) {
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

			var mu sync.Mutex
			var last time.Time
			report := func(done, total int64) {
				// a task may report from several goroutines at once, so the
				// throttle state needs its own lock
				mu.Lock()
				now := time.Now()
				fresh := now.Sub(last) >= 80*time.Millisecond
				if fresh {
					last = now
				}
				mu.Unlock()
				if !fresh {
					return
				}
				// a dropped update only costs a repaint, so never block on a
				// reader that already left
				select {
				case ch <- progressMsg{i, done, total}:
				case <-ctx.Done():
				}
			}
			err := task(ctx, i, report)
			// a real failure that raced the cancellation is deliberately folded
			// into ErrCancelled, matching the old fin-based semantics where an
			// interrupted task always read as cancelled
			if err != nil && ctx.Err() != nil {
				err = ErrCancelled
			}
			results[i] = err
			// a dropped doneMsg is fine, results carries the truth
			select {
			case ch <- doneMsg{i, err}:
			case <-ctx.Done():
			}
		}(i)
	}
	wg.Wait()
}

// defaultTerm is assumed until the first tea.WindowSizeMsg reports the real width
const defaultTerm = 80

const ellipsis = "..."

// flatten keeps a multi-line error on one row
// ffmpeg reports its failures across several lines, which would otherwise push
// the still-live rows below out of place
var flatten = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

// errLine renders one failed task as a single line that fits the terminal
// marker and label may carry ANSI styling, so only the plain message is cut and
// the head is composed afterwards
func errLine(marker, label, msg string, term int) string {
	if term <= 0 {
		term = defaultTerm
	}
	head := fmt.Sprintf("  %s %s  ", marker, label)
	msg = flatten.Replace(msg)

	room := term - lipgloss.Width(head)
	if lipgloss.Width(msg) <= room {
		return head + msg
	}
	// the label column alone already fills the terminal, so show what marker fits
	if room < len(ellipsis) {
		return head + ellipsis[:max(room, 0)]
	}
	return head + cut(msg, room-len(ellipsis)) + ellipsis
}

// cut returns the longest prefix of a plain string fitting w display columns
func cut(s string, w int) string {
	used := 0
	for i, r := range s {
		rw := lipgloss.Width(string(r))
		if used+rw > w {
			return s[:i]
		}
		used += rw
	}
	return s
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
