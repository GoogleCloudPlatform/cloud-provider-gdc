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

package gdc

import (
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/googlecloudplatform/cloud-provider-gdc/integration/pkg/gdcloud"
)

const (
	defaultClientQPS   = 100.0
	defaultClientBurst = 150
)

// GetUserClusterKubeconfig fetches credentials for a user cluster using the gdcloud CLI.
// It saves the credentials to a temporary kubeconfig file in /tmp/ and returns the path to this file.
func GetUserClusterKubeconfig(gdcloudClient *gdcloud.TestingClient, zone, project, cluster string) (string, error) {
	kubeconfigPath := fmt.Sprintf("/tmp/%s-kubeconfig", cluster)
	if err := os.Setenv("KUBECONFIG", kubeconfigPath); err != nil {
		return "", fmt.Errorf("unable to set KUBECONFIG variable %w", err)
	}
	result, err := gdcloudClient.Exec("clusters", "get-credentials", cluster, "--project="+project, "--zone="+zone, "--standard")
	if err != nil {
		return "", fmt.Errorf("failed to get cluster credentials (output: %s): %w", result, err)
	}
	if err := injectHermeticEnvIntoKubeconfig(kubeconfigPath, gdcloudClient.ConfigDir()); err != nil {
		return "", fmt.Errorf("failed to inject hermetic env to kubeconfig: %w", err)
	}
	return kubeconfigPath, nil
}

// GetUserClusterClient creates a new client for a given user cluster.
// It first calls GetUserClusterKubeconfig to obtain the cluster's credentials,
// then builds and returns a client configured to communicate with that cluster.
func GetUserClusterClient(gdcloudClient *gdcloud.TestingClient, zone, project, cluster string, scheme *runtime.Scheme) (client.WithWatch, error) {
	kubeconfigPath, err := GetUserClusterKubeconfig(gdcloudClient, zone, project, cluster)
	if err != nil {
		return nil, fmt.Errorf("unable to get cluster credential: %w", err)
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("error building kubeconfig: %w", err)
	}
	config.QPS = defaultClientQPS
	config.Burst = defaultClientBurst
	return client.NewWithWatch(config, client.Options{
		Scheme: scheme,
	})
}

// injectHermeticEnvIntoKubeconfig injects the given config directory into the
// generated kubeconfig's ExecConfig for HOME and XDG_CONFIG_HOME.
// This ensures that any downstream tools (e.g. client-go) running the gdcloud
// auth plugin do so hermetically using the same temporary config directory.
func injectHermeticEnvIntoKubeconfig(path, configDir string) error {
	rawCfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return fmt.Errorf("failed to load kubeconfig %s: %w", path, err)
	}

	for _, authInfo := range rawCfg.AuthInfos {
		if authInfo.Exec != nil {
			var env []clientcmdapi.ExecEnvVar
			for _, e := range authInfo.Exec.Env {
				if e.Name != "HOME" && e.Name != "XDG_CONFIG_HOME" {
					env = append(env, e)
				}
			}
			env = append(env,
				clientcmdapi.ExecEnvVar{Name: "HOME", Value: configDir},
				clientcmdapi.ExecEnvVar{Name: "XDG_CONFIG_HOME", Value: configDir},
			)
			authInfo.Exec.Env = env
		}
	}

	if err := clientcmd.WriteToFile(*rawCfg, path); err != nil {
		return fmt.Errorf("failed to write kubeconfig %s: %w", path, err)
	}

	return nil
}
