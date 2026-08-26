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
	"encoding/base64"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/googlecloudplatform/cloud-provider-gdc/gdc/pkg/auth"
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

	stsTS := auth.NewCachedSTSTokenSource(audience, serviceAccount, auth.WithCACert(certData))
	cfg := &rest.Config{
		Host: config.OrgClusterURL,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: certData,
		},
	}

	// Injects STS tokens as the bearer authorization header in the requests.
	cfg.Wrap(transport.TokenSourceWrapTransport(stsTS))

	return cfg, nil
}
