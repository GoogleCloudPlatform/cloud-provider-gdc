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
	"fmt"
	"os"
	"os/exec"
)

var gdcloudLocation = func() string {
	if loc := os.Getenv("GDCLOUD_PATH"); loc != "" {
		return loc
	}
	return "gdcloud"
}

// TestingClient provides a hermetic wrapper around the gdcloud CLI.
// It manages an isolated execution environment for safe concurrent testing.
type TestingClient struct {
	configDir string
}

// ConfigDir returns the path to the hermetic configuration directory managed by this client.
func (c *TestingClient) ConfigDir() string {
	return c.configDir
}

// NewTestingClient initializes a new hermetic gdcloud CLI client.
// It creates a temporary directory for gdcloud configuration and configures it with
// the provided CA data path, service account key file path, and console URL.
func NewTestingClient(caDataPath, serviceaccountPath, consoleURL string) (client *TestingClient, err error) {
	// Create a temporary HOME directory to ensure gdcloud does not mutate the user's ~/.config/gdcloud
	configDir, err := os.MkdirTemp("", "gdcloud-test-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp directory for gdcloud config: %w", err)
	}

	defer func() {
		if err != nil {
			os.RemoveAll(configDir)
		}
	}()

	c := &TestingClient{configDir: configDir}

	if result, err := c.Exec("config", "set", "auth/login_config_cert_path", caDataPath); err != nil {
		return nil, fmt.Errorf("%s, cannot execute gdcloud command: %w", result, err)
	}
	if result, err := c.Exec("config", "set", "core/organization_console_url", consoleURL); err != nil {
		return nil, fmt.Errorf("%s, cannot execute gdcloud command: %w", result, err)
	}
	if result, err := c.Exec("auth", "activate-service-account", "--key-file="+serviceaccountPath); err != nil {
		return nil, fmt.Errorf("%s, cannot execute gdcloud command: %w", result, err)
	}
	return c, nil
}

// Cleanup removes the temporary configuration directory used by the testing client.
func (c *TestingClient) Cleanup() error {
	return os.RemoveAll(c.configDir)
}

// Exec executes a gdcloud CLI command using the testing client's hermetic configuration.
func (c *TestingClient) Exec(args ...string) ([]byte, error) {
	loc := gdcloudLocation()
	if loc == "" {
		return nil, fmt.Errorf("cannot locate gdcloud binary")
	}
	executable, err := exec.LookPath(loc)
	if err != nil {
		return nil, fmt.Errorf("cannot locate gdcloud binary: %w", err)
	}
	cmd := exec.Command(executable, args...)
	// Override HOME and XDG_CONFIG_HOME to guarantee hermetic testing.
	cmd.Env = append(os.Environ(),
		"HOME="+c.configDir,
		"XDG_CONFIG_HOME="+c.configDir,
	)

	result, err := cmd.CombinedOutput()
	if err != nil {
		return result, fmt.Errorf("cannot execute gdcloud command: %w", err)
	}
	return result, nil
}
