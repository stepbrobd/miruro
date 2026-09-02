package ui

import (
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/log"
)

// a live view owns the terminal while it runs, so a log record written straight
// to stderr lands in the middle of a redraw
// captureLog routes the log into the view instead, and the view shows the last
// keptLines of it underneath itself

type logMsg string

// keptLines bounds how many log lines stay under a live view
// a walk down every stream of every provider writes more than belongs under a
// prompt, and eight covers the whole of one without pushing it off a short
// terminal
const keptLines = 8

// sink turns each log record into a message for the running view
// the logger writes one whole record per call and holds its own mutex while it
// does, so this must not block and drops rather than waits
type sink chan string

func (s sink) Write(b []byte) (int, error) {
	for line := range strings.SplitSeq(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		select {
		case s <- line:
		default:
		}
	}
	return len(b), nil
}

// captureLog points the log at a fresh sink and returns it with its undo
// nothing else sets the output, so stderr is where it came from
// the undo closes the sink, which releases the listener the view left parked
// on it, and it is safe to close because the logger holds its own mutex
// across the output swap and every write, so no write is in flight once the
// swap has returned
func captureLog() (sink, func()) {
	lines := make(sink, 64)
	log.SetOutput(lines)
	return lines, func() {
		log.SetOutput(os.Stderr)
		close(lines)
	}
}

func listenLog(ch <-chan string) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		line, ok := <-ch
		if !ok {
			return nil
		}
		return logMsg(line)
	}
}

// keep adds a line to the window, dropping the oldest past keptLines
func keep(seen []string, line string) []string {
	seen = append(seen, line)
	if len(seen) > keptLines {
		seen = seen[len(seen)-keptLines:]
	}
	return seen
}

func writeLines(b *strings.Builder, seen []string) {
	for _, line := range seen {
		b.WriteString(line)
		b.WriteByte('\n')
	}
}
