package tui

import (
	"bufio"
	"os"
)

// startStdinPump is the single stdin owner. All line input (prompts and
// approvals) is delivered on the returned channel; nothing else may read os.Stdin.
func startStdinPump() <-chan string {
	ch := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			ch <- sc.Text()
		}
		close(ch)
	}()
	return ch
}
