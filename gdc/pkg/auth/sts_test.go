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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, mockResponse())
	}))
	defer server.Close()

	stsTS := &stsTokenSource{
		tokenURI:       server.URL,
		audience:       "audience",
		jwtTokenSource: &mockJWTTokenSource{},
	}

	_, err := stsTS.Token()
	if err == nil {
		t.Fatal("got nil, expected error")
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
