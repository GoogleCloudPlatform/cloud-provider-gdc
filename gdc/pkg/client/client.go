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

// Package client provides a common function to authenticate and instantiate a
// Kubernetes client for GDC.
package client

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"k8s.io/apimachinery/pkg/runtime"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/googlecloudplatform/cloud-provider-gdc/gdc/pkg/auth"
)

const (
	// Short TCP dial timeout so new connections to Global or Zonal API servers
	// fail fast when an upstream load-balancer backend is unreachable.
	defaultDialTimeout     = 5 * time.Second
	defaultKeepAlivePeriod = 30 * time.Second

	// Refresh cached STS tokens 60s before expiry so clock skew between replicas
	// does not cause mid-flight 401 Unauthorized responses.
	defaultTokenExpiryLeeway = time.Minute

	defaultMaxTransportAttempts = 4
	defaultInitialRetryBackoff  = 250 * time.Millisecond
	defaultMaxRetryBackoff      = 2 * time.Second
)

type KubeClients struct {
	// Global client used for interacting with GDC global resources.
	GlobalAPIServer client.WithWatch `json:"globalAPIServer,omitempty"`
	// Map of zonal clients, used for interacting with GDC zonal resources.
	ZonalAPIServers map[string]client.WithWatch `json:"zonalAPIServers,omitempty"`
}

// Get returns a Kubernetes client to the specified cluster of GDC.
func Get(
	config *OrgClusterConfig,
	serviceAccount *auth.ServiceAccount,
	scheme *runtime.Scheme,
) (client.WithWatch, error) {
	restConfig, err := getRESTConfig(config, serviceAccount)
	if err != nil {
		return nil, fmt.Errorf("retrieving Kubernetes REST configuration: %w", err)
	}

	c, err := client.NewWithWatch(restConfig, client.Options{
		Scheme: scheme,
	})
	if err != nil {
		return nil, fmt.Errorf("create kubernetes client from config: %w", err)
	}

	return c, nil
}

func getRESTConfig(config *OrgClusterConfig, serviceAccount *auth.ServiceAccount) (*rest.Config, error) {
	certData, err := base64.StdEncoding.DecodeString(config.CAData)
	if err != nil {
		return nil, fmt.Errorf("decode cadata: %w", err)
	}
	audience := config.OrgClusterURL

	baseSTSTS := auth.NewSTSTokenSource(audience, serviceAccount, auth.WithCACert(certData))
	cachedTS := newResettableTokenSource(baseSTSTS, defaultTokenExpiryLeeway)
	cfg := &rest.Config{
		Host: config.OrgClusterURL,
		Dial: (&net.Dialer{
			Timeout:   defaultDialTimeout,
			KeepAlive: defaultKeepAlivePeriod,
		}).DialContext,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: certData,
		},
	}

	// Injects STS Bearer tokens and retries transient 401/429/502/503/504 responses
	// (resetting the cached token and evicting stale keep-alive connections on 401).
	cfg.Wrap(newRetryingAuthWrapTransport(cachedTS))

	return cfg, nil
}

// resettableTokenSource caches OAuth2 tokens with a proactive refresh leeway and
// allows callers to invalidate tokens minted before a failed 401 request.
type resettableTokenSource struct {
	base   oauth2.TokenSource
	leeway time.Duration
	nowFn  func() time.Time

	mu       sync.RWMutex
	tok      *oauth2.Token
	mintedAt time.Time
}

func newResettableTokenSource(base oauth2.TokenSource, leeway time.Duration) *resettableTokenSource {
	return &resettableTokenSource{
		base:   base,
		leeway: leeway,
		nowFn:  time.Now,
	}
}

func (s *resettableTokenSource) isValidLocked(now time.Time) bool {
	if s.tok == nil || s.tok.AccessToken == "" {
		return false
	}
	if s.tok.Expiry.IsZero() {
		return true
	}
	leeway := s.leeway
	if ttl := s.tok.Expiry.Sub(s.mintedAt); ttl > 0 && ttl <= 2*leeway {
		leeway = ttl / 2
	}
	return s.tok.Expiry.Add(-leeway).After(now)
}

func (s *resettableTokenSource) isUnexpiredLocked(now time.Time) bool {
	if s.tok == nil || s.tok.AccessToken == "" {
		return false
	}
	return s.tok.Expiry.IsZero() || s.tok.Expiry.After(now)
}

func (s *resettableTokenSource) Token() (*oauth2.Token, error) {
	now := s.nowFn()
	s.mu.RLock()
	if s.isValidLocked(now) {
		tok := s.tok
		s.mu.RUnlock()
		return tok, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	now = s.nowFn()
	if s.isValidLocked(now) {
		return s.tok, nil
	}

	tok, err := s.base.Token()
	if err != nil {
		if s.isUnexpiredLocked(s.nowFn()) {
			klog.Warningf("Unable to refresh STS token; continuing with cached token until expiry: %v", err)
			return s.tok, nil
		}
		return nil, err
	}
	s.tok = tok
	s.mintedAt = s.nowFn()
	return tok, nil
}

func (s *resettableTokenSource) ResetTokenOlderThan(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok != nil && !s.mintedAt.After(t) {
		s.tok = nil
		s.mintedAt = time.Time{}
	}
}

func newRetryingAuthWrapTransport(ts *resettableTokenSource) func(http.RoundTripper) http.RoundTripper {
	return func(rt http.RoundTripper) http.RoundTripper {
		return &retryingAuthTransport{
			base:           rt,
			tokenSource:    ts,
			maxAttempts:    defaultMaxTransportAttempts,
			initialBackoff: defaultInitialRetryBackoff,
			sleepFn:        sleepWithContext,
		}
	}
}

type retryingAuthTransport struct {
	base           http.RoundTripper
	tokenSource    *resettableTokenSource
	maxAttempts    int
	initialBackoff time.Duration
	sleepFn        func(context.Context, time.Duration) error
}

var _ utilnet.RoundTripperWrapper = (*retryingAuthTransport)(nil)

func (t *retryingAuthTransport) WrappedRoundTripper() http.RoundTripper {
	return t.base
}

func (t *retryingAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Respect explicit Authorization headers without overriding them.
	hasExplicitAuth := req.Header.Get("Authorization") != ""

	maxAttempts := t.maxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxTransportAttempts
	}
	if !isReplayableBody(req) {
		maxAttempts = 1
	}

	backoff := t.initialBackoff
	if backoff <= 0 {
		backoff = defaultInitialRetryBackoff
	}
	sleep := t.sleepFn
	if sleep == nil {
		sleep = sleepWithContext
	}

	tokenRefreshed := false

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		attemptReq, err := cloneRequestForAttempt(req, attempt)
		if err != nil {
			return nil, err
		}

		var authFetchedAt time.Time
		if !hasExplicitAuth {
			tok, tokErr := t.tokenSource.Token()
			if tokErr != nil {
				return nil, tokErr
			}
			authFetchedAt = t.tokenSource.nowFn()
			attemptReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		}

		resp, doErr := t.base.RoundTrip(attemptReq)
		if doErr != nil {
			if attempt < maxAttempts && req.Context().Err() == nil && isIdempotentMethod(req.Method) {
				closeIdleConnections(t.base)
				if sleepErr := sleep(req.Context(), backoff); sleepErr != nil {
					return nil, doErr
				}
				backoff = nextBackoff(backoff)
				continue
			}
			return nil, doErr
		}

		if resp.StatusCode == http.StatusUnauthorized && !hasExplicitAuth {
			if !tokenRefreshed {
				t.tokenSource.ResetTokenOlderThan(authFetchedAt)
				tokenRefreshed = true
			}
			if attempt < maxAttempts && req.Context().Err() == nil {
				drainAndClose(resp.Body)
				closeIdleConnections(t.base)
				if sleepErr := sleep(req.Context(), backoff); sleepErr != nil {
					return nil, sleepErr
				}
				backoff = nextBackoff(backoff)
				continue
			}
			closeIdleConnections(t.base)
		} else if isRetryableGatewayStatus(resp.StatusCode) && !hasRetryAfterHeader(resp) && attempt < maxAttempts && req.Context().Err() == nil {
			drainAndClose(resp.Body)
			closeIdleConnections(t.base)
			if sleepErr := sleep(req.Context(), backoff); sleepErr != nil {
				return nil, sleepErr
			}
			backoff = nextBackoff(backoff)
			continue
		}

		return resp, nil
	}

	return nil, fmt.Errorf("exhausted %d transport attempts for %s %s", maxAttempts, req.Method, req.URL)
}

func isReplayableBody(req *http.Request) bool {
	return req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
}

func cloneRequestForAttempt(req *http.Request, attempt int) (*http.Request, error) {
	cloned := req.Clone(req.Context())
	if attempt == 1 || req.Body == nil || req.Body == http.NoBody {
		return cloned, nil
	}
	bodyCopy, err := req.GetBody()
	if err != nil {
		return nil, fmt.Errorf("rewind request body for retry: %w", err)
	}
	cloned.Body = bodyCopy
	return cloned, nil
}

func isIdempotentMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

func isRetryableGatewayStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func hasRetryAfterHeader(resp *http.Response) bool {
	return resp != nil && strings.TrimSpace(resp.Header.Get("Retry-After")) != ""
}

func nextBackoff(cur time.Duration) time.Duration {
	next := cur * 2
	if next > defaultMaxRetryBackoff {
		return defaultMaxRetryBackoff
	}
	return next
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func drainAndClose(rc io.ReadCloser) {
	if rc == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 4<<10))
	_ = rc.Close()
}

func closeIdleConnections(rt http.RoundTripper) {
	type idleCloser interface {
		CloseIdleConnections()
	}
	for rt != nil {
		if ic, ok := rt.(idleCloser); ok {
			ic.CloseIdleConnections()
			return
		}
		if wrapper, ok := rt.(utilnet.RoundTripperWrapper); ok {
			rt = wrapper.WrappedRoundTripper()
			continue
		}
		return
	}
}
