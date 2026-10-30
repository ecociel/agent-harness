package journaltest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/ecociel/agent-harness"
)

// store is a correct journal built on JSON. The bugs below each break one promise.
type store struct {
	mu   sync.Mutex
	runs map[string]map[int64][]byte

	noIndex     bool // a later write replaces the first
	partial     bool // the events before a conflict stay
	wrongError  bool // a conflict is a plain error
	shared      bool // runs share one log
	reversed    bool // Load returns newest first
	unsorted    bool // Load returns map order
	dropsNul    bool // refuses NUL, like a Postgres text column
	dropsNanos  bool // keeps timestamps to the microsecond
	dropsWake   bool // a projection that loses a field
	clipsLarge  bool // a column with a size limit
	checkThenGo bool // reads then writes, with no index: the classic race
}

func newStore() *store { return &store{runs: map[string]map[int64][]byte{}} }

func (s *store) key(id string) string {
	if s.shared {
		return "all"
	}
	return id
}

func (s *store) Append(_ context.Context, id string, evs []harness.Event) error {
	if s.checkThenGo {
		s.mu.Lock()
		taken := s.runs[s.key(id)][evs[0].Seq] != nil
		s.mu.Unlock()
		if taken {
			return harness.ErrConflict
		}
		runtime.Gosched()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	log := s.runs[s.key(id)]
	if log == nil {
		log = map[int64][]byte{}
		s.runs[s.key(id)] = log
	}
	if !s.noIndex && !s.checkThenGo && !s.partial {
		for _, e := range evs {
			if log[e.Seq] != nil {
				return s.conflict()
			}
		}
	}
	for _, e := range evs {
		if s.partial && log[e.Seq] != nil {
			continue
		}
		if s.dropsNul && strings.Contains(e.Text, "\x00") {
			return errors.New("invalid byte sequence")
		}
		if s.dropsWake {
			e.Wake = e.Time
		}
		if s.clipsLarge && len(e.Text) > 1<<16 {
			e.Text = e.Text[:1<<16]
		}
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if s.dropsNanos {
			b = []byte(strings.ReplaceAll(string(b), ".123456789", ".123456"))
		}
		log[e.Seq] = b
	}
	if s.partial && len(evs) > 1 {
		return s.conflict()
	}
	return nil
}

func (s *store) conflict() error {
	if s.wrongError {
		return errors.New("duplicate key")
	}
	return harness.ErrConflict
}

func (s *store) Load(_ context.Context, id string) ([]harness.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	log := s.runs[s.key(id)]
	seqs := make([]int64, 0, len(log))
	for seq := range log {
		seqs = append(seqs, seq)
	}
	if !s.unsorted {
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] != s.reversed })
	}
	out := make([]harness.Event, len(seqs))
	for i, seq := range seqs {
		if err := json.Unmarshal(log[seq], &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// failure stands in for testing.T so a check's failure can be observed.
type failure struct {
	testing.TB
	mu     sync.Mutex
	failed bool
	first  string
}

func (f *failure) Helper()                        {}
func (f *failure) Logf(string, ...any)            {}
func (f *failure) Log(...any)                     {}
func (f *failure) Failed() bool                   { return f.failed }
func (f *failure) Errorf(format string, a ...any) { f.note(fmt.Sprintf(format, a...)) }
func (f *failure) Error(a ...any)                 { f.note(fmt.Sprint(a...)) }
func (f *failure) Fatalf(format string, a ...any) {
	f.note(fmt.Sprintf(format, a...))
	runtime.Goexit()
}
func (f *failure) Fatal(a ...any) {
	f.note(fmt.Sprint(a...))
	runtime.Goexit()
}

func (f *failure) note(msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.failed {
		f.failed, f.first = true, msg
	}
}

func runAll(j harness.Journal) (failed []string) {
	for _, c := range checks {
		f := &failure{}
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.run(f, j)
		}()
		<-done
		if f.failed {
			failed = append(failed, c.name)
		}
	}
	return failed
}

func TestACorrectJournalPasses(t *testing.T) {
	Run(t, func(*testing.T) harness.Journal { return newStore() })
}

func TestTheInMemoryJournalPasses(t *testing.T) {
	Run(t, func(*testing.T) harness.Journal { return harness.NewMemJournal() })
}

// A conformance suite is only worth trusting if it fails. Each broken journal must
// be refused by the check named for the promise it breaks.
func TestTheSuiteRefusesBrokenJournals(t *testing.T) {
	cases := []struct {
		name  string
		bug   func(*store)
		check string
	}{
		{"no unique index", func(s *store) { s.noIndex = true }, "TakenPositionConflicts"},
		{"a partial append survives a conflict", func(s *store) { s.partial = true }, "AppendIsAllOrNothing"},
		{"a conflict is a plain error", func(s *store) { s.wrongError = true }, "TakenPositionConflicts"},
		{"runs share a log", func(s *store) { s.shared = true }, "RunsAreIndependent"},
		{"newest first", func(s *store) { s.reversed = true }, "EventsComeBackInOrder"},
		{"map order", func(s *store) { s.unsorted = true }, "EventsComeBackInOrder"},
		{"a NUL byte is refused", func(s *store) { s.dropsNul = true }, "AwkwardTextSurvives"},
		{"nanoseconds are lost", func(s *store) { s.dropsNanos = true }, "EveryFieldSurvives"},
		{"a field is projected wrongly", func(s *store) { s.dropsWake = true }, "EveryFieldSurvives"},
		{"large text is clipped", func(s *store) { s.clipsLarge = true }, "AwkwardTextSurvives"},
		{"check then write", func(s *store) { s.checkThenGo = true }, "ConcurrentWritersGetOneWinner"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStore()
			c.bug(s)
			failed := runAll(s)
			for _, name := range failed {
				if name == c.check {
					return
				}
			}
			t.Fatalf("%s did not refuse it, failed: %v", c.check, failed)
		})
	}
}
