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

// Package main provides a bootstrap CLI helper that exchanges a GDC
// ServiceAccount JSON key for an STS Bearer token scoped to a target audience URL.
// This is used in CI before the gdcloud CLI is installed to authenticate against
// the GDC Management API server and query CLIBundleMetadata.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/googlecloudplatform/cloud-provider-gdc/gdc/pkg/auth"
)

func main() {
	var (
		saPath   string
		caPath   string
		audience string
	)
	flag.StringVar(&saPath, "service-account-file", "", "Path to the GDC ServiceAccount JSON key file.")
	flag.StringVar(&caPath, "ca-cert-file", "", "Path to the GDC Root CA certificate PEM file.")
	flag.StringVar(&audience, "audience", "", "Target audience URL for the STS token exchange (e.g. GDC Management API URL).")
	flag.Parse()

	token, err := mintSTSToken(saPath, caPath, audience)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		flag.Usage()
		os.Exit(1)
	}

	// Print the raw access token to stdout so the caller script can capture it
	// for Authorization: Bearer headers.
	fmt.Print(token)
}

// mintSTSToken reads the GDC ServiceAccount key and Root CA certificate from disk
// and exchanges a locally signed ES256 JWT for an STS access token.
func mintSTSToken(saPath, caPath, audience string) (string, error) {
	if saPath == "" || caPath == "" || audience == "" {
		return "", fmt.Errorf("--service-account-file, --ca-cert-file, and --audience are all required")
	}

	// Read and parse the GDC ProjectServiceAccount JSON credential file, which
	// contains the ES256 private key, key ID, project, service account name, and STS token URI.
	saBytes, err := os.ReadFile(saPath)
	if err != nil {
		return "", fmt.Errorf("read service account file %q: %w", saPath, err)
	}

	var sa auth.ServiceAccount
	if err := json.Unmarshal(saBytes, &sa); err != nil {
		return "", fmt.Errorf("unmarshal service account JSON: %w", err)
	}

	// Read the GDC Root CA certificate so the STS HTTPS client can verify the
	// GDC ServiceIdentityServer TLS certificate.
	caBytes, err := os.ReadFile(caPath)
	if err != nil {
		return "", fmt.Errorf("read CA certificate file %q: %w", caPath, err)
	}

	// Create an STS TokenSource configured with the GDC Root CA and exchange a
	// signed JWT for a short-lived OAuth 2.0 Bearer access token scoped to the audience.
	ts := auth.NewSTSTokenSource(audience, &sa, auth.WithCACert(caBytes))
	tok, err := ts.Token()
	if err != nil {
		return "", fmt.Errorf("mint STS token for audience %q: %w", audience, err)
	}

	return tok.AccessToken, nil
}
