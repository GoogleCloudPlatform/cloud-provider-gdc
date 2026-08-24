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
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cloud-provider/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	globalnetworkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/networking/v1"
	networkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/networking/v1"
	klog "k8s.io/klog/v2"
)

func (g *lb) getExternalLoadBalancer(ctx context.Context, clusterName string, service *v1.Service) (status *v1.LoadBalancerStatus, exists bool, err error) {
	klog.Infof("getExternalLoadBalancer: %q/%q", service.Namespace, service.Name)
	lbNamespace := g.project
	lbName := g.GetLoadBalancerName(ctx, clusterName, service)

	// fetch forwarding rule
	fwdRuleKey := types.NamespacedName{
		Namespace: lbNamespace,
		Name:      lbName + fwdRuleSuffix,
	}

	extFwdR := &globalnetworkingv1.ForwardingRuleExternal{}
	if err = g.globalMPClient.Get(ctx, fwdRuleKey, extFwdR); errors.IsNotFound(err) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, fmt.Errorf("failed to fetch forwarding rule - LB %q/%q for service %q - %w", lbNamespace, lbName, service.Name, err)
	}

	status = &v1.LoadBalancerStatus{}
	if ip := g.extractVIPFromEFR(extFwdR); ip != "" {
		status.Ingress = []v1.LoadBalancerIngress{{IP: ip}}
	}
	return status, true, nil
}

func (g *lb) ensureExternalLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node, name, namespace string) (*v1.LoadBalancerStatus, error) {
	klog.Infof("ensureExternalLoadBalancer: %q/%q", service.Namespace, service.Name)
	// create or update the LB objects
	if err := g.createOrUpdateExtFwdRule(ctx, name, namespace, service); err != nil {
		msg := fmt.Sprintf("createOrUpdateFwdRule error for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
		return nil, api.NewRetryError(msg, 10*time.Second)
	}

	if err := g.createOrUpdateBackendService(ctx, name, namespace, service); err != nil {
		msg := fmt.Sprintf("createOrUpdateBackendService error for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
		return nil, api.NewRetryError(msg, 10*time.Second)
	}

	if err := g.createOrUpdateBackend(ctx, name, namespace, clusterName); err != nil {
		msg := fmt.Sprintf("createOrUpdateBackend error for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
		return nil, api.NewRetryError(msg, 10*time.Second)
	}

	if err := g.createOrUpdateBackendServicePolicy(ctx, name, namespace); err != nil {
		msg := fmt.Sprintf("ELB service %q error - %v", service.Name, err)
		return nil, api.NewRetryError(msg, 10*time.Second)
	}

	if err := g.createOrUpdateHealthCheck(ctx, name, namespace, service); err != nil {
		msg := fmt.Sprintf("ELB service %q error - %v", service.Name, err)
		return nil, api.NewRetryError(msg, 10*time.Second)
	}

	// create or update PNP for the successfully created LB service
	desiredPNPPorts := servicePortToPnpPorts(service.Spec.Ports)
	if err := g.createOrUpdateOpenAllProjectNetworkPolicy(ctx, name, namespace, clusterName, service, desiredPNPPorts); err != nil {
		msg := fmt.Sprintf("EnsureLoadBalancerLancer: GDC service %q for shoot service %q has pnp error %q", name, service.Name, err)
		return nil, api.NewRetryError(msg, 10*time.Second)
	}

	// Fetch the Forwarding rule to populate the CIDR in loadbalancer status
	extFwdR := &globalnetworkingv1.ForwardingRuleExternal{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + fwdRuleSuffix,
			Namespace: namespace,
		},
	}
	if err := g.globalMPClient.Get(ctx, client.ObjectKeyFromObject(extFwdR), extFwdR); err != nil {
		msg := fmt.Sprintf("extFwdR error for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
		return nil, api.NewRetryError(msg, 10*time.Second)
	}

	if vip := g.extractVIPFromEFR(extFwdR); vip != "" {
		status := &v1.LoadBalancerStatus{
			Ingress: []v1.LoadBalancerIngress{{IP: vip}},
		}
		return status, nil
	}
	msg := fmt.Sprintf("extractVIPFromCIDR error for %q/%q LB for service %q", namespace, name, service.Name)
	return nil, api.NewRetryError(msg, 10*time.Second)
}

func (g *lb) ensureExternalLoadBalancerDeleted(ctx context.Context, clusterName string, service *v1.Service, name, namespace string) error {
	klog.Infof("ensureExternalLoadBalancerDeleted: %q/%q", service.Namespace, service.Name)
	err := g.deleteExtFwdRule(ctx, name, namespace)

	// nothing to delete as the forwarding rule does not exist
	if errors.IsNotFound(err) {
		klog.Infof("ensureExternalLoadBalancerDeleted: Forwarding rule not found for service %q/%q", service.Namespace, service.Name)
		return nil
	} else if err != nil {
		msg := fmt.Sprintf("deleteExtFwdRule error for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
		return api.NewRetryError(msg, 10*time.Second)
	}

	if err = g.deleteGlobalLeafSubnet(ctx, name, namespace); err != nil && !errors.IsNotFound(err) {
		msg := fmt.Sprintf("deleteGlobalLeafSubnet error for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
		return api.NewRetryError(msg, 10*time.Second)
	}

	if err = g.deleteBackendService(ctx, name, namespace); err != nil && !errors.IsNotFound(err) {
		msg := fmt.Sprintf("deleteBackendService error for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
		return api.NewRetryError(msg, 10*time.Second)
	}

	if err = g.deleteBackendServicePolicy(ctx, name, namespace); err != nil && !errors.IsNotFound(err) {
		msg := fmt.Sprintf("ELB service %q error - %v", service.Name, err)
		return api.NewRetryError(msg, 10*time.Second)
	}

	if err = g.deleteBackend(ctx, name, namespace); err != nil && !errors.IsNotFound(err) {
		msg := fmt.Sprintf("deleteBackend error for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
		return api.NewRetryError(msg, 10*time.Second)
	}

	if err = g.deleteHealthCheck(ctx, name, namespace); err != nil && !errors.IsNotFound(err) {
		msg := fmt.Sprintf("ELB service %q error - %v", service.Name, err)
		return api.NewRetryError(msg, 10*time.Second)
	}

	if err = g.deleteProjectNetworkPolicy(ctx, name, namespace); err != nil && !errors.IsNotFound(err) {
		msg := fmt.Sprintf("deleteProjectNetworkPolicy error for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
		return api.NewRetryError(msg, 10*time.Second)
	}

	return nil
}

// Update the LB objects if the Fwd Rule exists
// No updates needed if fwd rule does not exist
func (g *lb) updateExternalLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node, name, namespace string) error {
	klog.Infof("updateExternalLoadBalancer: %q/%q", service.Namespace, service.Name)
	// fetch forwarding rule to see if the service exists
	extFwdRule := &globalnetworkingv1.ForwardingRuleExternal{}
	fwdRuleKey := types.NamespacedName{
		Namespace: namespace,
		Name:      name + fwdRuleSuffix,
	}
	if err := g.globalMPClient.Get(ctx, fwdRuleKey, extFwdRule); errors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}

	// update the LB objects
	if err := g.createOrUpdateExtFwdRule(ctx, name, namespace, service); err != nil {
		return err
	}

	if err := g.createOrUpdateBackendService(ctx, name, namespace, service); err != nil {
		return err
	}

	if err := g.createOrUpdateBackend(ctx, name, namespace, clusterName); err != nil {
		return err
	}

	if err := g.createOrUpdateHealthCheck(ctx, name, namespace, service); err != nil {
		return fmt.Errorf("ELB service %q error - %v", service.Name, err)
	}

	if err := g.createOrUpdateBackendServicePolicy(ctx, name, namespace); err != nil {
		return fmt.Errorf("ELB service %q error - %v", service.Name, err)
	}

	// create or update PNP for the successfully created LB service
	desiredPNPPorts := servicePortToPnpPorts(service.Spec.Ports)
	if err := g.createOrUpdateOpenAllProjectNetworkPolicy(ctx, name, namespace, clusterName, service, desiredPNPPorts); err != nil {
		return err
	}

	return nil
}

func (g *lb) createOrUpdateExtFwdRule(ctx context.Context, name, namespace string, service *v1.Service) error {
	lbLeafSubnetName, err := g.ensureLeafSubnetForService(ctx, name, namespace, service)
	if err != nil {
		return fmt.Errorf("createOrUpdateExtFwdRule error for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
	}

	var desiredCIDRRef *networkingv1.CIDRRef
	if lbLeafSubnetName != "" {
		desiredCIDRRef = &networkingv1.CIDRRef{
			Name: lbLeafSubnetName,
		}
	}

	fwdRuleName := name + fwdRuleSuffix
	existingExtFwdR := &globalnetworkingv1.ForwardingRuleExternal{}
	err = g.globalMPClient.Get(ctx, types.NamespacedName{Name: fwdRuleName, Namespace: namespace}, existingExtFwdR)
	if err != nil {
		if errors.IsNotFound(err) {
			klog.Infof("createOrUpdateExtFwdRule: ForwardingRuleExternal %s/%s not found, creating...", namespace, fwdRuleName)
			return g.ensureExtFwdRule(ctx, name, namespace, service, desiredCIDRRef)
		}
		return fmt.Errorf("createOrUpdateExtFwdRule: failed to get ForwardingRuleExternal for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
	}

	if reflect.DeepEqual(existingExtFwdR.Spec.CIDRRef, desiredCIDRRef) {
		return g.ensureExtFwdRule(ctx, name, namespace, service, desiredCIDRRef)
	}

	klog.Warningf("CIDRRef changed for ForwardingRuleExternal %s/%s. Deleting to trigger recreate.", namespace, fwdRuleName)
	if err := g.globalMPClient.Delete(ctx, existingExtFwdR); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("createOrUpdateExtFwdRule error: failed to delete existing ForwardingRuleExternal for recreate: %w", err)
	}
	// delete the managed Leaf subnet if the new spec either uses no subnet or uses a different subnet
	shouldDeleteManagedSubnet := desiredCIDRRef == nil || desiredCIDRRef.Name != name+subnetSuffix
	if shouldDeleteManagedSubnet {
		if err = g.deleteGlobalLeafSubnet(ctx, name, namespace); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("deleteGlobalLeafSubnet error on CIDRRef change for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
		}
	}
	return fmt.Errorf("createOrUpdateExtFwdRule retryable error: recreating ForwardingRuleExternal due to CIDRRef change")
}

// g.createOrUpdateOpenAllProjectNetworkPolicy creates an ingress "OpenAll" policy to allow ingress from any projects and all CIDR blocks
// even outside the org
func (g *lb) createOrUpdateOpenAllProjectNetworkPolicy(ctx context.Context, name, projectNs, clusterName string, service *v1.Service, desiredPNPPorts []networkingv1.ProjectNetworkPolicyPort) error {
	updatedPNP := &globalnetworkingv1.ProjectNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: projectNs,
		},
	}

	mutateFn := func() error {
		if updatedPNP.Labels == nil {
			updatedPNP.Labels = make(map[string]string)
		}
		updatedPNP.Labels[pnpLabelServiceUID] = string(service.UID)

		updatedPNP.Spec = networkingv1.ProjectNetworkPolicySpec{
			Subject: networkingv1.ProjectNetworkPolicySubject{
				SubjectType: networkingv1.PolicySubjectTypeUserWorkload,
				UserWorkloadSelector: &networkingv1.WorkloadSelector{
					LabelSelector: &networkingv1.WorkloadLabelSelector{
						Workloads: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								lbSelectorLabel: clusterName,
							},
						},
					},
				},
			},
			PolicyType: networkingv1.PolicyTypeIngress,
			Ingress: []networkingv1.ProjectNetworkPolicyIngressRule{
				{
					Ports: desiredPNPPorts,
				},
			},
		}
		return nil
	}

	if _, err := controllerutil.CreateOrUpdate(ctx, g.globalMPClient, updatedPNP, mutateFn); err != nil {
		return fmt.Errorf("createOrUpdateOpenAllProjectNetworkPolicy %s: %w", client.ObjectKeyFromObject(updatedPNP), err)
	}

	return nil
}

func (g *lb) deleteExtFwdRule(ctx context.Context, name, namespace string) error {
	extFwdR := &globalnetworkingv1.ForwardingRuleExternal{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + fwdRuleSuffix,
			Namespace: namespace,
		},
	}
	return g.globalMPClient.Delete(ctx, extFwdR)
}

func (g *lb) extractVIPFromEFR(extFwdR *globalnetworkingv1.ForwardingRuleExternal) string {
	isZoneReady := func(zone globalnetworkingv1.ForwardingRuleExternalZoneStatus) bool {
		for _, condition := range zone.ReplicaStatus.Conditions {
			if condition.Type == "Ready" && condition.Status == metav1.ConditionTrue {
				return true
			}
		}
		return false
	}

	zones := extFwdR.Status.Zones
	if len(zones) == 0 {
		klog.Errorf("len(zones)=0, extFwdR.Status.Zones=%v", extFwdR.Status.Zones)
		return ""
	}

	// Extract VIP from first non empty CIDR zone as per active zones defined in shoot
	for _, zone := range zones {
		if _, ok := g.zonalClients[zone.Name]; ok && zone.ReplicaStatus.CIDR != "" && isZoneReady(zone) {
			return extractVIPFromCIDR(zone.ReplicaStatus.CIDR)
		}
	}
	klog.Errorf("No ready zone with CIDR, extFwdR.Status.Zones=%v", extFwdR.Status.Zones)
	return ""
}

func (g *lb) ensureExtFwdRule(ctx context.Context, name, namespace string, service *v1.Service, desiredCIDRRef *networkingv1.CIDRRef) error {
	extFwdR := &globalnetworkingv1.ForwardingRuleExternal{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + fwdRuleSuffix,
			Namespace: namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, g.globalMPClient, extFwdR, func() error {
		extFwdR.Spec = networkingv1.ForwardingRuleExternalSpec{
			ForwardingRuleSpecCommon: networkingv1.ForwardingRuleSpecCommon{
				BackendServiceRef: &networkingv1.BackendServiceRef{
					Name: name + backendServiceRefSuffix,
				},
				CIDRRef: desiredCIDRRef,
				Ports:   configureFRPorts(service.Spec.Ports),
			},
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("ensureExtFwdRule error: failed to create or update ForwardingRuleExternal: %w", err)
	}

	return nil
}
