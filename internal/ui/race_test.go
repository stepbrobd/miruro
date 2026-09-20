package ui

import (
	"sync"
	"testing"

	"github.com/charmbracelet/log"
)

// a live view is raised while workers are already logging, so capturing the log
// must not touch anything the logger reads outside the mutex it takes for the
// output
func TestCaptureLogUnderConcurrentWriters(t *testing.T) {
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					log.Warn("download failed, trying the next stream", "episode", 3, "provider", "pewe")
				}
			}
		})
	}
	for range 20 {
		lines, restore := captureLog()
		go func() {
			for range lines {
			}
		}()
		restore()
	}
	close(stop)
	wg.Wait()
}
