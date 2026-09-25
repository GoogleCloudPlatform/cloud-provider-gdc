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
		clock: clock.RealClock{},
	}
}

type stsTokenSource struct {
	tokenURI       string
	audience       string
	jwtTokenSource oauth2.TokenSource
	httpClient     *http.Client
	clock          clock.PassiveClock
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

	body := bytes.NewReader(jsonData)
	req, err := http.NewRequest("POST", ts.tokenURI, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := ts.httpClient
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch token: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read fetched token: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &oauth2.RetrieveError{
			Response: resp,
			Body:     respBody,
		}
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
