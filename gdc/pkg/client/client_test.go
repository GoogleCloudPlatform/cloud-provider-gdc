// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

type countingTokenSource struct {
	calls  atomic.Int32
	ttl    time.Duration
	nowRef *time.Time
	err    error
}

func (c *countingTokenSource) Token() (*oauth2.Token, error) {
	n := c.calls.Add(1)
	if c.err != nil {
		return nil, c.err
	}
	now := time.Now()
	if c.nowRef != nil {
		now = *c.nowRef
	}
	return &oauth2.Token{
		AccessToken: "tok-" + string(rune('0'+n)),
		TokenType:   "Bearer",
		Expiry:      now.Add(c.ttl),
	}, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type trackingRoundTripper struct {
	fn         roundTripFunc
	idleClosed atomic.Int32
}

func (t *trackingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.fn(r)
}

func (t *trackingRoundTripper) CloseIdleConnections() {
	t.idleClosed.Add(1)
}

func TestResettableTokenSourceLeewayAndReset(t *testing.T) {
	fakeNow := time.Date(2026, 11, 26, 22, 0, 0, 0, time.UTC)
	base := &countingTokenSource{ttl: 10 * time.Minute, nowRef: &fakeNow}
	ts := newResettableTokenSource(base, time.Minute)
	ts.nowFn = func() time.Time { return fakeNow }

	tok1, err := ts.Token()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if base.calls.Load() != 1 {
		t.Fatalf("expected 1 base Token() call, got %d", base.calls.Load())
	}

	// Still valid before the 1-minute leeway window.
	fakeNow = fakeNow.Add(8 * time.Minute)
	tok2, err := ts.Token()
	if err != nil || tok2.AccessToken != tok1.AccessToken {
		t.Fatalf("expected cached token %q before leeway, got %q (err=%v)", tok1.AccessToken, tok2.AccessToken, err)
	}

	// Entering the 1-minute leeway window triggers proactive refresh.
	fakeNow = fakeNow.Add(90 * time.Second)
	tok3, err := ts.Token()
	if err != nil || tok3.AccessToken == tok1.AccessToken {
		t.Fatalf("expected refreshed token inside leeway window, got %q (err=%v)", tok3.AccessToken, err)
	}

	// Explicit reset invalidates tokens minted at or before attemptStart.
	ts.ResetTokenOlderThan(fakeNow)
	tok4, err := ts.Token()
	if err != nil || tok4.AccessToken == tok3.AccessToken {
		t.Fatalf("expected new token after ResetTokenOlderThan, got %q (err=%v)", tok4.AccessToken, err)
	}
}

func TestRetryingAuthTransportRetriesUnauthorizedWithFreshTokenAndRewoundBody(t *testing.T) {
	baseTS := &countingTokenSource{ttl: time.Hour}
	rts := newResettableTokenSource(baseTS, time.Minute)

	var attempts atomic.Int32
	baseRT := &trackingRoundTripper{
		fn: func(req *http.Request) (*http.Response, error) {
			n := attempts.Add(1)
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request body on attempt %d: %v", n, err)
			}
			if string(body) != `{"kind":"Backend"}` {
				t.Fatalf("unexpected body on attempt %d: %q", n, string(body))
			}
			if n == 1 {
				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Body:       io.NopCloser(strings.NewReader("Unauthorized")),
				}, nil
			}
			if gotAuth := req.Header.Get("Authorization"); gotAuth == "Bearer tok-1" || gotAuth == "" {
				t.Fatalf("expected rotated token on retry attempt %d, got %q", n, gotAuth)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		},
	}

	rt := &retryingAuthTransport{
		base:           baseRT,
		tokenSource:    rts,
		maxAttempts:    4,
		initialBackoff: time.Millisecond,
		sleepFn:        func(context.Context, time.Duration) error { return nil },
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://mgmt.example.com/apis/networking/v1/backends", bytes.NewReader([]byte(`{"kind":"Backend"}`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK after retry, got %d", resp.StatusCode)
	}
	if attempts.Load() != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts.Load())
	}
	if baseTS.calls.Load() != 2 {
		t.Fatalf("expected token rotation (2 mints), got %d", baseTS.calls.Load())
	}
	if baseRT.idleClosed.Load() == 0 {
		t.Fatalf("expected idle connections to be closed on 401")
	}
}

func TestRetryingAuthTransportRetriesGatewayErrorsAndIdempotentNetworkDrops(t *testing.T) {
	baseTS := &countingTokenSource{ttl: time.Hour}
	rts := newResettableTokenSource(baseTS, time.Minute)

	var attempts atomic.Int32
	baseRT := &trackingRoundTripper{
		fn: func(req *http.Request) (*http.Response, error) {
			switch attempts.Add(1) {
			case 1:
				return nil, errors.New("connection reset by peer")
			case 2:
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Body:       io.NopCloser(strings.NewReader("upstream unavailable")),
				}, nil
			default:
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("ok")),
				}, nil
			}
		},
	}

	rt := &retryingAuthTransport{
		base:           baseRT,
		tokenSource:    rts,
		maxAttempts:    4,
		initialBackoff: time.Millisecond,
		sleepFn:        func(context.Context, time.Duration) error { return nil },
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://mgmt.example.com/api", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || attempts.Load() != 3 {
		t.Fatalf("expected 200 OK on attempt 3, got status=%d attempts=%d", resp.StatusCode, attempts.Load())
	}
}

func TestResettableTokenSourceFallsBackOnRefreshFailure(t *testing.T) {
	fakeNow := time.Date(2026, 11, 26, 22, 0, 0, 0, time.UTC)
	base := &countingTokenSource{ttl: 10 * time.Minute, nowRef: &fakeNow}
	ts := newResettableTokenSource(base, time.Minute)
	ts.nowFn = func() time.Time { return fakeNow }

	tok1, err := ts.Token()
	if err != nil {
		t.Fatalf("initial Token() failed: %v", err)
	}

	fakeNow = fakeNow.Add(9*time.Minute + 30*time.Second)
	base.err = errors.New("sts temporarily unavailable")
	tok2, err := ts.Token()
	if err != nil {
		t.Fatalf("expected fallback to unexpired cached token, got error: %v", err)
	}
	if tok2.AccessToken != tok1.AccessToken {
		t.Fatalf("expected cached token %q, got %q", tok1.AccessToken, tok2.AccessToken)
	}

	fakeNow = fakeNow.Add(31 * time.Second)
	if _, err := ts.Token(); err == nil {
		t.Fatal("expected error once cached token has expired")
	}
}

type orderTrackingBody struct {
	r       io.Reader
	onClose func()
	drained bool
	closed  bool
}

func (b *orderTrackingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF {
		b.drained = true
	}
	return n, err
}

func (b *orderTrackingBody) Close() error {
	b.closed = true
	if b.onClose != nil {
		b.onClose()
	}
	return nil
}

type eventTrackingRoundTripper struct {
	inner       http.RoundTripper
	onCloseIdle func()
}

func (e *eventTrackingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return e.inner.RoundTrip(r)
}

func (e *eventTrackingRoundTripper) CloseIdleConnections() {
	if e.onCloseIdle != nil {
		e.onCloseIdle()
	}
}

func TestRetryingAuthTransportPersistent401RefreshesAtMostOnceAndDrainsBeforeEvict(t *testing.T) {
	baseTS := &countingTokenSource{ttl: time.Hour}
	rts := newResettableTokenSource(baseTS, time.Minute)
	if _, err := rts.Token(); err != nil {
		t.Fatalf("warm cache: %v", err)
	}

	var attempts atomic.Int32
	var events []string
	var bodies []*orderTrackingBody

	baseRT := &trackingRoundTripper{}
	baseRT.fn = func(req *http.Request) (*http.Response, error) {
		n := attempts.Add(1)
		body := &orderTrackingBody{
			r: strings.NewReader("unauthorized"),
			onClose: func() {
				events = append(events, "close-"+string(rune('0'+n)))
			},
		}
		bodies = append(bodies, body)
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     make(http.Header),
			Body:       body,
		}, nil
	}

	wrappedRT := &eventTrackingRoundTripper{
		inner: baseRT,
		onCloseIdle: func() {
			events = append(events, "evict")
		},
	}

	rt := &retryingAuthTransport{
		base:           wrappedRT,
		tokenSource:    rts,
		maxAttempts:    4,
		initialBackoff: time.Millisecond,
		sleepFn:        func(context.Context, time.Duration) error { return nil },
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://mgmt.example.com/api", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	if attempts.Load() != 4 {
		t.Fatalf("expected 4 attempts, got %d", attempts.Load())
	}
	if got := baseTS.calls.Load(); got != 2 {
		t.Fatalf("expected 2 total token mints (1 initial + 1 refresh), got %d", got)
	}
	for i := 0; i < 3; i++ {
		if !bodies[i].drained || !bodies[i].closed {
			t.Fatalf("attempt %d body not drained/closed before retry (drained=%v closed=%v)", i+1, bodies[i].drained, bodies[i].closed)
		}
	}
	if bodies[3].closed {
		t.Fatal("final returned response body must remain open for caller")
	}
	_ = resp.Body.Close()

	wantPrefix := []string{"close-1", "evict", "close-2", "evict", "close-3", "evict"}
	if len(events) < len(wantPrefix) {
		t.Fatalf("unexpected event sequence: %v", events)
	}
	for i, want := range wantPrefix {
		if events[i] != want {
			t.Fatalf("event[%d] = %q, want %q (full sequence: %v)", i, events[i], want, events)
		}
	}
}

func TestRetryingAuthTransportSkipsRetryWhenRetryAfterPresent(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		baseTS := &countingTokenSource{ttl: time.Hour}
		rts := newResettableTokenSource(baseTS, time.Minute)
		var attempts atomic.Int32
		rt := &retryingAuthTransport{
			base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				attempts.Add(1)
				return &http.Response{
					StatusCode: code,
					Header:     http.Header{"Retry-After": []string{"5"}},
					Body:       io.NopCloser(strings.NewReader("rate limited")),
				}, nil
			}),
			tokenSource:    rts,
			maxAttempts:    4,
			initialBackoff: time.Millisecond,
			sleepFn:        func(context.Context, time.Duration) error { return nil },
		}

		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://mgmt.example.com/api", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip(%d) failed: %v", code, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != code || attempts.Load() != 1 || resp.Header.Get("Retry-After") != "5" {
			t.Fatalf("status=%d attempts=%d retry-after=%q; want single attempt passing Retry-After through", resp.StatusCode, attempts.Load(), resp.Header.Get("Retry-After"))
		}
	}
}
