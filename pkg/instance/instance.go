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

package instance

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/hashicorp/go-multierror"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vmv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/virtualmachine/v1"
	klog "k8s.io/klog/v2"
)

func New(c *Config) (*instanceImpl, error) {
	// providerID must be of the format of
	// $PROVIDER_NAME://$PROJECT/$ZONE_NAME/$VM_NAME.
	providerIDExp := `^` + c.ProviderName + `://([^/]+)/([^/]+)/([^/]+)$`
	providerIDReg, err := regexp.Compile(providerIDExp)
	if err != nil {
		return nil, fmt.Errorf("compile regular expression %q: %v", providerIDExp, err)
	}

	// Zone should be of format
	// ${region-name}-${ix}
	zoneRegionExp := `^([^-]+-[^-]+)-[^-]+$`
	zoneRegionReg, err := regexp.Compile(zoneRegionExp)
	if err != nil {
		return nil, fmt.Errorf("compile regular expression %q: %v", zoneRegionExp, err)
	}

	return &instanceImpl{
		project:         c.Project,
		providerIDRegex: providerIDReg,
		zoneRegionRegex: zoneRegionReg,
		providerName:    c.ProviderName,
		zonalClients:    c.ZonalClients,
	}, nil
}

type Config struct {
	Project      string
	ProviderName string
	ZonalClients map[string]client.Client
}

type instanceImpl struct {
	project      string
	providerName string

	providerIDRegex *regexp.Regexp
	zoneRegionRegex *regexp.Regexp
	zonalClients    map[string]client.Client
}

// NodeAddresses returns the addresses of the specified instance.
func (i *instanceImpl) NodeAddresses(ctx context.Context, name types.NodeName) ([]v1.NodeAddress, error) {
	providerID, err := i.getNodeProviderID(ctx, name)
	if err != nil {
		return nil, err
	}
	nodeAddresses, err := i.NodeAddressesByProviderID(ctx, providerID)
	if err != nil {
		return nil, err
	}
	return nodeAddresses, nil
}

// NodeAddressesByProviderID returns the addresses of the specified instance.
// The instance is specified using the providerID of the node. The
// ProviderID is a unique identifier of the node. This will not be called
// from the node whose nodeaddresses are being queried. i.e. local metadata
// services cannot be used in this method to obtain nodeaddresses
func (i *instanceImpl) NodeAddressesByProviderID(ctx context.Context, providerID string) ([]v1.NodeAddress, error) {
	nodeAddresses := []v1.NodeAddress{}

	vm, err := i.vmByProviderID(ctx, providerID)
	if err != nil {
		return nil, err
	}

	// using the 1st nic as user cluster will have only 1 interface
	if len(vm.Status.Network.Interfaces) == 0 {
		return nodeAddresses, fmt.Errorf("IP cant be determined from vm.Status.Network.Interfaces - %v", vm.Status.Network.Interfaces)
	}
	for _, ipAddress := range vm.Status.Network.Interfaces[0].IpAddresses {
		ip := strings.Split(ipAddress, "/")[0]
		if ip == "" {
			klog.Errorf("VM IP address %q has ip - %q", vm.Status.Network.Interfaces[0].IpAddresses[0], ip)
			continue
		}
		_, err := netip.ParseAddr(ip)
		if err != nil {
			klog.Errorf("IP address %q has err - %q", ip, err)
			continue
		}
		nodeAddresses = append(nodeAddresses, v1.NodeAddress{Type: v1.NodeInternalIP, Address: ip})
	}

	if len(nodeAddresses) == 0 {
		return nodeAddresses, fmt.Errorf("len(nodeAddresses)=0, vm.Status.Network.Interfaces=%v", vm.Status.Network.Interfaces)
	}

	return nodeAddresses, nil
}

// InstanceID returns the cloud provider ID of the node with the specified NodeName.
// Note that if the instance does not exist, we must return ("", cloudprovider.InstanceNotFound)
// cloudprovider.InstanceNotFound should NOT be returned for instances that exist but are stopped/sleeping
// InstanceID must be of the format of
// $PROJECT/$VM_NAME.
func (i *instanceImpl) InstanceID(ctx context.Context, nodeName types.NodeName) (string, error) {
	nn := types.NamespacedName{
		Name:      string(nodeName),
		Namespace: i.project,
	}

	var errs error
	for zone, k8sClient := range i.zonalClients {
		if err := k8sClient.Get(ctx, nn, &vmv1.VirtualMachine{}); err == nil {
			return fmt.Sprintf("%s/%s/%s", i.project, zone, string(nodeName)), nil
		} else if !k8serrors.IsNotFound(err) {
			errs = multierror.Append(errs, fmt.Errorf("get vm.VirtualMachine %q/%q from zone %q failed: %w", i.project, nodeName, zone, err))
		}
	}
	if errs != nil {
		return "", errs
	}
	return "", cloudprovider.InstanceNotFound
}

// InstanceType returns the type of the specified instance.
func (i *instanceImpl) InstanceType(ctx context.Context, name types.NodeName) (string, error) {
	providerID, err := i.getNodeProviderID(ctx, name)
	if err != nil {
		return "", err
	}
	return i.InstanceTypeByProviderID(ctx, providerID)
}

// vmByProjectNodeName returns the VirtualMachine object for the given project and nodeName.
func (i *instanceImpl) vmByProjectNodeName(ctx context.Context, project, nodeName, zone string) (*vmv1.VirtualMachine, error) {
	if zone == "" {
		return nil, fmt.Errorf("zone cannot be empty to retrieve vmByProjectNodeName")
	}

	zoneClient, ok := i.zonalClients[zone]
	if !ok || zoneClient == nil {
		return nil, fmt.Errorf("zonal client for zone %q not found", zone)
	}
	vm := vmv1.VirtualMachine{}
	err := zoneClient.Get(ctx, types.NamespacedName{Namespace: project, Name: nodeName}, &vm)
	if err != nil {
		return nil, err
	}

	return &vm, nil
}

// vmByProviderID returns the VirtualMachine object for the given providerID.
func (i *instanceImpl) vmByProviderID(ctx context.Context, providerID string) (*vmv1.VirtualMachine, error) {
	project, zone, nodeName, err := i.splitProviderID(providerID)
	if err != nil {
		return nil, err
	}

	vm, err := i.vmByProjectNodeName(ctx, project, nodeName, zone)
	if err != nil {
		return nil, err
	}
	return vm, nil
}

// InstanceTypeByProviderID returns the type of the specified instance.
func (i *instanceImpl) InstanceTypeByProviderID(ctx context.Context, providerID string) (string, error) {
	vm, err := i.vmByProviderID(ctx, providerID)
	if err != nil {
		return "", err
	}
	return vm.Spec.Compute.VirtualMachineType, nil
}

// AddSSHKeyToAllInstances adds an SSH public key as a legal identity for all instances
// expected format for the key is standard ssh-keygen format: <protocol> <blob>
// This interface method is not supported in GDC.
func (i *instanceImpl) AddSSHKeyToAllInstances(ctx context.Context, user string, keyData []byte) error {
	return cloudprovider.NotImplemented
}

// CurrentNodeName returns the name of the node we are currently running on.
// On most clouds this is the hostname, so we provide the hostname.
// In GDC it will look like `vm-<UUID>`.
func (i *instanceImpl) CurrentNodeName(ctx context.Context, hostname string) (types.NodeName, error) {
	return types.NodeName(hostname), nil
}

// InstanceExistsByProviderID returns true if the instance for the given provider exists.
// If false is returned with no error, the instance will be immediately deleted by the cloud controller manager.
// This method should still return true for instances that exist but are stopped/sleeping.
func (i *instanceImpl) InstanceExistsByProviderID(ctx context.Context, providerID string) (bool, error) {
	if _, err := i.vmByProviderID(ctx, providerID); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return true, nil
}

// InstanceShutdownByProviderID returns true if the instance is shutdown in cloudprovider
func (i *instanceImpl) InstanceShutdownByProviderID(ctx context.Context, providerID string) (bool, error) {
	vm, err := i.vmByProviderID(ctx, providerID)
	if err != nil {
		return false, err
	}
	return vm.Status.State == vmv1.VirtualMachineStateStopped, nil
}

// splitProviderID splits a provider's id into core components.
// A providerID is build out of '${ProviderName}://${project-id}/${zone}/${instance-name}'
func (i *instanceImpl) splitProviderID(providerID string) (project, zone, instance string, err error) {
	matches := i.providerIDRegex.FindStringSubmatch(providerID)
	if len(matches) != 4 {
		return "", "", "", fmt.Errorf("misformatted providerID: %q", providerID)
	}
	return matches[1], matches[2], matches[3], nil
}

// providerID must be of the format of
// $PROVIDER_NAME://$PROJECT/$VM_NAME.
func (i *instanceImpl) getNodeProviderID(ctx context.Context, node types.NodeName) (string, error) {
	instanceID, err := i.InstanceID(ctx, node)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s://%s", i.providerName, instanceID), nil
}

// InstanceExists returns true if the instance for the given node exists according to the cloud provider.
// Use the node.name or node.spec.providerID field to find the node in the cloud provider.
func (i *instanceImpl) InstanceExists(ctx context.Context, node *v1.Node) (bool, error) {
	providerId, err := i.getProviderID(ctx, node)
	if errors.Is(err, cloudprovider.InstanceNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return i.InstanceExistsByProviderID(ctx, providerId)
}

// InstanceShutdown returns true if the instance is shutdown according to the cloud provider.
// Use the node.name or node.spec.providerID field to find the node in the cloud provider.
func (i *instanceImpl) InstanceShutdown(ctx context.Context, node *v1.Node) (bool, error) {
	providerId, err := i.getProviderID(ctx, node)
	if err != nil {
		return false, err
	}
	vm, err := i.vmByProviderID(ctx, providerId)
	if err != nil {
		return false, err
	}
	return vm.Status.State == vmv1.VirtualMachineStateStopped, nil
}

func (i *instanceImpl) getProviderID(ctx context.Context, node *v1.Node) (string, error) {
	providerId := node.Spec.ProviderID
	if providerId == "" {
		// When the `node.Spec.ProviderID` is empty (likely on node initialization),
		// call `i.InstanceID()` to fetch the providerID.
		// `i.InstanceID()` loops though all managed zones to look up
		// the VM and constructs the providerID.
		klog.Warningf("getProviderID: ProviderID was empty for node %q", node.Name)
		return i.getNodeProviderID(ctx, types.NodeName(node.Name))
	}
	return providerId, nil
}

// InstanceMetadata returns the instance's metadata. The values returned in InstanceMetadata are
// translated into specific fields and labels in the Node object on registration.
// Implementations should always check node.spec.providerID first when trying to discover the instance
// for a given node. In cases where node.spec.providerID is empty, implementations can use other
// properties of the node like its name, labels and annotations.
func (i *instanceImpl) InstanceMetadata(ctx context.Context, node *v1.Node) (*cloudprovider.InstanceMetadata, error) {
	providerId, err := i.getProviderID(ctx, node)
	if err != nil {
		return nil, err
	}

	_, zone, _, err := i.splitProviderID(providerId)
	if err != nil {
		return nil, err
	}

	region, err := i.extractRegionName(zone)
	if err != nil {
		return nil, err
	}

	nodeAddresses, err := i.NodeAddressesByProviderID(ctx, providerId)
	if err != nil {
		return nil, err
	}

	instanceType, err := i.InstanceTypeByProviderID(ctx, providerId)
	if err != nil {
		return nil, err
	}

	return &cloudprovider.InstanceMetadata{
		ProviderID:    providerId,
		Zone:          zone,
		NodeAddresses: nodeAddresses,
		Region:        region,
		InstanceType:  instanceType,
	}, nil
}

// extractRegionName splits a zone's name into core components.
// A zone's name is build out of '${region-name}-${ix}'
func (i *instanceImpl) extractRegionName(zone string) (string, error) {
	matches := i.zoneRegionRegex.FindStringSubmatch(zone)
	if len(matches) != 2 {
		return "", fmt.Errorf("invalid zone format: %s", zone)
	}
	return matches[1], nil
}
