package httperr

import (
	"net/http"
	"testing"
	"time"

	"github.com/ecociel/agent-harness/model"
)

func TestClass(t *testing.T) {
	for status, want := range map[int]model.ErrorClass{
		0: model.ClassTransient, 200: model.ClassUnknown, 400: model.ClassInvalid, 401: model.ClassAuth,
		402: model.ClassAuth, 403: model.ClassAuth, 404: model.ClassInvalid, 408: model.ClassTransient,
		409: model.ClassTransient, 413: model.ClassContextLength, 422: model.ClassInvalid, 425: model.ClassTransient,
		429: model.ClassRateLimit, 500: model.ClassTransient, 503: model.ClassTransient, 529: model.ClassTransient,
	} {
		if got := Class(status); got != want {
			t.Errorf("%d: got %s, want %s", status, got, want)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name, header, value string
		want                time.Duration
	}{
		{"nothing", "", "", 0},
		{"seconds", "Retry-After", "7", 7 * time.Second},
		{"a fraction", "Retry-After", "1.5", 1500 * time.Millisecond},
		{"milliseconds", "Retry-After-Ms", "250", 250 * time.Millisecond},
		{"a date", "Retry-After", now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{"a date gone by", "Retry-After", now.Add(-time.Hour).Format(http.TimeFormat), 0},
		{"negative", "Retry-After", "-3", 0},
		{"junk", "Retry-After", "soon", 0},
	} {
		h := http.Header{}
		if c.header != "" {
			h.Set(c.header, c.value)
		}
		if got := RetryAfter(h, now); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
