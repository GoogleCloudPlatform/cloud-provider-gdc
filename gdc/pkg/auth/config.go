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

// Config of GDC service identity private key.
type ServiceAccount struct {
	//Name is the Service account name. It will be used as the `iss` and `sub` claim in the self signed JWT.
	Name string `json:"name"`

	// Project ID associated with the service account credential.
	Project string `json:"project"`

	// The OAuth 2.0 Token URI.
	TokenURI string `json:"token_uri"`

	// ID of private key
	PrivateKeyID string `json:"private_key_id"`

	// Private key plain text content
	PrivateKey string `json:"private_key"`
}
