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
	"net"

	v1 "k8s.io/api/core/v1"
)

const (
	// Annotations
	lbType                               = "networking.gke.io/load-balancer-type"
	internalLBSubnetAnnotationKey        = "networking.gke.io/load-balancer-subnet"
	externalLBIPAddressesAnnotationKey   = "networking.gke.io/load-balancer-ip-addresses"
	internalLBAllowProjectsAnnotationKey = "networking.gke.io/load-balancer-allow-projects"

	// Annotation Values
	lbInternal = "internal"
	lbExternal = "external"

	// Resource Name Suffixes
	fwdRuleSuffix              = "-fr"
	backendSuffix              = "-be"
	backendServicePolicySuffix = "-bep"
	backendServiceRefSuffix    = "-bes"
	healthCheckSuffix          = "-hc"
	subnetSuffix               = "-sb"

	// Labels and Prefixes for PNP
	pnpLabelServiceUID        = "networking.gke.io/load-balancer-service-uid"
	ilbPnpLabelAllowedProject = "networking.gke.io/load-balancer-allowed-project"
	ilbPnpNamePrefix          = "ilb"
)

// GetLoadBalancerName returns the name of the load balancer.
//
// regex used for validation is '[a-z]([-a-z0-9]*[a-z0-9])?'
func (g *lb) GetLoadBalancerName(ctx context.Context, clusterName string, service *v1.Service) string {
	return "a" + string(service.UID)
}

// GetLoadBalancer returns whether the specified load balancer exists, and
// if so, what its status is.
func (g *lb) GetLoadBalancer(ctx context.Context, clusterName string, service *v1.Service) (status *v1.LoadBalancerStatus, exists bool, err error) {
	svcType, err := extractServiceType(service)
	if err != nil {
		return nil, false, err
	}

	switch svcType {
	case lbInternal:
		return g.getInternalLoadBalancer(ctx, clusterName, service)
	default:
		return g.getExternalLoadBalancer(ctx, clusterName, service)
	}
}

// EnsureLoadBalancer creates a new load balancer 'name', or updates the existing one. Returns the status of the balancer.
func (g *lb) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	lbName := g.GetLoadBalancerName(ctx, clusterName, service)
	lbNamespace := g.project

	desiredType, err := extractServiceType(service)
	if err != nil {
		return nil, fmt.Errorf("extractServiceType %q/%q for service %q:%w", lbNamespace, lbName, service.Name, err)
	}

	if err := validateServicePorts(service); err != nil {
		return nil, fmt.Errorf("validateServicePorts %q/%q for service %q:%w", lbNamespace, lbName, service.Name, err)
	}

	// Delete the LB of the other type if it exists and create the LB of the desiredType if it does not exist
	switch desiredType {
	case lbInternal:
		err = g.ensureExternalLoadBalancerDeleted(ctx, clusterName, service, lbName, lbNamespace)
		if err != nil {
			return nil, fmt.Errorf("error deleting LB %q/%q for service %q - %w", lbName, lbNamespace, service.Name, err)
		}
		return g.ensureInternalLoadBalancer(ctx, clusterName, service, nodes, lbName, lbNamespace)
	default:
		err = g.ensureInternalLoadBalancerDeleted(ctx, clusterName, service, lbName, lbNamespace)
		if err != nil {
			return nil, fmt.Errorf("error deleting LB %q/%q for service %q - %w", lbName, lbNamespace, service.Name, err)
		}
		return g.ensureExternalLoadBalancer(ctx, clusterName, service, nodes, lbName, lbNamespace)
	}
}

// UpdateLoadBalancer updates hosts under the specified load balancer.
func (g *lb) UpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) error {
	lbNamespace := g.project
	lbName := g.GetLoadBalancerName(ctx, clusterName, service)

	svcType, err := extractServiceType(service)
	if err != nil {
		return fmt.Errorf("extractServiceType %q/%q for service %q:%w", lbNamespace, lbName, service.Name, err)
	}

	if err := validateServicePorts(service); err != nil {
		return fmt.Errorf("validateServicePorts %q/%q for service %q:%w", lbNamespace, lbName, service.Name, err)
	}

	switch svcType {
	case lbInternal:
		err = g.updateInternalLoadBalancer(ctx, clusterName, service, nodes, lbName, lbNamespace)
		if err != nil {
			return fmt.Errorf("LB %q/%q for service %q has error %w", lbNamespace, lbName, service.Name, err)
		}
	default:
		err = g.updateExternalLoadBalancer(ctx, clusterName, service, nodes, lbName, lbNamespace)
		if err != nil {
			return fmt.Errorf("LB %q/%q for service %q has error %w", lbNamespace, lbName, service.Name, err)
		}
	}
	return nil
}

// EnsureLoadBalancerDeleted deletes the specified load balancer if it
// exists, returning nil if the load balancer specified either didn't exist or
// was successfully deleted.
func (g *lb) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *v1.Service) error {
	lbNamespace := g.project
	lbName := g.GetLoadBalancerName(ctx, clusterName, service)

	svcType, err := extractServiceType(service)
	if err != nil {
		return err
	}

	switch svcType {
	case lbInternal:
		err = g.ensureInternalLoadBalancerDeleted(ctx, clusterName, service, lbName, lbNamespace)
	default:
		err = g.ensureExternalLoadBalancerDeleted(ctx, clusterName, service, lbName, lbNamespace)
	}
	if err != nil {
		return fmt.Errorf("EnsureLoadBalancerDeleted error for %q/%q LB for service %q - %w", lbNamespace, lbName, service.Name, err)
	}
	return nil
}

func extractServiceType(svc *v1.Service) (string, error) {
	internalVal, internalOk := svc.Annotations[internalLBSubnetAnnotationKey]
	externalVal, externalOk := svc.Annotations[externalLBIPAddressesAnnotationKey]
	svcType := svc.Annotations[lbType]

	switch svcType {
	case lbExternal, "":
		if internalOk {
			return "", fmt.Errorf("external LB service %q/%q must not have internal subnet annotation %q",
				svc.Namespace, svc.Name, internalLBSubnetAnnotationKey)
		}
		if externalOk && net.ParseIP(externalVal) != nil {
			return "", fmt.Errorf("external LB service %q/%q annotation %q value must be a subnet name, not an IP address: got %q",
				svc.Namespace, svc.Name, externalLBIPAddressesAnnotationKey, externalVal)
		}
		return lbExternal, nil

	case lbInternal:
		if externalOk {
			return "", fmt.Errorf("internal LB service %q/%q must not have external IP annotation %q",
				svc.Namespace, svc.Name, externalLBIPAddressesAnnotationKey)
		}

		if internalOk && net.ParseIP(internalVal) != nil {
			return "", fmt.Errorf("internal LB service %q/%q annotation %q value must be a subnet name, not an IP address: got %q",
				svc.Namespace, svc.Name, internalLBSubnetAnnotationKey, internalVal)
		}
		return lbInternal, nil

	default:
		return "", fmt.Errorf("LBType %q not supported. Use values %s or %s for service %q/%q",
			svcType, lbInternal, lbExternal, svc.Namespace, svc.Name)
	}
}

func validateServicePorts(service *v1.Service) error {
	// Services with multiples protocols are not supported by this controller, warn the users and sets
	// the corresponding Service Status Condition.
	// https://github.com/kubernetes/enhancements/tree/master/keps/sig-network/1435-mixed-protocol-lb
	// check for mixed protocols of ports
	ports := service.Spec.Ports
	if len(ports) == 0 {
		return nil
	}
	firstProtocol := ports[0].Protocol
	for _, port := range ports[1:] {
		if port.Protocol != firstProtocol {
			return fmt.Errorf("mixed protocol is not supported for LoadBalancer")
		}
	}

	// checks if the Service has the LoadBalancerPortsError set to True
	for _, cond := range service.Status.Conditions {
		if cond.Type == v1.LoadBalancerPortsError {
			return fmt.Errorf("PortError %v", cond)
		}
	}
	return nil
}
