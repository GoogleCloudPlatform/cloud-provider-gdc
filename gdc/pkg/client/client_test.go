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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/googlecloudplatform/cloud-provider-gdc/gdc/pkg/auth"
)

func generateTestECPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal ECDSA key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

func TestGetRESTConfig_ResetsCachedTokenOn401(t *testing.T) {
	var stsCalls atomic.Int32
	stsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := stsCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("sts-token-%d", n),
			"expires_in":   43200, // 12h TTL matching GDC STS
		})
	}))
	defer stsServer.Close()

	var invalidateFirstToken atomic.Bool
	var lastAuthHeader atomic.Value
	apiServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHdr := r.Header.Get("Authorization")
		lastAuthHeader.Store(authHdr)
		if invalidateFirstToken.Load() && authHdr == "Bearer sts-token-1" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"kind":"Status","status":"Failure","reason":"Unauthorized","code":401}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer apiServer.Close()

	var caBundle []byte
	for _, cert := range []*x509.Certificate{stsServer.Certificate(), apiServer.Certificate()} {
		caBundle = append(caBundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
	}

	sa := &auth.ServiceAccount{
		Name:         "test-sa",
		Project:      "test-project",
		PrivateKeyID: "test-key-id",
		PrivateKey:   generateTestECPrivateKeyPEM(t),
		TokenURI:     stsServer.URL,
	}

	restCfg, err := getRESTConfig(&OrgClusterConfig{
		OrgClusterURL: apiServer.URL,
		CAData:        base64.StdEncoding.EncodeToString(caBundle),
	}, sa)
	if err != nil {
		t.Fatalf("getRESTConfig: %v", err)
	}

	httpClient, err := rest.HTTPClientFor(restCfg)
	if err != nil {
		t.Fatalf("rest.HTTPClientFor: %v", err)
	}

	doRequest := func() int {
		t.Helper()
		resp, err := httpClient.Get(apiServer.URL + "/api/v1/nodes")
		if err != nil {
			t.Fatalf("GET failed: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	// Request 1 acquires sts-token-1.
	if got := doRequest(); got != http.StatusOK {
		t.Fatalf("request 1 status = %d, want 200", got)
	}
	if got := stsCalls.Load(); got != 1 {
		t.Fatalf("after request 1 stsCalls = %d, want 1", got)
	}
	if got := lastAuthHeader.Load().(string); got != "Bearer sts-token-1" {
		t.Fatalf("request 1 auth = %q, want Bearer sts-token-1", got)
	}

	// Request 2 reuses cached sts-token-1 without calling STS again.
	if got := doRequest(); got != http.StatusOK {
		t.Fatalf("request 2 status = %d, want 200", got)
	}
	if got := stsCalls.Load(); got != 1 {
		t.Fatalf("after request 2 stsCalls = %d, want 1 (cached)", got)
	}

	// Simulate server-side key rotation invalidating sts-token-1 well before its 12h TTL.
	// Sleep briefly so ResetTokenOlderThan(start) is strictly after the cached token's fetch timestamp.
	time.Sleep(2 * time.Millisecond)
	invalidateFirstToken.Store(true)

	// Request 3 receives 401 and must evict the cached token.
	if got := doRequest(); got != http.StatusUnauthorized {
		t.Fatalf("request 3 status = %d, want 401", got)
	}
	if got := stsCalls.Load(); got != 1 {
		t.Fatalf("after request 3 stsCalls = %d, want 1", got)
	}

	// Request 4 automatically refreshes the token via STS and succeeds.
	if got := doRequest(); got != http.StatusOK {
		t.Fatalf("request 4 status = %d, want 200 (after 401 token reset)", got)
	}
	if got := stsCalls.Load(); got != 2 {
		t.Fatalf("after request 4 stsCalls = %d, want 2", got)
	}
	if got := lastAuthHeader.Load().(string); got != "Bearer sts-token-2" {
		t.Fatalf("request 4 auth = %q, want Bearer sts-token-2", got)
	}

	// Request 5 reuses the refreshed cached sts-token-2.
	if got := doRequest(); got != http.StatusOK {
		t.Fatalf("request 5 status = %d, want 200", got)
	}
	if got := stsCalls.Load(); got != 2 {
		t.Fatalf("after request 5 stsCalls = %d, want 2 (cached)", got)
	}
}
