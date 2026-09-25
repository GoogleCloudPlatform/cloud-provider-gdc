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
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	accessToken = "test-access-token"
)

func TestJWTToken(t *testing.T) {
	saConfig := serviceAccountKey()
	jwtTS := &jwtTokenSource{
		config: saConfig,
		signer: &mockSigner{},
	}

	token, err := jwtTS.Token()
	if err != nil {
		t.Fatalf("got error: %v", err)
	}

	if token.AccessToken != accessToken {
		t.Fatalf("got access token (%s), expected (%s)", token.AccessToken, accessToken)
	}
}

func TestToken_Failed(t *testing.T) {
	saConfig := serviceAccountKey()
	jwtTS := &jwtTokenSource{
		config: saConfig,
		signer: &mockSignerError{},
	}

	_, err := jwtTS.Token()
	if err == nil {
		t.Fatal("got nil, expected error")
	}
}

func serviceAccountKey() *ServiceAccount {
	return &ServiceAccount{
		Name:         "name",
		Project:      "project",
		TokenURI:     "token_uri",
		PrivateKeyID: "1234",
		PrivateKey:   "key",
	}
}

type mockSigner struct {
}

func (m *mockSigner) signJWTWithKey(kid, key, sub, issuer, audience string) (string, error) {
	return accessToken, nil
}

type mockSignerError struct {
}

func (m *mockSignerError) signJWTWithKey(kid, key, sub, issuer, audience string) (string, error) {
	return "", errors.New("failed")
}

func TestSignJWTWithKey(t *testing.T) {
	// Generate real ECDSA P-256 key
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(privKey)
	if err != nil {
		t.Fatalf("failed to marshal private key: %v", err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: der,
	}))

	kid := "test-key-id"
	sub := "system:serviceaccount:test-project:test-sa"
	issuer := "system:serviceaccount:test-project:test-sa"
	audience := "https://iam.googleapis.com/v1/token"

	tokenString, err := signJWTWithKey(kid, pemKey, sub, issuer, audience)
	if err != nil {
		t.Fatalf("signJWTWithKey failed: %v", err)
	}

	// Parse and verify token using golang-jwt/jwt/v5
	parsedToken, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodECDSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return &privKey.PublicKey, nil
	})
	if err != nil {
		t.Fatalf("failed to parse/verify signed JWT: %v", err)
	}

	if !parsedToken.Valid {
		t.Fatalf("expected token to be valid")
	}

	// Verify headers
	if parsedToken.Header["kid"] != kid {
		t.Errorf("got kid %v, want %v", parsedToken.Header["kid"], kid)
	}
	if parsedToken.Header["alg"] != "ES256" {
		t.Errorf("got alg %v, want ES256", parsedToken.Header["alg"])
	}
	if parsedToken.Header["typ"] != "JWT" {
		t.Errorf("got typ %v, want JWT", parsedToken.Header["typ"])
	}

	// Verify claims
	claims, ok := parsedToken.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatalf("failed to cast claims to MapClaims")
	}
	if claims["iss"] != issuer {
		t.Errorf("got iss %v, want %v", claims["iss"], issuer)
	}
	if claims["sub"] != sub {
		t.Errorf("got sub %v, want %v", claims["sub"], sub)
	}
	switch aud := claims["aud"].(type) {
	case string:
		if aud != audience {
			t.Errorf("got aud %v, want %v", aud, audience)
		}
	case []interface{}:
		if len(aud) == 0 || aud[0] != audience {
			t.Errorf("got aud %v, want [%v]", aud, audience)
		}
	default:
		t.Errorf("unexpected aud type %T: %v", aud, aud)
	}

	exp, ok := claims["exp"].(float64)
	if !ok || time.Unix(int64(exp), 0).Before(time.Now()) {
		t.Errorf("invalid exp claim: %v", claims["exp"])
	}
}

func TestSignJWTWithKey_InvalidKey(t *testing.T) {
	_, err := signJWTWithKey("kid", "not-a-pem-key", "sub", "issuer", "audience")
	if err == nil {
		t.Fatalf("expected error with invalid key, got nil")
	}
}

func TestParsePrivateKey_InvalidPEM(t *testing.T) {
	_, err := parsePrivateKey("invalid")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}
