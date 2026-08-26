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

// Package gdc contains the cloud provider specific implementations to manage gdc cloud
// Implements cloudprovider interface:
// https://github.com/kubernetes/cloud-provider/blob/release-1.21/cloud.go#L42-L69
package gdc

import (
	"fmt"
	"io"

	"github.com/hashicorp/go-multierror"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	cloudprovider "k8s.io/cloud-provider"
	"sigs.k8s.io/controller-runtime/pkg/client"

	klog "k8s.io/klog/v2"

	ipamglobalv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/ipam/v1"
	globalnetworkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/networking/v1"
	networkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/networking/v1"
	vmv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/virtualmachine/v1"

	gdcclient "github.com/googlecloudplatform/cloud-provider-gdc/gdc/pkg/client"
	"github.com/googlecloudplatform/cloud-provider-gdc/pkg/instance"
)

const (
	ProviderName      = "gdch"
	ServiceAccountEnv = "GDCH_ORG_ADMIN_SERVICE_ACCOUNT"

	// WorkloadLabelSelectorKey is the label selector key for shoot cluster worker machines and workloads.
	lbSelectorLabel  = "provider.extensions.gardener.gdc.goog/cluster"
	besSelectorLabel = "networking.gke.io/bes"
)

type cloud struct {
	providerName string
	config       *configFile
	lb           cloudprovider.LoadBalancer

	instances   cloudprovider.Instances
	instancesv2 cloudprovider.InstancesV2
}

type lb struct {
	project        string
	globalMPClient client.Client
	zonalClients   map[string]client.Client
}

func init() {
	cloudprovider.RegisterCloudProvider(ProviderName, func(config io.Reader) (cloudprovider.Interface, error) {
		klog.Info("Register CloudProvider: gdch")
		return newGDCCloud(config)
	})
}

func newGDCCloud(config io.Reader) (cloudprovider.Interface, error) {
	cloudProviderConfig, err := readCloudProviderConfig(config)
	if err != nil {
		return nil, err
	}

	c := &cloud{
		providerName: ProviderName,
		config:       cloudProviderConfig,
	}

	return c, nil
}

// This interface method is not supported.
func (c *cloud) Clusters() (cloudprovider.Clusters, bool) {
	return nil, false
}

func (c *cloud) HasClusterID() bool {
	return false
}

func (c *cloud) getGlobalManagementPlaneClient() (client.Client, error) {
	gdcConfig := &gdcclient.OrgClusterConfig{
		OrgClusterURL: c.config.GlobalManagementServerURL,
		CAData:        c.config.CaData,
	}

	serviceAccount, err := getServiceAccount(ServiceAccountEnv)
	if err != nil {
		return nil, fmt.Errorf("failed to get service account: %w", err)
	}

	scheme, err := globalManagementPlaneScheme()
	if err != nil {
		return nil, err
	}

	kubeClient, err := gdcclient.Get(gdcConfig, serviceAccount, scheme)
	if err != nil {
		klog.Errorf("Failed to get the management plane client - %s", err.Error())
		return nil, err
	}

	return kubeClient, nil
}

func (c *cloud) getZonalClients() (map[string]client.Client, error) {
	zonalClients := make(map[string]client.Client)
	serviceAccount, err := getServiceAccount(ServiceAccountEnv)
	if err != nil {
		return zonalClients, fmt.Errorf("failed to get service account: %w", err)
	}

	scheme, err := zonalManagementPlaneScheme()
	if err != nil {
		return zonalClients, err
	}

	var errs error
	for _, zoneEndpoint := range c.config.Zones {
		gdcConfig := &gdcclient.OrgClusterConfig{
			OrgClusterURL: zoneEndpoint.ManagementAPI,
			CAData:        c.config.CaData,
		}
		kubeClient, err := gdcclient.Get(gdcConfig, serviceAccount, scheme)

		if err != nil {
			errs = multierror.Append(errs, err)
			klog.Errorf("Failed to get the zonal client for zone %s: %v", zoneEndpoint.Name, err)
		} else {
			zonalClients[zoneEndpoint.Name] = kubeClient
		}
	}
	return zonalClients, errs
}

// Initialize provides the cloud with a kubernetes client.
// It is not thread safe.
func (c *cloud) Initialize(clientBuilder cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {
	project := c.config.Project
	globalMPClient, err := c.getGlobalManagementPlaneClient()
	if err != nil {
		panic(fmt.Errorf("failed to create mpClient %w", err))
	}

	zonalClients, err := c.getZonalClients()
	if err != nil {
		panic(fmt.Errorf("failed to get zonal clients %w", err))
	}

	c.lb = &lb{
		project:        project,
		globalMPClient: globalMPClient,
		zonalClients:   zonalClients,
	}
	i, err := instance.New(&instance.Config{
		ProviderName: c.providerName,
		Project:      project,
		ZonalClients: zonalClients,
	})
	if err != nil {
		panic(fmt.Errorf("create instance: %v", err))
	}
	c.instances = i
	c.instancesv2 = i
}

func (c *cloud) Instances() (cloudprovider.Instances, bool) {
	return c.instances, true
}

// InstancesV2 returns an implementation of InstancesV2 for GDC.
func (c *cloud) InstancesV2() (cloudprovider.InstancesV2, bool) {
	if c.instancesv2 == nil {
		return nil, false
	}
	return c.instancesv2, true
}

func (c *cloud) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	return c.lb, true
}

// This interface method is not implemented.
func (c *cloud) Routes() (cloudprovider.Routes, bool) {
	return nil, false
}

// This interface method is not supported in GDC.
func (c *cloud) Zones() (cloudprovider.Zones, bool) {
	return nil, false
}

func (c *cloud) ProviderName() string {
	return ProviderName
}

func zonalManagementPlaneScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	// for UNET Zonal API
	if err := networkingv1.AddToScheme(scheme); err != nil {
		return scheme, err
	}
	// for Service
	if err := v1.AddToScheme(scheme); err != nil {
		return scheme, err
	}
	// for VM
	if err := vmv1.AddToScheme(scheme); err != nil {
		return scheme, err
	}
	return scheme, nil
}

func globalManagementPlaneScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	// for UNET Global API
	if err := globalnetworkingv1.AddToScheme(scheme); err != nil {
		return scheme, err
	}
	// for IPAM Global API
	if err := ipamglobalv1.AddToScheme(scheme); err != nil {
		return scheme, fmt.Errorf("failed to add GDC IPAM APIs to the scheme, %w", err)
	}
	return scheme, nil
}
