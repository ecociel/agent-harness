// Package sse reads server-sent events.
package sse

import (
	"bufio"
	"io"
	"strings"
)

// Event is one SSE event.
type Event struct {
	Name string
	Data string
}

// Read calls fn for every event in r until EOF, an error, or fn returns
// false.
func Read(r io.Reader, fn func(Event) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var ev Event
	var data []string
	flush := func() bool {
		if len(data) == 0 && ev.Name == "" {
			return true
		}
		ev.Data = strings.Join(data, "\n")
		ok := fn(ev)
		ev, data = Event{}, nil
		return ok
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if !flush() {
				return nil
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			ev.Name = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(line[5:], " "))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	flush()
	return nil
}
