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
	"errors"
	"testing"
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
