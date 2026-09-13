// Package chttp provides the HTTP client every katbyte tool talks to APIs
// with: trace-level request/response logging through clog, per-attempt
// timeouts so a stalled server fails fast, and retries for transient failures
// that are careful never to re-send a mutation whose outcome is unknown.
package chttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/katbyte/go-kt/clog"
)

// DefaultMaxRetry is how many attempts NewHTTPClient makes before giving up:
// enough to ride through a blip or a single rate-limit window without turning
// an outage into a minutes-long hang.
const DefaultMaxRetry = 3

// MaxRetryAfter caps how long a Retry-After header can make a retry wait. A
// server asking for more than this is telling an interactive tool to come back
// later, not to sit there.
const MaxRetryAfter = 60 * time.Second

type ctxKey int

const retrySafeKey ctxKey = 0

// MarkRetrySafe declares a request safe to re-send even though its method is
// not idempotent: a GraphQL or JQL query is a read that happens to travel as a
// POST. Reads with idempotent methods (GET, HEAD, OPTIONS) need no mark.
func MarkRetrySafe(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), retrySafeKey, true))
}

// retrySafe reports whether a request may be re-sent when its outcome is
// unknown (a transport error or a 5xx): true for idempotent methods and marked
// reads. A mutation that gets no response may still have been applied
// server-side, so re-sending it risks doing the work twice.
func retrySafe(req *http.Request) bool {
	if v, ok := req.Context().Value(retrySafeKey).(bool); ok && v {
		return true
	}
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// NewBaseTransport returns http.DefaultTransport tuned with per-attempt
// timeouts so a stalled connection or unresponsive server fails fast and gets
// retried by RetryTransport instead of hanging the command. It is exported so
// callers that must build their own client (oauth2, for one) can still start
// from the same transport.
func NewBaseTransport() http.RoundTripper {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport // unreachable, but degrade gracefully
	}

	c := t.Clone()
	c.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	c.TLSHandshakeTimeout = 10 * time.Second
	c.ResponseHeaderTimeout = 30 * time.Second
	return c
}

// NewHTTPClient returns a client that trace-logs every exchange under name and
// retries transient failures up to DefaultMaxRetry times. name appears in the
// logs so a tool talking to two APIs can tell their traffic apart.
func NewHTTPClient(name string) *http.Client {
	return &http.Client{
		Transport: NewRetryTransport(name, NewTransport(name, NewBaseTransport()), DefaultMaxRetry),
	}
}

// Transport is an http.RoundTripper that dumps each request and response to
// clog.Log at TRACE, with JSON bodies pretty-printed. The dumps include
// headers, so they are only produced when TRACE is actually enabled.
type Transport struct {
	name      string
	transport http.RoundTripper
}

// NewTransport wraps next with trace logging under name.
func NewTransport(name string, next http.RoundTripper) *Transport {
	return &Transport{name: name, transport: next}
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if clog.Log.IsLevelEnabled(logrus.TraceLevel) {
		reqData, err := httputil.DumpRequestOut(req, true)
		if err == nil {
			clog.Log.Tracef(logReqMsg, t.name, prettyPrintJSON(reqData))
		} else {
			clog.Log.Debugf("%s API Request error: %#v", t.name, err)
		}
	}

	resp, err := t.transport.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	if clog.Log.IsLevelEnabled(logrus.TraceLevel) {
		respData, err := httputil.DumpResponse(resp, true)
		if err == nil {
			clog.Log.Tracef(logRespMsg, t.name, prettyPrintJSON(respData))
		} else {
			clog.Log.Debugf("%s API Response error: %#v", t.name, err)
		}
	}

	return resp, nil
}

// RetryTransport wraps an http.RoundTripper with retry logic for transient
// failures: 429 (rate limited) for every request, plus connection errors and
// 5xx (server error) responses for retry-safe requests only (see
// MarkRetrySafe). Attempts back off exponentially (1s, 2s, 4s, ...), a 429's
// Retry-After header is honoured up to MaxRetryAfter, and a cancelled request
// context aborts the wait.
type RetryTransport struct {
	name      string
	transport http.RoundTripper
	maxRetry  int
}

// NewRetryTransport wraps next with up to maxRetry attempts under name. A
// maxRetry below 1 is treated as 1: every request is sent at least once.
func NewRetryTransport(name string, next http.RoundTripper, maxRetry int) *RetryTransport {
	return &RetryTransport{name: name, transport: next, maxRetry: max(1, maxRetry)}
}

// RoundTrip implements http.RoundTripper.
func (t *RetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var resp *http.Response
	var err error

	// a request body is consumed by each attempt, so it must be rewound via GetBody
	// before a retry; requests with a body but no GetBody cannot be retried safely.
	// http.NoBody counts as bodyless — NewRequest leaves GetBody nil for it
	rewind := func() bool {
		if req.Body == nil || req.Body == http.NoBody {
			return true
		}
		if req.GetBody == nil {
			return false
		}
		body, gbErr := req.GetBody()
		if gbErr != nil {
			return false
		}
		req.Body = body
		return true
	}

	safe := retrySafe(req)
	for attempt := range t.maxRetry {
		resp, err = t.transport.RoundTrip(req)
		if err != nil {
			// a transport error can land after the server committed the write
			// (the response just never made it back), so only retry-safe
			// requests go again — re-posting a comment would duplicate it
			if attempt < t.maxRetry-1 && safe && rewind() {
				wait := backoff(attempt)
				clog.Log.Debugf("%s request failed (attempt %d/%d), retrying in %s: %v", t.name, attempt+1, t.maxRetry, wait, err)
				if !sleep(req.Context(), wait) {
					return nil, errors.Join(req.Context().Err(), err)
				}
				continue
			}
			return nil, err
		}

		// 429 (rate limited) was rejected before it was acted on, so every
		// request may retry it; a 5xx leaves a mutation's fate unknown, so
		// only retry-safe requests ride through those
		if resp.StatusCode == http.StatusTooManyRequests || (safe && resp.StatusCode >= 500) {
			if attempt < t.maxRetry-1 && rewind() {
				wait := backoff(attempt)
				if resp.StatusCode == http.StatusTooManyRequests {
					if ra, ok := retryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
						wait = ra
					}
				}
				clog.Log.Debugf("%s got status %d (attempt %d/%d), retrying in %s", t.name, resp.StatusCode, attempt+1, t.maxRetry, wait)
				_ = resp.Body.Close()
				if !sleep(req.Context(), wait) {
					return nil, req.Context().Err()
				}
				continue
			}
		}

		return resp, nil
	}

	// unreachable: maxRetry is at least 1, so the loop always returns
	return resp, err
}

// backoff is the exponential wait before the attempt after attempt: 1s, 2s, 4s...
func backoff(attempt int) time.Duration {
	return time.Duration(1<<attempt) * time.Second
}

// sleep waits for d unless ctx is done first, reporting whether the full wait
// completed. A cancelled request should not sit out a backoff it will never use.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// retryAfter parses a Retry-After header value, either delay-seconds or an
// HTTP-date, into a wait relative to now. It reports false for an absent or
// unparsable value, and clamps the result to [0, MaxRetryAfter] so a server
// cannot park the tool indefinitely.
func retryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}

	var wait time.Duration
	if secs, err := strconv.Atoi(value); err == nil {
		wait = time.Duration(secs) * time.Second
	} else if at, err := http.ParseTime(value); err == nil {
		wait = at.Sub(now)
	} else {
		return 0, false
	}

	return max(0, min(wait, MaxRetryAfter)), true
}

// prettyPrintJSON iterates through a []byte line-by-line,
// transforming any lines that are complete json into pretty-printed json.
func prettyPrintJSON(b []byte) string {
	parts := strings.Split(string(b), "\n")
	for i, p := range parts {
		if b := []byte(p); json.Valid(b) {
			var out bytes.Buffer
			//nolint:errcheck,gosec // json.Indent only fails on invalid input, which json.Valid just ruled out
			json.Indent(&out, b, "", " ")
			parts[i] = out.String()
		}
	}

	return strings.Join(parts, "\n")
}

const logReqMsg = `%s API Request Details:
---[ REQUEST ]---------------------------------------
%s
-----------------------------------------------------`

const logRespMsg = `%s API Response Details:
---[ RESPONSE ]--------------------------------------
%s
-----------------------------------------------------`
