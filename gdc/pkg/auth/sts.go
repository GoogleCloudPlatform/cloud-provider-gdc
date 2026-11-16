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
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"golang.org/x/oauth2"
	"k8s.io/client-go/transport"
	"k8s.io/utils/clock"
)

const (
	tokenExchangeType       = "urn:ietf:params:oauth:token-type:token-exchange"
	accessTokenType         = "urn:ietf:params:oauth:token-type:access_token"
	serviceAccountTokenType = "urn:k8s:params:oauth:token-type:serviceaccount"

	// Bound TCP dial and TLS handshake timeouts so connections hitting an
	// unhealthy load-balancer backend fail quickly instead of stalling for 15-30s,
	// and retry transient failures with capped exponential backoff.
	stsDialTimeout           = 5 * time.Second
	stsTLSHandshakeTimeout   = 5 * time.Second
	stsKeepAlivePeriod       = 30 * time.Second
	stsDefaultMaxAttempts    = 6
	stsDefaultInitialBackoff = 500 * time.Millisecond
	stsMaxBackoff            = 4 * time.Second
)

type Option func(*optionalConfig)

// WithCACert sets the CA certificate to be used for server certificate
// validation.
func WithCACert(caCert []byte) Option {
	return func(cfg *optionalConfig) {
		cfg.caCert = caCert
	}
}

// Creates an oauth2.TokenSource that caches STS tokens for the specified audience.
// The CachedSTSTokenSource will request a new STS token when the STS token it is
// expired or nearing the expiry time.
func NewCachedSTSTokenSource(
	audience string,
	saConfig *ServiceAccount,
	opts ...Option,
) oauth2.TokenSource {
	stsTS := NewSTSTokenSource(audience, saConfig, opts...)
	return transport.NewCachedTokenSource(stsTS)
}

type optionalConfig struct {
	caCert []byte
}

func NewSTSTokenSource(
	audience string,
	saConfig *ServiceAccount,
	opts ...Option,
) oauth2.TokenSource {
	cfg := &optionalConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	jwtTS := newJWTTokenSource(saConfig)

	tr := http.DefaultTransport.(*http.Transport).Clone()
	// Fail fast on dead VIP backends during connection setup while retaining the
	// 30s per-attempt request timeout on httpClient below.
	tr.DialContext = (&net.Dialer{
		Timeout:   stsDialTimeout,
		KeepAlive: stsKeepAlivePeriod,
	}).DialContext
	tr.TLSHandshakeTimeout = stsTLSHandshakeTimeout
	if len(cfg.caCert) != 0 {
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(cfg.caCert)
		tr.TLSClientConfig = &tls.Config{RootCAs: caCertPool}
	}

	return &stsTokenSource{
		tokenURI:       saConfig.TokenURI,
		audience:       audience,
		jwtTokenSource: jwtTS,
		httpClient: &http.Client{
			Transport: tr,
			Timeout:   30 * time.Second,
		},
		clock:          clock.RealClock{},
		maxAttempts:    stsDefaultMaxAttempts,
		initialBackoff: stsDefaultInitialBackoff,
	}
}

type stsTokenSource struct {
	tokenURI       string
	audience       string
	jwtTokenSource oauth2.TokenSource
	httpClient     *http.Client
	clock          clock.PassiveClock
	maxAttempts    int                 // defaults to stsDefaultMaxAttempts when <= 0
	initialBackoff time.Duration       // defaults to stsDefaultInitialBackoff when <= 0
	sleepFn        func(time.Duration) // injectable sleep hook to keep unit tests fast
}

type generateAccessTokenResp struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"` // relative seconds from now
}

// Exchanges signed JWT token for STS token.
// Equivalent python method: https://github.com/googleapis/google-auth-library-python/blob/main/google/oauth2/gdch_credentials.py#L124
func (ts *stsTokenSource) Token() (*oauth2.Token, error) {
	jwtToken, err := ts.jwtTokenSource.Token()
	if err != nil {
		return nil, fmt.Errorf("jwt token: %v", err)
	}

	data := map[string]string{
		"grant_type":           tokenExchangeType,
		"audience":             ts.audience,
		"requested_token_type": accessTokenType,
		"subject_token":        jwtToken.AccessToken,
		"subject_token_type":   serviceAccountTokenType,
	}
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshal request body: %v", err)
	}

	client := ts.httpClient
	if client == nil {
		client = http.DefaultClient
	}

	maxAttempts := ts.maxAttempts
	if maxAttempts <= 0 {
		maxAttempts = stsDefaultMaxAttempts
	}
	backoff := ts.initialBackoff
	if backoff <= 0 {
		backoff = stsDefaultInitialBackoff
	}
	sleep := ts.sleepFn
	if sleep == nil {
		sleep = time.Sleep
	}

	var (
		respBody []byte
		lastErr  error
	)
	// Retry transient transport errors, truncated body reads, HTTP 429, and HTTP 5xx
	// responses so a single unhealthy STS/ingress replica does not fail authentication.
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Create a fresh body reader on every attempt because client.Do consumes it.
		req, reqErr := http.NewRequest("POST", ts.tokenURI, bytes.NewReader(jsonData))
		if reqErr != nil {
			return nil, fmt.Errorf("create request: %v", reqErr)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, doErr := client.Do(req)
		if doErr != nil {
			lastErr = fmt.Errorf("fetch token: %w", doErr)
		} else {
			// Close resp.Body immediately inside the loop rather than deferring until Token() returns.
			bodyBytes, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				lastErr = fmt.Errorf("read fetched token: %w", readErr)
			} else if resp.StatusCode == http.StatusOK {
				respBody = bodyBytes
				lastErr = nil
				break
			} else {
				retrieveErr := &oauth2.RetrieveError{
					Response: resp,
					Body:     bodyBytes,
				}
				// Do not retry permanent 4xx client/credential errors (except 429 Too Many Requests).
				if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < http.StatusInternalServerError {
					return nil, retrieveErr
				}
				lastErr = retrieveErr
			}
		}

		if attempt < maxAttempts {
			sleep(backoff)
			backoff *= 2
			if backoff > stsMaxBackoff {
				backoff = stsMaxBackoff
			}
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}

	var tokenResp generateAccessTokenResp
	if err := json.Unmarshal(respBody, &tokenResp); err != nil {
		return nil, fmt.Errorf("unmarshal: %v", err)
	}
	expiry := ts.clock.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	return &oauth2.Token{
		AccessToken: tokenResp.AccessToken,
		TokenType:   "Bearer",
		Expiry:      expiry,
	}, nil
}
