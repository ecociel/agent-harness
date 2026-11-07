// Package httperr holds what the provider adapters share about a failed call.
package httperr

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ecociel/agent-harness/model"
)

// Class is what a status says by itself. An adapter that can read the provider's own
// error type looks at that first. Status 0 is a failure inside a stream that had
// already answered 200: the server gave up partway, so another attempt can work.
func Class(status int) model.ErrorClass {
	switch {
	case status == 0, status == 408, status == 409, status == 425, status >= 500:
		return model.ClassTransient
	case status == 429:
		return model.ClassRateLimit
	case status == 401, status == 402, status == 403:
		return model.ClassAuth
	case status == 413:
		return model.ClassContextLength
	case status >= 400:
		return model.ClassInvalid
	}
	return model.ClassUnknown
}

// Refused reports a status that turns down the request as sent: malformed, too
// large, of a media type the server does not take, or unprocessable. When the
// request carried a binary part, such a refusal is the part's, unless the provider
// says the context is full.
func Refused(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// RetryAfter reads how long the server asked for: milliseconds, seconds or a date.
// Anything unreadable or already past is zero.
func RetryAfter(h http.Header, now time.Time) time.Duration {
	if ms, err := strconv.ParseFloat(strings.TrimSpace(h.Get("Retry-After-Ms")), 64); err == nil && ms > 0 {
		return time.Duration(ms * float64(time.Millisecond))
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if s, err := strconv.ParseFloat(v, 64); err == nil {
		return max(0, time.Duration(s*float64(time.Second)))
	}
	if at, err := http.ParseTime(v); err == nil {
		return max(0, at.Sub(now))
	}
	return 0
}
