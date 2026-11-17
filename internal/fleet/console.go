package fleet

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// The runner's console output (what run.sh prints) is mirrored into its
// Runner_*.log as "[… Terminal] WRITE LINE: …" / "WRITE ERROR: …" lines, so
// cothon can show it however the runner was started.

// ConsoleLine is one line the runner printed.
type ConsoleLine struct {
	Text  string
	Error bool
}

// ReadConsole returns the console lines in path from byte offset on, and the
// offset after the last complete line.
func ReadConsole(path string, offset int64) ([]ConsoleLine, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() < offset {
		offset = 0 // replaced
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	var out []ConsoleLine
	br := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return out, offset, nil
			}
			return out, offset, err
		}
		offset += int64(len(line))
		_, component, msg, ok := parseLogLine(strings.TrimRight(line, "\r\n"))
		if !ok || component != "Terminal" {
			continue
		}
		switch {
		case strings.HasPrefix(msg, "WRITE LINE: "):
			out = append(out, ConsoleLine{Text: strings.TrimPrefix(msg, "WRITE LINE: ")})
		case strings.HasPrefix(msg, "WRITE LINE:"):
			out = append(out, ConsoleLine{Text: ""})
		case strings.HasPrefix(msg, "WRITE ERROR: "):
			out = append(out, ConsoleLine{Text: strings.TrimPrefix(msg, "WRITE ERROR: "), Error: true})
		}
	}
}
