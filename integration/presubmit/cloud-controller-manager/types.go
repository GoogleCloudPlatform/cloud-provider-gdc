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

package main

import (
	"flag"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// Timeouts and polling intervals
	provisioningTimeout = 8 * time.Minute
	reconcileTimeout    = 5 * time.Minute
	cleanupTimeout      = 5 * time.Minute
	pollInterval        = 2 * time.Second

	// CCM deployment resource names
	ccmAppName            = "gdch-cloud-controller-manager"
	ccmConfigMapName      = "ccm-cloud-provider-config"
	ccmSASecretName       = "ccm-cloudprovider-sa"
	ccmVClusterSecretName = "ccm-vcluster-kubeconfig"

	// Service annotations and labels supported by cloud-provider-gdc
	lbTypeAnnotationKey                  = "networking.gke.io/load-balancer-type"
	internalLBSubnetAnnotationKey        = "networking.gke.io/load-balancer-subnet"
	externalLBIPAddressesAnnotationKey   = "networking.gke.io/load-balancer-ip-addresses"
	internalLBAllowProjectsAnnotationKey = "networking.gke.io/load-balancer-allow-projects"

	lbTypeInternal = "internal"
	lbTypeExternal = "external"

	// GDC UNET resource naming suffixes and metadata keys
	fwdRuleSuffix              = "-fr"
	backendSuffix              = "-be"
	backendServicePolicySuffix = "-bep"
	backendServiceSuffix       = "-bes"
	healthCheckSuffix          = "-hc"
	subnetSuffix               = "-sb"

	besSelectorLabel          = "networking.gke.io/bes"
	clusterSelectorLabel      = "provider.extensions.gardener.gdc.goog/cluster"
	pnpLabelServiceUID        = "networking.gke.io/load-balancer-service-uid"
	ilbPnpLabelAllowedProject = "networking.gke.io/load-balancer-allowed-project"
	forwardingRuleCIDRKey     = "networking.gke.io/forwardingRuleCIDR"
	backendPoliciesAnnotation = "networking.private.gdc.goog/backend-service-policies"

	defaultHealthzPort = int32(10256)
	healthzRequestPath = "/healthz"

	// Node controller taints and labels
	uninitializedNodeTaintKey = "node.cloudprovider.kubernetes.io/uninitialized"

	// Custom subnet prefixes
	elbTestSubnetPrefix = "ccm-elb-test-"
	ilbTestSubnetPrefix = "ccm-ilb-test-"
	branchSubnetSuffix  = "-branch"
	leafSubnetSuffix    = "-leaf"
)

// Config holds the CLI flag configuration for the CCM presubmit integration test.
type Config struct {
	CommitHash     string
	Zone           string
	Region         string
	Project        string
	VUC            string
	Org            string
	LabURL         string
	CAFile         string
	SAFile         string
	CCMImageTag    string
	VClusterChart  string
	VClusterK8sTag string
}

// TestEnv holds the initialized clients and runtime environment for the test suite.
type TestEnv struct {
	HostClient         client.WithWatch
	VClusterClient     client.WithWatch
	GlobalClient       client.WithWatch
	MgmtClient         client.WithWatch
	Scheme             *runtime.Scheme
	HostNamespace      string
	ClusterName        string
	Project            string
	Zone               string
	Region             string
	HostKubeconfig     string
	VClusterKubeconfig string
}

var cfg Config

func init() {
	flag.StringVar(&cfg.CommitHash, "commit_hash", "dummy", "Short commit hash for git repo")
	flag.StringVar(&cfg.Zone, "zone", "", "Zone where VUC is deployed")
	flag.StringVar(&cfg.Region, "region", "", "Region where VUC is deployed")
	flag.StringVar(&cfg.Project, "project", "", "Project where VUC is created")
	flag.StringVar(&cfg.VUC, "vuc", "", "VUC name")
	flag.StringVar(&cfg.Org, "org", "", "Org name for GDC-AG lab")
	flag.StringVar(&cfg.LabURL, "lab_url", "", "Lab URL (e.g., staging.gpcdemolabs.com)")
	flag.StringVar(&cfg.CAFile, "cafile", "", "Path to CA file")
	flag.StringVar(&cfg.SAFile, "service_account", "", "Path to Service Account file")
	flag.StringVar(&cfg.CCMImageTag, "ccm_image_tag", "", "Cloud Controller Manager image tag (image:tag)")
	flag.StringVar(&cfg.VClusterChart, "vcluster_chart", "", "Path to vcluster Helm chart archive")
	flag.StringVar(&cfg.VClusterK8sTag, "vcluster_k8s_tag", "v1.35.0", "Kubernetes version tag for vcluster")
}
