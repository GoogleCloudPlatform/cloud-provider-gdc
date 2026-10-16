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

package helm

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/release"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

// InstallOptions contains the options for installing a Helm chart.
type InstallOptions struct {
	ChartPath      string
	KubeconfigPath string
	ReleaseName    string
	Namespace      string
	Values         map[string]interface{}
	Timeout        time.Duration
}

// UninstallOptions contains the options for uninstalling a Helm chart.
type UninstallOptions struct {
	KubeconfigPath string
	ReleaseName    string
	Namespace      string
	IgnoreNotFound bool
	Wait           bool
}

// InstallOrUpgrade installs or upgrades a Helm chart.
func InstallOrUpgrade(opts InstallOptions) (*release.Release, error) {
	chrt, err := loadChart(opts.ChartPath)
	if err != nil {
		return nil, err
	}

	actionConfig, err := newActionConfig(opts.KubeconfigPath, opts.Namespace)
	if err != nil {
		return nil, err
	}

	listClient := action.NewList(actionConfig)
	listClient.Filter = opts.ReleaseName
	releases, err := listClient.Run()
	if err != nil {
		return nil, fmt.Errorf("failed to list helm releases: %w", err)
	}

	if len(releases) == 0 {
		return installChart(actionConfig, chrt, opts)
	}

	return upgradeChart(actionConfig, chrt, opts)
}

func installChart(actionConfig *action.Configuration, chrt *chart.Chart, opts InstallOptions) (*release.Release, error) {
	installClient := action.NewInstall(actionConfig)
	installClient.ReleaseName = opts.ReleaseName
	installClient.Namespace = opts.Namespace
	installClient.CreateNamespace = true
	installClient.Timeout = opts.Timeout

	rel, err := installClient.Run(chrt, opts.Values)
	if err != nil {
		return nil, fmt.Errorf("failed to run helm install: %w", err)
	}
	return rel, nil
}

func upgradeChart(actionConfig *action.Configuration, chrt *chart.Chart, opts InstallOptions) (*release.Release, error) {
	upgradeClient := action.NewUpgrade(actionConfig)
	upgradeClient.Namespace = opts.Namespace
	upgradeClient.Timeout = opts.Timeout

	rel, err := upgradeClient.Run(opts.ReleaseName, chrt, opts.Values)
	if err != nil {
		return nil, fmt.Errorf("failed to run helm upgrade: %w", err)
	}
	return rel, nil
}

// Uninstall removes a specified Helm release from a cluster.
func Uninstall(opts UninstallOptions) (*release.UninstallReleaseResponse, error) {
	actionConfig, err := newActionConfig(opts.KubeconfigPath, opts.Namespace)
	if err != nil {
		return nil, err
	}

	uninstallClient := action.NewUninstall(actionConfig)
	uninstallClient.Wait = opts.Wait
	if opts.Wait {
		uninstallClient.Timeout = 5 * time.Minute
	}
	uninstallClient.IgnoreNotFound = opts.IgnoreNotFound
	res, err := uninstallClient.Run(opts.ReleaseName)
	if err != nil {
		return nil, fmt.Errorf("failed to uninstall release %q in namespace %q: %w", opts.ReleaseName, opts.Namespace, err)
	}

	return res, nil
}

func newActionConfig(kubeconfigPath, namespace string) (*action.Configuration, error) {
	kubeConfigFlags := genericclioptions.NewConfigFlags(false)
	kubeConfigFlags.KubeConfig = &kubeconfigPath

	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(kubeConfigFlags, namespace, os.Getenv("HELM_DRIVER"), log.Printf); err != nil {
		return nil, fmt.Errorf("failed to initialize Helm action configuration: %w", err)
	}
	return actionConfig, nil
}

func isRemoteChart(chartPath string) bool {
	isOCI := strings.HasPrefix(chartPath, "oci://")
	isHTTP := strings.HasPrefix(chartPath, "http://")
	isHTTPS := strings.HasPrefix(chartPath, "https://")
	return isOCI || isHTTP || isHTTPS
}

func loadChart(chartPath string) (*chart.Chart, error) {
	if isRemoteChart(chartPath) {
		settings := cli.New()
		providers := getter.All(settings)
		parsedURL, err := url.ParseRequestURI(chartPath)
		if err != nil {
			return nil, fmt.Errorf("invalid chart URL %q: %w", chartPath, err)
		}

		provider, err := providers.ByScheme(parsedURL.Scheme)
		if err != nil {
			return nil, fmt.Errorf("no getter for scheme %q: %w", parsedURL.Scheme, err)
		}

		var getterOpts []getter.Option
		if parsedURL.Scheme == "oci" {
			getterOpts = append(getterOpts, getter.WithURL(chartPath))
		}

		data, err := provider.Get(chartPath, getterOpts...)
		if err != nil {
			return nil, fmt.Errorf("failed to download chart from %q: %w", chartPath, err)
		}

		return loader.LoadArchive(data)
	}

	loadedChart, err := loader.Load(chartPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load chart from path %s: %w", chartPath, err)
	}
	return loadedChart, nil
}
