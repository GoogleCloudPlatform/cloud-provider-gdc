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
	"strings"
	"testing"
	"text/template"

	"github.com/google/go-cmp/cmp"
	"helm.sh/helm/v3/pkg/chartutil"
)

// zoneEndpoints is a local test helper struct that mirrors the ZoneEndpoints
// type from extension-provider/pkg/apis/gdc, used only for test data construction.
type zoneEndpoints struct {
	Name          string `json:"name"`
	ManagementAPI string `json:"managementAPI"`
}

func TestReadCloudProviderConfig(t *testing.T) {
	values := map[string]interface{}{
		"project":                  "fake-garden",
		"globalMangementServerURL": "https://global-management-server-url",
		"caData":                   "base64-string-ca",
		"zones": []*zoneEndpoints{
			{
				Name:          "a",
				ManagementAPI: "https://zonal-management-server-url",
			},
			{
				Name:          "b",
				ManagementAPI: "https://zonal-management-server-url-1",
			},
		},
	}

	parsedValues, err := json.Marshal(values)
	if err != nil {
		t.Fatalf("failed to parse values for chart %v: %v", values, err)
	}

	valuesCopy, err := chartutil.ReadValues(parsedValues)
	if err != nil {
		t.Fatalf("failed to read values for chart %v: %v", valuesCopy, err)
	}
	valuesCopy["Values"] = valuesCopy

	// helm chart template
	data := `
		[Global]
		project="{{ .Values.project }}"
		global-management-server-url="{{ .Values.globalMangementServerURL }}"
		ca="{{ .Values.caData }}"
		{{- range $index, $zone := .Values.zones }}

		[zone_{{ $index }}]
		name="{{ $zone.name }}"
		management-api="{{ $zone.managementAPI }}"
		{{- end }}
	`

	helmTemplate, err := template.New("cloudprovider.conf").Parse(data)
	if err != nil {
		t.Fatalf("Helm template creation error %v", err)
	}

	var buf strings.Builder
	if err := helmTemplate.ExecuteTemplate(&buf, "cloudprovider.conf", valuesCopy); err != nil {
		t.Fatalf("Error executing helm template %v", err)
	}

	contentString := buf.String()
	reader := strings.NewReader(contentString)
	gotConfigFile, err := readCloudProviderConfig(reader)
	if err != nil {
		t.Errorf("Error reading config file: %v", err)
	}

	wantConfigFile := configFile{
		Project:                   "fake-garden",
		GlobalManagementServerURL: "https://global-management-server-url",
		CaData:                    "base64-string-ca",
		Zones: []*ZoneEndpoint{
			{
				Name:          "a",
				ManagementAPI: "https://zonal-management-server-url",
			},
			{
				Name:          "b",
				ManagementAPI: "https://zonal-management-server-url-1",
			},
		},
	}

	if diff := cmp.Diff(wantConfigFile, *gotConfigFile); diff != "" {
		t.Errorf("readCloudProviderConfig returned unexpected diff (-want +got):\n%s", diff)
	}
}
