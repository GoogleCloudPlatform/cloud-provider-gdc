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
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/hashicorp/go-multierror"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	klog "k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ipamglobalv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/ipam/v1"
	globalnetworkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/networking/v1"
	ipamv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/ipam/v1"
	networkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/networking/v1"
)

const (
	defaultHealthzPort = int32(10256)
	healthzRequestPath = "/healthz"
)

func extractVIPFromCIDR(cidr string) string {
	if len(cidr) == 0 {
		return ""
	}
	return strings.Split(cidr, "/")[0]
}

func configureBSTargetPorts(ports []v1.ServicePort) []networkingv1.TargetPort {
	var targetPorts []networkingv1.TargetPort
	for _, port := range ports {
		targetPort := networkingv1.TargetPort{
			Port: networkingv1.Port{
				Port:     port.Port,
				Protocol: ptr.To(port.Protocol),
			},
			TargetPort: port.NodePort,
		}
		targetPorts = append(targetPorts, targetPort)
	}
	return targetPorts
}

func configureFRPorts(ports []v1.ServicePort) []networkingv1.Port {
	var lbPorts []networkingv1.Port
	for _, port := range ports {
		lbPort := networkingv1.Port{
			Port:     port.Port,
			Protocol: ptr.To(port.Protocol),
		}

		lbPorts = append(lbPorts, lbPort)
	}
	return lbPorts
}

func (g *lb) createOrUpdateBackendService(ctx context.Context, name, namespace string, service *v1.Service) error {
	backendService := &globalnetworkingv1.BackendService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + backendServiceRefSuffix,
			Namespace: namespace,
		},
	}

	backendRef := []globalnetworkingv1.BackendRef{}

	for zone := range g.zonalClients {
		backendRef = append(backendRef, globalnetworkingv1.BackendRef{
			Name: name + backendSuffix,
			Zone: zone,
		})
	}

	_, err := controllerutil.CreateOrUpdate(ctx, g.globalMPClient, backendService, func() error {
		// label needed for session affinity to match against the backend service policy
		backendService.Labels = map[string]string{
			besSelectorLabel: name + backendServiceRefSuffix,
		}
		backendService.Spec = globalnetworkingv1.BackendServiceSpec{
			BackendRefs:     backendRef,
			TargetPorts:     configureBSTargetPorts(service.Spec.Ports),
			HealthCheckName: ptr.To(name + healthCheckSuffix),
		}
		return nil
	})
	return err
}

func (g *lb) createOrUpdateBackendServicePolicy(ctx context.Context, name, namespace string) error {
	backendServicePolicy := globalnetworkingv1.BackendServicePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + backendServicePolicySuffix,
			Namespace: namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, g.globalMPClient, &backendServicePolicy, func() error {
		backendServicePolicy.Spec = networkingv1.BackendServicePolicySpec{
			SessionAffinity: networkingv1.SessionAffinityClientIpDstPortProto,
			Selectors: metav1.LabelSelector{
				MatchLabels: map[string]string{
					besSelectorLabel: name + backendServiceRefSuffix,
				},
			},
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("createOrUpdateBackendServicePolicy error for %q/%q LB - %w", namespace, name, err)
	}
	return nil
}

func (g *lb) createOrUpdateBackend(ctx context.Context, name, namespace, clusterName string) error {
	var errs error
	for _, mpClient := range g.zonalClients {
		backend := &networkingv1.Backend{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + backendSuffix,
				Namespace: namespace,
			},
		}

		_, err := controllerutil.CreateOrUpdate(ctx, mpClient, backend, func() error {
			backend.Spec = networkingv1.BackendSpec{
				EndpointsLabels: metav1.LabelSelector{
					MatchLabels: map[string]string{
						lbSelectorLabel: clusterName,
					},
				},
			}
			return nil
		})
		if err != nil {
			errs = multierror.Append(errs, err)
		}
	}
	return errs
}

func (g *lb) createOrUpdateHealthCheck(ctx context.Context, name, namespace string, service *v1.Service) error {
	healthzPort := defaultHealthzPort
	if service.Spec.ExternalTrafficPolicy == "Local" {
		if service.Spec.HealthCheckNodePort == 0 {
			return fmt.Errorf("HealthCheckNodePort is not yet set for service %q/%q; Requeing", service.Namespace, service.Name)
		} else {
			healthzPort = service.Spec.HealthCheckNodePort
		}
	}

	desiredSpec := networkingv1.HealthCheckSpec{
		ProbeHandler: networkingv1.ProbeHandler{
			HTTPHealthCheck: &networkingv1.HTTPHealthCheck{
				Port:        healthzPort,
				RequestPath: healthzRequestPath,
			},
		},
	}

	hcName := name + healthCheckSuffix
	existingHC := &globalnetworkingv1.HealthCheck{}
	err := g.globalMPClient.Get(ctx, types.NamespacedName{Name: hcName, Namespace: namespace}, existingHC)

	if err != nil {
		if errors.IsNotFound(err) {
			klog.Infof("createOrUpdateHealthCheck: HealthCheck %s/%s not found, creating...", namespace, hcName)
			return g.ensureHealthCheck(ctx, hcName, namespace, desiredSpec)
		}
		return fmt.Errorf("createOrUpdateHealthCheck: failed to get HealthCheck %q/%q: %w", namespace, hcName, err)
	}

	if !reflect.DeepEqual(existingHC.Spec.ProbeHandler, desiredSpec.ProbeHandler) {
		klog.Warningf("ProbeHandler type changed for HealthCheck %s/%s. Deleting to trigger recreate.", namespace, hcName)

		if err := g.globalMPClient.Delete(ctx, existingHC); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("createOrUpdateHealthCheck: failed to delete existing HealthCheck for recreate: %w", err)
		}
		return fmt.Errorf("createOrUpdateHealthCheck: recreating HealthCheck due to ProbeHandler type change")
	}

	return g.ensureHealthCheck(ctx, hcName, namespace, desiredSpec)
}

func (g *lb) ensureLeafSubnetForService(ctx context.Context, name, namespace string, service *v1.Service) (string, error) {
	internalSubnet, internalOk := service.Annotations[internalLBSubnetAnnotationKey]
	externalSubnet, externalOk := service.Annotations[externalLBIPAddressesAnnotationKey]

	if internalOk && externalOk {
		return "", fmt.Errorf("service %s/%s should not have both internal and external LB subnet annotations", service.Namespace, service.Name)
	}

	var lbParentSubnetName string
	if internalOk {
		lbParentSubnetName = internalSubnet
	} else if externalOk {
		lbParentSubnetName = externalSubnet
	} else {
		return "", nil
	}

	// check the type of the parent subnet. If subnet type is `Leaf`, return it. Otherwise create a new child subnet.
	parentSubnet := &ipamglobalv1.Subnet{}
	parentSubnetKey := client.ObjectKey{Name: lbParentSubnetName, Namespace: namespace}
	if err := g.globalMPClient.Get(ctx, parentSubnetKey, parentSubnet); err != nil {
		if errors.IsNotFound(err) {
			return "", fmt.Errorf("parent subnet %q not found in namespace %q: %w", lbParentSubnetName, namespace, err)
		}
		return "", fmt.Errorf("failed to get parent subnet %q: %w", lbParentSubnetName, err)
	}
	if parentSubnet.Spec.Type == ipamv1.Leaf {
		return lbParentSubnetName, nil
	}

	// check if the subnet's parent reference is changing
	leafSubnetName := name + subnetSuffix
	existingLeafSubnet := &ipamglobalv1.Subnet{}
	err := g.globalMPClient.Get(ctx, types.NamespacedName{Name: leafSubnetName, Namespace: namespace}, existingLeafSubnet)
	if err == nil {
		if existingLeafSubnet.Spec.ParentReference != nil && existingLeafSubnet.Spec.ParentReference.Name != lbParentSubnetName {
			klog.Warningf("Parent subnet changed for leaf subnet %s/%s. Deleting to trigger recreate.", namespace, leafSubnetName)
			if err := g.globalMPClient.Delete(ctx, existingLeafSubnet); err != nil && !errors.IsNotFound(err) {
				return "", fmt.Errorf("failed to delete existing leaf subnet for recreate: %w", err)
			}
			return "", fmt.Errorf("recreating leaf subnet due to parent change")
		}
	} else if !errors.IsNotFound(err) {
		return "", fmt.Errorf("failed to get leaf subnet %q: %w", leafSubnetName, err)
	}

	subnet := &ipamglobalv1.Subnet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      leafSubnetName,
			Namespace: namespace,
		},
	}

	_, err = controllerutil.CreateOrUpdate(ctx, g.globalMPClient, subnet, func() error {
		var subnetPrefixLength int32 = 32
		subnet.Spec = ipamglobalv1.SubnetSpec{
			Type: ipamv1.Leaf,
			IPv4Request: &ipamv1.SubnetRequest{
				PrefixLength: ptr.To(subnetPrefixLength),
			},
			ParentReference: &ipamv1.SubnetReference{
				Name:      lbParentSubnetName,
				Namespace: ptr.To(namespace),
			},
		}
		return nil
	})

	if err != nil {
		return "", fmt.Errorf("failed to create or update Global Subnet for LB: %w", err)
	}
	return leafSubnetName, nil
}

func (g *lb) deleteGlobalLeafSubnet(ctx context.Context, name, namespace string) error {
	lbSubnet := &ipamglobalv1.Subnet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + subnetSuffix,
			Namespace: namespace,
		},
	}
	return g.globalMPClient.Delete(ctx, lbSubnet)
}

func (g *lb) deleteBackendService(ctx context.Context, name, namespace string) error {
	backendService := &globalnetworkingv1.BackendService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + backendServiceRefSuffix,
			Namespace: namespace,
		},
	}
	return g.globalMPClient.Delete(ctx, backendService)
}

func (g *lb) deleteBackendServicePolicy(ctx context.Context, name, namespace string) error {
	backendServicePolicy := &globalnetworkingv1.BackendServicePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + backendServicePolicySuffix,
			Namespace: namespace,
		},
	}

	if err := g.globalMPClient.Delete(ctx, backendServicePolicy); err != nil {
		return fmt.Errorf("deleteBackendServicePolicy error for %q/%q LB - %w", namespace, name, err)
	}
	return nil
}

func (g *lb) deleteBackend(ctx context.Context, name, namespace string) error {
	backend := &networkingv1.Backend{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + backendSuffix,
			Namespace: namespace,
		},
	}
	var errs error
	for _, mpClient := range g.zonalClients {
		if err := mpClient.Delete(ctx, backend); err != nil {
			errs = multierror.Append(errs, err)
		}
	}
	return errs
}

func (g *lb) deleteHealthCheck(ctx context.Context, name, namespace string) error {
	healthCheck := &globalnetworkingv1.HealthCheck{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + healthCheckSuffix,
			Namespace: namespace,
		},
	}

	if err := g.globalMPClient.Delete(ctx, healthCheck); err != nil {
		return fmt.Errorf("deleteHealthCheck error for %q/%q LB - %w", namespace, name, err)
	}
	return nil
}

func (g *lb) deleteProjectNetworkPolicy(ctx context.Context, pnpName, projectNs string) error {
	pnp := &globalnetworkingv1.ProjectNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pnpName,
			Namespace: projectNs,
		},
	}

	if err := g.globalMPClient.Delete(ctx, pnp); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("deleteProjectNetworkPolicy %s: %w", client.ObjectKeyFromObject(pnp), err)
	}
	return nil
}

func servicePortToPnpPorts(ports []v1.ServicePort) []networkingv1.ProjectNetworkPolicyPort {
	pnpPorts := make([]networkingv1.ProjectNetworkPolicyPort, 0, len(ports))
	for _, port := range ports {
		pnpPort := networkingv1.ProjectNetworkPolicyPort{
			Port:     &intstr.IntOrString{IntVal: port.NodePort},
			Protocol: &port.Protocol,
		}
		pnpPorts = append(pnpPorts, pnpPort)
	}
	return pnpPorts
}

func (g *lb) ensureHealthCheck(ctx context.Context, hcName, namespace string, desiredSpec networkingv1.HealthCheckSpec) error {
	healthCheck := &globalnetworkingv1.HealthCheck{
		ObjectMeta: metav1.ObjectMeta{
			Name:      hcName,
			Namespace: namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, g.globalMPClient, healthCheck, func() error {
		healthCheck.Spec = desiredSpec
		return nil
	})

	if err != nil {
		return fmt.Errorf("ensureHealthCheck: CreateOrUpdate error for %q/%q: %w", namespace, hcName, err)
	}
	return nil
}
