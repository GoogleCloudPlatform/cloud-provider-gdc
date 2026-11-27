// Copyright 2025 Google LLC
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

package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"net/http/httptest"

	clock "k8s.io/utils/clock/testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/oauth2"
)

const (
	jwtAccessToken   = "test-jwt-access-token"
	stsAccessToken   = "test-sts-access-token"
	expiresInSeconds = 1700434861
)

var testTimeNow = time.Date(2023, 1, 1, 1, 1, 1, 1, time.UTC)

func TestSTSToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, mockResponse())
	}))
	defer server.Close()

	stsTS := &stsTokenSource{
		tokenURI:       server.URL,
		audience:       "audience",
		jwtTokenSource: &mockJWTTokenSource{},
		clock:          clock.NewFakePassiveClock(testTimeNow),
	}

	got, err := stsTS.Token()
	if err != nil {
		t.Fatalf("got error: %v", err)
	}

	expected := &oauth2.Token{
		AccessToken: stsAccessToken,
		TokenType:   "Bearer",
		Expiry:      testTimeNow.Add(time.Duration(expiresInSeconds) * time.Second),
	}

	if diff := cmp.Diff(expected, got, cmpopts.IgnoreUnexported(oauth2.Token{})); diff != "" {
		t.Errorf("test requests diff (-want +got):\n%s", diff)
	}
}

func TestSTSToken_Failed(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, mockResponse())
	}))
	defer server.Close()

	stsTS := &stsTokenSource{
		tokenURI:       server.URL,
		audience:       "audience",
		jwtTokenSource: &mockJWTTokenSource{},
		sleepFn:        func(time.Duration) {},
	}

	_, err := stsTS.Token()
	if err == nil {
		t.Fatal("got nil, expected error")
	}
	// Non-429 4xx responses represent permanent client errors and must fail fast without retrying.
	if attempts != 1 {
		t.Fatalf("expected non-retryable 400 response to stop after 1 attempt, got %d", attempts)
	}
}

// TestSTSToken_RetriesTransientFailures verifies that Token() recovers across both
// abrupt TCP connection resets (attempt 1) and HTTP 503 responses (attempt 2).
func TestSTSToken_RetriesTransientFailures(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		switch attempts {
		case 1:
			// Simulate an upstream load-balancer connection reset mid-request.
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
					return
				}
			}
			w.WriteHeader(http.StatusBadGateway)
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "upstream unavailable")
		default:
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, mockResponse())
		}
	}))
	defer server.Close()

	stsTS := &stsTokenSource{
		tokenURI:       server.URL,
		audience:       "audience",
		jwtTokenSource: &mockJWTTokenSource{},
		clock:          clock.NewFakePassiveClock(testTimeNow),
		sleepFn:        func(time.Duration) {},
	}

	got, err := stsTS.Token()
	if err != nil {
		t.Fatalf("expected retry to succeed, got error: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
	if got.AccessToken != stsAccessToken {
		t.Fatalf("expected token %q, got %q", stsAccessToken, got.AccessToken)
	}
}

type mockJWTTokenSource struct {
}

func (mts *mockJWTTokenSource) Token() (*oauth2.Token, error) {
	return &oauth2.Token{AccessToken: jwtAccessToken}, nil
}

func mockResponse() string {
	tokenResp := &generateAccessTokenResp{
		AccessToken: stsAccessToken,
		ExpiresIn:   expiresInSeconds,
	}

	jsonData, _ := json.Marshal(tokenResp)
	return string(jsonData)
}

func TestNewSTSTokenSource_WithCACert(t *testing.T) {
	sa := &ServiceAccount{
		PrivateKey: "dummy",
		TokenURI:   "https://example.com/token",
	}
	ts := NewSTSTokenSource("aud", sa, WithCACert([]byte("dummy-ca"))).(*stsTokenSource)
	if ts.httpClient == nil {
		t.Fatal("expected httpClient to be configured, got nil")
	}
	if ts.httpClient.Timeout != 30*time.Second {
		t.Errorf("expected timeout 30s, got %v", ts.httpClient.Timeout)
	}
	tr, ok := ts.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", ts.httpClient.Transport)
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs == nil {
		t.Error("expected RootCAs to be set in TLSClientConfig")
	}
}

func TestNewCachedSTSTokenSource_Resettable(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(&generateAccessTokenResp{
			AccessToken: fmt.Sprintf("sts-token-%d", calls),
			ExpiresIn:   43200,
		})
	}))
	defer server.Close()

	stsTS := &stsTokenSource{
		tokenURI:       server.URL,
		audience:       "audience",
		jwtTokenSource: &mockJWTTokenSource{},
		clock:          clock.NewFakePassiveClock(time.Now()),
	}
	cached := NewCachedSTSTokenSource("audience", &ServiceAccount{TokenURI: server.URL})
	if cached == nil {
		t.Fatal("expected non-nil ResettableTokenSource")
	}

	// Verify the returned transport.ResettableTokenSource caches and resets cleanly.
	resettable := newCachedTokenSource(stsTS)
	tok1, err := resettable.Token()
	if err != nil {
		t.Fatalf("first Token() error: %v", err)
	}
	tok2, err := resettable.Token()
	if err != nil {
		t.Fatalf("second Token() error: %v", err)
	}
	if tok1.AccessToken != "sts-token-1" || tok2.AccessToken != "sts-token-1" || calls != 1 {
		t.Fatalf("expected cached token reuse (calls=%d, tok1=%q, tok2=%q)", calls, tok1.AccessToken, tok2.AccessToken)
	}

	resettable.ResetTokenOlderThan(time.Now().Add(time.Second))
	tok3, err := resettable.Token()
	if err != nil {
		t.Fatalf("post-reset Token() error: %v", err)
	}
	if tok3.AccessToken != "sts-token-2" || calls != 2 {
		t.Fatalf("expected refreshed token after reset (calls=%d, tok3=%q)", calls, tok3.AccessToken)
	}
}
