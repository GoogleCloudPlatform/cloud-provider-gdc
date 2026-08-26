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

package gdc

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/ini.v1"

	klog "k8s.io/klog/v2"

	"github.com/googlecloudplatform/cloud-provider-gdc/gdc/pkg/auth"
)

// https://ini.unknwon.io/docs/advanced/map_and_reflect
type configFile struct {
	// GDC project / namespace
	Project string `ini:"project"`

	// Name of the org
	Org string `ini:"org-admin-name"`

	// The login URL of the global cluster of the 'Org'
	GlobalManagementServerURL string `ini:"global-management-server-url"`

	// The zone name and zonal management URL
	Zones []*ZoneEndpoint

	// Base64 encoded certificateAuthorityData of org admin
	CaData string `ini:"ca"`
}

type ZoneEndpoint struct {
	Name          string `ini:"name"`
	ManagementAPI string `ini:"management-api"`
}

// For given environment variable containing file path to a service account
// json file returns a ServiceAccount
func getServiceAccount(filePathEnvVar string) (*auth.ServiceAccount, error) {
	var serviceAccount auth.ServiceAccount

	filePath, err := readEnvVar(filePathEnvVar)
	if err != nil {
		if filePathEnvVar == "GDC_ORG_ADMIN_SERVICE_ACCOUNT" {
			filePath, err = readEnvVar("GDCH_ORG_ADMIN_SERVICE_ACCOUNT")
		}
		if err != nil {
			return nil, err
		}
	}

	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	if err := json.NewDecoder(file).Decode(&serviceAccount); err != nil {
		return nil, err
	}
	return &serviceAccount, nil
}

// readEnvVar reads the environment variable with the given name.
// If the environment variable is not set, an error is returned.
func readEnvVar(varName string) (string, error) {
	value := os.Getenv(varName)

	if value == "" {
		return "", fmt.Errorf("unable to read environment variable %s", varName)
	}

	return value, nil
}

// readCloudProviderConfig reads the configuration from the given reader.
// The configuration is expected to be in INI format.
func readCloudProviderConfig(config io.Reader) (*configFile, error) {
	configFile := new(configFile)

	cfg, err := ini.Load(config)
	if err != nil {
		return nil, err
	}

	if err := cfg.Section("Global").MapTo(&configFile); err != nil {
		return nil, fmt.Errorf("unable to map configuration to struct: %w", err)
	}

	for _, section := range cfg.Sections() {
		if strings.HasPrefix(section.Name(), "zone_") {
			zone := new(ZoneEndpoint)
			err := section.MapTo(zone)
			if err != nil {
				klog.Errorf("Warning: Failed to map section '%s' to ZoneEndpoints: %v", section.Name(), err)
				continue
			}
			configFile.Zones = append(configFile.Zones, zone)
		}
	}

	return configFile, nil
}
