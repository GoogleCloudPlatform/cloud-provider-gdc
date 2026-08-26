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
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cloud-provider/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ipamglobalv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/ipam/v1"
	globalnetworkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/networking/v1"
	networkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/networking/v1"
	klog "k8s.io/klog/v2"
)

func (g *lb) getInternalLoadBalancer(ctx context.Context, clusterName string, service *v1.Service) (status *v1.LoadBalancerStatus, exists bool, err error) {
	klog.Infof("getInternalLoadBalancer: %q/%q", service.Namespace, service.Name)
	lbNamespace := g.project
	lbName := g.GetLoadBalancerName(ctx, clusterName, service)

	// fetch forwarding rule
	fwdRuleKey := types.NamespacedName{
		Namespace: lbNamespace,
		Name:      lbName + fwdRuleSuffix,
	}

	intFwdR := &globalnetworkingv1.ForwardingRuleInternal{}
	if err = g.globalMPClient.Get(ctx, fwdRuleKey, intFwdR); errors.IsNotFound(err) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, fmt.Errorf("failed to fetch forwarding rule - LB %q/%q for service %q - %w", lbNamespace, lbName, service.Name, err)
	}

	status = &v1.LoadBalancerStatus{}
	if ip := g.extractVIPFromIFR(intFwdR); ip != "" {
		status.Ingress = []v1.LoadBalancerIngress{{IP: ip}}
	}
	return status, true, nil
}

func (g *lb) ensureInternalLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node, name, namespace string) (*v1.LoadBalancerStatus, error) {
	klog.Infof("ensureInternalLoadBalancer: %q/%q", service.Namespace, service.Name)

	if err := g.createOrUpdateIntFwdRule(ctx, name, namespace, service); err != nil {
		msg := fmt.Sprintf("createOrUpdateFwdRule error for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
		return nil, api.NewRetryError(msg, 10*time.Second)
	}

	if err := g.createOrUpdateBackendService(ctx, name, namespace, service); err != nil {
		msg := fmt.Sprintf("createOrUpdateBackendService error for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
		return nil, api.NewRetryError(msg, 10*time.Second)
	}

	if err := g.createOrUpdateBackendServicePolicy(ctx, name, namespace); err != nil {
		msg := fmt.Sprintf("ILB service %q error - %v", service.Name, err)
		return nil, api.NewRetryError(msg, 10*time.Second)
	}

	if err := g.createOrUpdateBackend(ctx, name, namespace, clusterName); err != nil {
		msg := fmt.Sprintf("createOrUpdateBackend error for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
		return nil, api.NewRetryError(msg, 10*time.Second)
	}

	if err := g.createOrUpdateHealthCheck(ctx, name, namespace, service); err != nil {
		msg := fmt.Sprintf("ILB service %q error - %v", service.Name, err)
		return nil, api.NewRetryError(msg, 10*time.Second)
	}

	// create or update PNPs for the successfully created LB service
	if err := g.createOrUpdateProjectNetworkPolicies(ctx, name, namespace, clusterName, service); err != nil {
		msg := fmt.Sprintf("EnsureLoadBalancer: GDC service %q for shoot service %q has pnp error %q", name, service.Name, err)
		return nil, api.NewRetryError(msg, 10*time.Second)
	}

	// Fetch the Forwarding rule to populate the CIDR in loadbalancer status
	intFwdR := &globalnetworkingv1.ForwardingRuleInternal{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + fwdRuleSuffix,
			Namespace: namespace,
		},
	}
	if err := g.globalMPClient.Get(ctx, client.ObjectKeyFromObject(intFwdR), intFwdR); err != nil {
		msg := fmt.Sprintf("intFwdR error for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
		return nil, api.NewRetryError(msg, 10*time.Second)
	}

	if vip := g.extractVIPFromIFR(intFwdR); vip != "" {
		status := &v1.LoadBalancerStatus{
			Ingress: []v1.LoadBalancerIngress{{IP: vip}},
		}
		return status, nil
	}
	msg := fmt.Sprintf("extractVIPFromCIDR error for %q/%q LB for service %q", namespace, name, service.Name)
	return nil, api.NewRetryError(msg, 10*time.Second)
}

func (g *lb) ensureInternalLoadBalancerDeleted(ctx context.Context, clusterName string, service *v1.Service, name, namespace string) error {
	klog.Infof("ensureInternalLoadBalancerDeleted: %q/%q", service.Namespace, service.Name)
	err := g.deleteIntFwdRule(ctx, name, namespace)

	// nothing to delete as the forwarding rule does not exist
	if errors.IsNotFound(err) {
		klog.Infof("EnsureInternalLoadBalancerDeleted: Forwarding rule not found for service %q/%q", service.Namespace, service.Name)
		return nil
	} else if err != nil {
		msg := fmt.Sprintf("deleteIntFwdRule error for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
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
		msg := fmt.Sprintf("ILB service %q error - %v", service.Name, err)
		return api.NewRetryError(msg, 10*time.Second)
	}

	if err = g.deleteBackend(ctx, name, namespace); err != nil && !errors.IsNotFound(err) {
		msg := fmt.Sprintf("deleteBackend error for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
		return api.NewRetryError(msg, 10*time.Second)
	}

	if err = g.deleteHealthCheck(ctx, name, namespace); err != nil && !errors.IsNotFound(err) {
		msg := fmt.Sprintf("ILB service %q error - %v", service.Name, err)
		return api.NewRetryError(msg, 10*time.Second)
	}

	if pnpNamesToDelete, err := g.listProjectNetworkPolicies(ctx, namespace, string(service.UID), name); err == nil {
		if err = g.deleteProjectNetworkPolicies(ctx, namespace, pnpNamesToDelete); err != nil && !errors.IsNotFound(err) {
			msg := fmt.Sprintf("deleteProjectNetworkPolicies error for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
			return api.NewRetryError(msg, 10*time.Second)
		}
	} else if !errors.IsNotFound(err) {
		msg := fmt.Sprintf("listProjectNetworkPolicies error for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
		return api.NewRetryError(msg, 10*time.Second)
	}

	return nil
}

// Update the LB objects if the Fwd Rule exists
// No updates needed if fwd rule does not exist
func (g *lb) updateInternalLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node, name, namespace string) error {
	klog.Infof("updateInternalLoadBalancer: %q/%q", service.Namespace, service.Name)
	// fetch forwarding rule to see if the service exists
	intFwdRule := &globalnetworkingv1.ForwardingRuleInternal{}
	fwdRuleKey := types.NamespacedName{
		Namespace: namespace,
		Name:      name + fwdRuleSuffix,
	}
	if err := g.globalMPClient.Get(ctx, fwdRuleKey, intFwdRule); errors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}

	// update the LB objects
	if err := g.createOrUpdateIntFwdRule(ctx, name, namespace, service); err != nil {
		return err
	}

	if err := g.createOrUpdateBackendService(ctx, name, namespace, service); err != nil {
		return err
	}

	if err := g.createOrUpdateBackendServicePolicy(ctx, name, namespace); err != nil {
		return fmt.Errorf("ILB service %q error - %v", service.Name, err)
	}

	if err := g.createOrUpdateBackend(ctx, name, namespace, clusterName); err != nil {
		return err
	}

	if err := g.createOrUpdateHealthCheck(ctx, name, namespace, service); err != nil {
		return fmt.Errorf("ILB service %q error - %v", service.Name, err)
	}

	// create or update PNPs for the successfully created LB service
	if err := g.createOrUpdateProjectNetworkPolicies(ctx, name, namespace, clusterName, service); err != nil {
		return err
	}

	return nil
}

func (g *lb) createOrUpdateIntFwdRule(ctx context.Context, name, namespace string, service *v1.Service) error {
	lbLeafSubnetName, err := g.ensureLeafSubnetForService(ctx, name, namespace, service)
	if err != nil {
		return fmt.Errorf("createOrUpdateIntFwdRule error for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
	}

	var desiredCIDRRef *networkingv1.CIDRRef
	if lbLeafSubnetName != "" {
		desiredCIDRRef = &networkingv1.CIDRRef{
			Name: lbLeafSubnetName,
		}
	}

	fwdRuleName := name + fwdRuleSuffix
	existingIntFwdR := &globalnetworkingv1.ForwardingRuleInternal{}
	err = g.globalMPClient.Get(ctx, types.NamespacedName{Name: fwdRuleName, Namespace: namespace}, existingIntFwdR)
	if err != nil {
		if errors.IsNotFound(err) {
			klog.Infof("createOrUpdateIntFwdRule: ForwardingRuleInternal %s/%s not found, creating...", namespace, fwdRuleName)
			return g.ensureIntFwdRule(ctx, name, namespace, service, desiredCIDRRef)
		}
		return fmt.Errorf("createOrUpdateIntFwdRule: failed to get ForwardingRuleInternal for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
	}

	if reflect.DeepEqual(existingIntFwdR.Spec.CIDRRef, desiredCIDRRef) {
		return g.ensureIntFwdRule(ctx, name, namespace, service, desiredCIDRRef)
	}

	klog.Warningf("CIDRRef changed for ForwardingRuleInternal %s/%s. Deleting to trigger recreate.", namespace, fwdRuleName)
	if err := g.globalMPClient.Delete(ctx, existingIntFwdR); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("createOrUpdateIntFwdRule error: failed to delete existing ForwardingRuleInternal for recreate: %w", err)
	}
	// delete the managed Leaf subnet if the new spec either uses no subnet or uses a different subnet
	shouldDeleteManagedSubnet := desiredCIDRRef == nil || desiredCIDRRef.Name != name+subnetSuffix
	if shouldDeleteManagedSubnet {
		if err = g.deleteGlobalLeafSubnet(ctx, name, namespace); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("deleteGlobalLeafSubnet error on CIDRRef change for %q/%q LB for service %q - %v", namespace, name, service.Name, err)
		}
	}
	return fmt.Errorf("createOrUpdateIntFwdRule retryable error: recreating ForwardingRuleInternal due to CIDRRef change")
}

// createOrUpdateProjectNetworkPolicies creates or updates the ProjectNetworkPolicies for the service.
// It also deletes any obsolete ProjectNetworkPolicies that are no longer desired.
func (g *lb) createOrUpdateProjectNetworkPolicies(ctx context.Context, lbName, projectNs, clusterName string, service *v1.Service) error {
	allowProjectsVal := service.Annotations[internalLBAllowProjectsAnnotationKey]
	var projects []string
	// Determine the list of projects allowed to access the service.
	if allowProjectsVal == "*" {
		projects = []string{"*"}
	} else {
		projects = g.getAllowedProjects(allowProjectsVal)
	}
	klog.Infof("createOrUpdateProjectNetworkPolicies: desired projects for service %s/%s: %v", service.Namespace, service.Name, projects)

	currentPNPNames := make(map[string]struct{})
	serviceUID := string(service.UID)
	// Ensure all desired ProjectNetworkPolicies exist.
	for _, project := range projects {
		pnpName, err := getProjectNetworkPolicyName(serviceUID, project)
		if err != nil {
			return err
		}
		if err := g.ensureProjectNetworkPolicy(ctx, pnpName, projectNs, clusterName, service, project); err != nil {
			return err
		}
		currentPNPNames[pnpName] = struct{}{}
	}

	// List all existing ProjectNetworkPolicies for the service.
	existingPNPNames, err := g.listProjectNetworkPolicies(ctx, projectNs, serviceUID, lbName)
	if err != nil {
		return fmt.Errorf("list existing PNPs for deletion: %w", err)
	}

	var pnpNamesToDelete []string
	// Identify obsolete ProjectNetworkPolicies for deletion.
	for _, name := range existingPNPNames {
		if _, ok := currentPNPNames[name]; !ok {
			pnpNamesToDelete = append(pnpNamesToDelete, name)
		}
	}

	// Delete obsolete ProjectNetworkPolicies.
	return g.deleteProjectNetworkPolicies(ctx, projectNs, pnpNamesToDelete)
}

// ensureProjectNetworkPolicy ensures that a ProjectNetworkPolicy exists for the given project.
// If project is "*", it creates a wildcard policy allowing all projects.
func (g *lb) ensureProjectNetworkPolicy(ctx context.Context, pnpName, projectNs, clusterName string, service *v1.Service, project string) error {
	serviceUID := string(service.UID)
	desiredPNPPorts := servicePortToPnpPorts(service.Spec.Ports)

	updatedPNP := &globalnetworkingv1.ProjectNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pnpName,
			Namespace: projectNs,
		},
	}

	mutateFn := func() error {
		// Set mandatory labels.
		if updatedPNP.Labels == nil {
			updatedPNP.Labels = make(map[string]string)
		}
		updatedPNP.Labels[pnpLabelServiceUID] = serviceUID

		// Define the PNP spec targeting the cluster workloads.
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
		}

		// Configure the ingress rule.
		ingressRule := networkingv1.ProjectNetworkPolicyIngressRule{
			Ports: desiredPNPPorts,
		}
		// If not a wildcard, restrict access to the specific project and label it.
		if project != "*" {
			updatedPNP.Labels[ilbPnpLabelAllowedProject] = project
			ingressRule.From = []networkingv1.ProjectNetworkPolicyPeer{
				{
					Projects: &networkingv1.PolicyProjects{
						MatchNames: []string{project},
					},
				},
			}
		}
		updatedPNP.Spec.Ingress = []networkingv1.ProjectNetworkPolicyIngressRule{ingressRule}
		return nil
	}

	klog.Infof("ensureProjectNetworkPolicy: creating/updating PNP %s", pnpName)
	if _, err := controllerutil.CreateOrUpdate(ctx, g.globalMPClient, updatedPNP, mutateFn); err != nil {
		return fmt.Errorf("ensureProjectNetworkPolicy %s: %w", client.ObjectKeyFromObject(updatedPNP), err)
	}
	return nil
}

func (g *lb) deleteIntFwdRule(ctx context.Context, name, namespace string) error {
	intFwdR := &globalnetworkingv1.ForwardingRuleInternal{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + fwdRuleSuffix,
			Namespace: namespace,
		},
	}
	return g.globalMPClient.Delete(ctx, intFwdR)
}

func (g *lb) extractVIPFromIFR(intFwdR *globalnetworkingv1.ForwardingRuleInternal) string {
	isZoneReady := func(zone globalnetworkingv1.ForwardingRuleInternalZoneStatus) bool {
		for _, condition := range zone.ReplicaStatus.Conditions {
			if condition.Type == "Ready" && condition.Status == metav1.ConditionTrue {
				return true
			}
		}
		return false
	}

	zones := intFwdR.Status.Zones
	if len(zones) == 0 {
		klog.Errorf("extractVIPFromIFR: len(zones)=0, intFwdR.Status.Zones=%v", intFwdR.Status.Zones)
		return ""
	}
	// Extract VIP from first non empty CIDR zone as per active zones defined in shoot
	for _, zone := range zones {
		if _, ok := g.zonalClients[zone.Name]; ok && zone.ReplicaStatus.CIDR != "" && isZoneReady(zone) {
			return extractVIPFromCIDR(zone.ReplicaStatus.CIDR)
		}
	}
	klog.Errorf("No ready zone with CIDR, intFwdR.Status.Zones=%v", intFwdR.Status.Zones)
	return ""
}

func (g *lb) ensureIntFwdRule(ctx context.Context, name, namespace string, service *v1.Service, desiredCIDRRef *networkingv1.CIDRRef) error {
	intFwdR := &globalnetworkingv1.ForwardingRuleInternal{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + fwdRuleSuffix,
			Namespace: namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, g.globalMPClient, intFwdR, func() error {
		intFwdR.Spec = networkingv1.ForwardingRuleInternalSpec{
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
		return fmt.Errorf("ensureIntFwdRule error: failed to create or update ForwardingRuleInternal: %w", err)
	}

	if desiredCIDRRef == nil || desiredCIDRRef.Name != name+subnetSuffix {
		return nil
	}

	subnetKey := client.ObjectKey{Name: desiredCIDRRef.Name, Namespace: namespace}
	if err := g.updateSubnetOwnerReference(ctx, subnetKey, intFwdR); err != nil {
		return fmt.Errorf("ensureIntFwdRule error: failed to set ForwardingRuleInternal as owner of Subnet: %w", err)
	}
	return nil
}

func (g *lb) updateSubnetOwnerReference(ctx context.Context, subnetKey client.ObjectKey, fwdRObject metav1.Object) error {
	subnet := &ipamglobalv1.Subnet{}
	if err := g.globalMPClient.Get(ctx, subnetKey, subnet); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("subnet %q not found, cannot set owner reference: %w", subnetKey.Name, err)
		}
		return fmt.Errorf("failed to get Subnet %q to update owner reference: %w", subnetKey.Name, err)
	}

	patchBase := client.MergeFrom(subnet.DeepCopy())
	if err := controllerutil.SetOwnerReference(fwdRObject, subnet, g.globalMPClient.Scheme()); err != nil {
		return fmt.Errorf("failed to to set %q as owner of Subnet %q: %w", fwdRObject.GetName(), subnet.GetName(), err)
	}

	if err := g.globalMPClient.Patch(ctx, subnet, patchBase); err != nil {
		return fmt.Errorf("failed to patch Subnet %q with new owner reference: %w", subnetKey.Name, err)
	}
	return nil
}

func (g *lb) deleteProjectNetworkPolicies(ctx context.Context, projectNs string, pnpNamesToDelete []string) error {
	for _, name := range pnpNamesToDelete {
		pnp := &globalnetworkingv1.ProjectNetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: projectNs,
			},
		}
		if err := g.globalMPClient.Delete(ctx, pnp); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete PNP %s: %w", name, err)
		}
	}
	return nil
}

// listProjectNetworkPolicies returns a list of ProjectNetworkPolicy names associated with the service.
// It includes PNPs found by service UID label and the legacy PNP name if it exists.
func (g *lb) listProjectNetworkPolicies(ctx context.Context, projectNs, serviceUID, lbName string) ([]string, error) {
	existingPNPs := &globalnetworkingv1.ProjectNetworkPolicyList{}
	// List PNPs that match the service UID label.
	err := g.globalMPClient.List(ctx, existingPNPs, client.InNamespace(projectNs), client.MatchingLabels{
		pnpLabelServiceUID: serviceUID,
	})
	if err != nil {
		return nil, err
	}

	pnpNames := make([]string, 0, len(existingPNPs.Items))
	for _, item := range existingPNPs.Items {
		pnpNames = append(pnpNames, item.Name)
	}

	// Also check for the legacy PNP name.
	legacyPNP := &globalnetworkingv1.ProjectNetworkPolicy{}
	err = g.globalMPClient.Get(ctx, types.NamespacedName{Namespace: projectNs, Name: lbName}, legacyPNP)
	if err == nil {
		klog.Infof("listProjectNetworkPolicies: found legacy PNP %s", lbName)
		pnpNames = append(pnpNames, legacyPNP.Name)
	} else if !errors.IsNotFound(err) {
		return nil, err
	}
	return pnpNames, nil
}

// getProjectNetworkPolicyName generates a unique name for the ProjectNetworkPolicy.
// It follows the format: ilb-<serviceUID>-<project-name>.
// The name is limited to 200 characters to leave a 53-character buffer for downstream
// systems (like the propagation engine) to add prefixes/suffixes (e.g., zone names)
// without exceeding the Kubernetes 253-character limit.
func getProjectNetworkPolicyName(serviceUID, project string) (string, error) {
	suffix := project
	if project == "*" {
		suffix = "all"
	}

	pnpName := fmt.Sprintf("%s-%s-%s", ilbPnpNamePrefix, serviceUID, suffix)
	if len(pnpName) > 200 {
		return "", fmt.Errorf("generated ProjectNetworkPolicy name %q exceeds 200 characters", pnpName)
	}
	return pnpName, nil
}

func (g *lb) getAllowedProjects(allowProjectsVal string) []string {
	allowedProjectsMap := map[string]struct{}{g.project: {}}
	if allowProjectsVal != "" {
		for _, p := range strings.Split(allowProjectsVal, ",") {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				allowedProjectsMap[trimmed] = struct{}{}
			}
		}
	}

	var allowedProjects []string
	for p := range allowedProjectsMap {
		allowedProjects = append(allowedProjects, p)
	}
	return allowedProjects
}
