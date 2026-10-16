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

package gdcloud

import (
	"bytes"
	"strings"
	"testing"
)

func TestExec(t *testing.T) {
	tests := []struct {
		name                  string
		args                  []string
		mockedGdcloudLocation func() string
		wantOutput            []byte
		wantErrMsg            string
	}{
		{
			name:                  "Successful command execution",
			args:                  []string{"gdcloud version: 1.14.3-gdch.9425-21"},
			mockedGdcloudLocation: func() string { return "echo" },
			wantOutput:            []byte("gdcloud version: 1.14.3-gdch.9425-21\n"),
			wantErrMsg:            "",
		},
		{
			name:                  "Command returns an error",
			args:                  []string{"-c", "exit 1"},
			mockedGdcloudLocation: func() string { return "sh" },
			wantOutput:            nil,
			wantErrMsg:            "exit status 1",
		},
		{
			name:                  "Binary not found",
			args:                  []string{"invalid-arg"},
			mockedGdcloudLocation: func() string { return "nonexistent-gdcloud-binary-xyz" },
			wantOutput:            nil,
			wantErrMsg:            "cannot locate gdcloud binary",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gdcloudLocation = tt.mockedGdcloudLocation
			c := &TestingClient{configDir: "/tmp/fake"}
			gotOutput, gotErr := c.Exec(tt.args...)

			if (gotErr != nil) && !strings.Contains(gotErr.Error(), tt.wantErrMsg) {
				t.Errorf("Exec() error = %v, wantErr %v", gotErr, tt.wantErrMsg)
				return
			}
			if !bytes.Equal(gotOutput, tt.wantOutput) {
				t.Errorf("Exec() gotOutput = %q, want %q", gotOutput, tt.wantOutput)
			}
		})
	}
}
