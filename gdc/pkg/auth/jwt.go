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
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"
)

type jwtSigner interface {
	signJWTWithKey(kid, key, sub, issuer, audience string) (string, error)
}

type defaultSigner struct{}

func (s *defaultSigner) signJWTWithKey(kid, key, sub, issuer, audience string) (string, error) {
	return signJWTWithKey(kid, key, sub, issuer, audience)
}

func newJWTTokenSource(config *ServiceAccount) oauth2.TokenSource {
	return &jwtTokenSource{
		config: config,
		signer: &defaultSigner{},
	}
}

type jwtTokenSource struct {
	config *ServiceAccount
	signer jwtSigner
}

func (ts *jwtTokenSource) Token() (*oauth2.Token, error) {
	// The service account name is both the issuer and the subject of the signed JWT.
	issSubValue := fmt.Sprintf("system:serviceaccount:%s:%s", ts.config.Project, ts.config.Name)
	jwtToken, err := ts.signer.signJWTWithKey(ts.config.PrivateKeyID, ts.config.PrivateKey, issSubValue, issSubValue, ts.config.TokenURI)
	if err != nil {
		return nil, fmt.Errorf("jwt token: %w", err)
	}

	// No need to populate expiry as JWT is for token exchange
	return &oauth2.Token{AccessToken: jwtToken}, nil
}

// signJWTWithKey signs a JWT using a known private key to be used with
// the service identity server in order to exchange for an audienced
// STS token. This only supports signing the JWT with ECDSA 256 keys.
//
// The assertion is re-signed on every token exchange and is only used to
// obtain a short-lived STS token, so it is given a short validity window
// (1 hour) and a unique jti to minimize the replay surface if captured.
func signJWTWithKey(kid, key, sub, issuer, audience string) (string, error) {
	priv, err := parsePrivateKey(key)
	if err != nil {
		return "", err
	}

	jti, err := newJTI()
	if err != nil {
		return "", err
	}

	now := time.Now()
	claims := jwt.RegisteredClaims{
		ID:        jti,
		Issuer:    issuer,
		Subject:   sub,
		Audience:  jwt.ClaimStrings{audience},
		NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		IssuedAt:  jwt.NewNumericDate(now),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["kid"] = kid
	token.Header["typ"] = "JWT"

	return token.SignedString(priv)
}

// newJTI returns a random token identifier for the "jti" claim.
func newJTI() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate jti: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// parsePrivateKey parses a private key string into an *ecdsa.PrivateKey.
func parsePrivateKey(key string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(key))
	if block == nil {
		return nil, fmt.Errorf("private key decoded into nil PEM block")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}
