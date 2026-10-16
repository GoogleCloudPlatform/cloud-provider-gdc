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
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	ipamglobalv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/ipam/v1"
	globalnetworkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/networking/v1"
	ipamv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/ipam/v1"
	networkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/networking/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func runLoadBalancerTests(ctx context.Context, t *testing.T, env *TestEnv) {
	t.Run("ExternalLoadBalancerLifecycleAndConversionToInternal", func(t *testing.T) {
		svcName := "ccm-elb-lifecycle-" + cfg.CommitHash
		svc := newLoadBalancerService(svcName, corev1.NamespaceDefault, nil)

		t.Logf("Creating External LoadBalancer Service %s/%s in vcluster", svc.Namespace, svc.Name)
		createLoadBalancerService(ctx, t, env.VClusterClient, svc)
		t.Cleanup(func() {
			deleteServiceAndWait(context.Background(), t, env.VClusterClient, svc.Namespace, svc.Name)
		})

		elbVIP, currentSvc := waitForServiceLoadBalancerIP(ctx, t, env.VClusterClient, svc.Namespace, svc.Name, "")
		lbName := "a" + string(currentSvc.UID)
		t.Logf("External LoadBalancer Service %s assigned VIP %s (lbName=%s)", svc.Name, elbVIP, lbName)

		// 1. Verify all Global & Zonal UNET resources for External LoadBalancer.
		verifyExternalLBResources(ctx, t, env, currentSvc, lbName, elbVIP, nil, defaultHealthzPort)

		// 2. Update Service ports and switch ExternalTrafficPolicy to Local to trigger
		// ForwardingRule, BackendService, ProjectNetworkPolicy, and HealthCheck updates.
		t.Logf("Updating Service %s to add port 8443 and set ExternalTrafficPolicy=Local", svc.Name)
		if err := env.VClusterClient.Get(ctx, client.ObjectKeyFromObject(svc), currentSvc); err != nil {
			t.Fatalf("failed to get Service %s for update: %v", svc.Name, err)
		}
		currentSvc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
		currentSvc.Spec.Ports = append(currentSvc.Spec.Ports, corev1.ServicePort{
			Name:       "https-alt",
			Protocol:   corev1.ProtocolTCP,
			Port:       8443,
			TargetPort: intstr.FromInt32(8443),
		})
		if err := env.VClusterClient.Update(ctx, currentSvc); err != nil {
			t.Fatalf("failed to update Service %s: %v", svc.Name, err)
		}

		if err := env.VClusterClient.Get(ctx, client.ObjectKeyFromObject(svc), currentSvc); err != nil {
			t.Fatalf("failed to re-fetch Service %s after update: %v", svc.Name, err)
		}
		if currentSvc.Spec.HealthCheckNodePort == 0 {
			t.Fatalf("expected HealthCheckNodePort to be allocated on Service %s when ExternalTrafficPolicy=Local", svc.Name)
		}
		t.Logf("Verifying updated ports and HealthCheckNodePort (%d) on Global & Zonal UNET resources...", currentSvc.Spec.HealthCheckNodePort)
		verifyExternalLBResources(ctx, t, env, currentSvc, lbName, elbVIP, nil, currentSvc.Spec.HealthCheckNodePort)

		// 3. Convert External LoadBalancer (ELB) -> Internal LoadBalancer (ILB) in-place.
		t.Logf("Converting Service %s from External LB to Internal LB", svc.Name)
		if err := env.VClusterClient.Get(ctx, client.ObjectKeyFromObject(svc), currentSvc); err != nil {
			t.Fatalf("failed to get Service %s for conversion: %v", svc.Name, err)
		}
		if currentSvc.Annotations == nil {
			currentSvc.Annotations = make(map[string]string)
		}
		currentSvc.Annotations[lbTypeAnnotationKey] = lbTypeInternal
		if err := env.VClusterClient.Update(ctx, currentSvc); err != nil {
			t.Fatalf("failed to update Service %s to Internal LB: %v", svc.Name, err)
		}

		ilbVIP, currentSvc := waitForServiceLoadBalancerIP(ctx, t, env.VClusterClient, svc.Namespace, svc.Name, elbVIP)
		t.Logf("Service %s converted from ELB VIP %s to ILB VIP %s", svc.Name, elbVIP, ilbVIP)

		// Verify ForwardingRuleExternal and open-all PNP are deleted, and ForwardingRuleInternal + project PNP exist.
		waitForObjectDeleted(ctx, t, env.GlobalClient, client.ObjectKey{Namespace: env.Project, Name: lbName + fwdRuleSuffix}, &globalnetworkingv1.ForwardingRuleExternal{})
		waitForObjectDeleted(ctx, t, env.GlobalClient, client.ObjectKey{Namespace: env.Project, Name: lbName}, &globalnetworkingv1.ProjectNetworkPolicy{})
		verifyInternalLBResources(ctx, t, env, currentSvc, lbName, ilbVIP, nil, currentSvc.Spec.HealthCheckNodePort, []string{env.Project}, false)

		// 4. Delete Service and verify all Global & Zonal UNET resources are cleaned up.
		t.Logf("Deleting Service %s and verifying complete Global & Zonal UNET resource cleanup", svc.Name)
		deleteServiceAndWait(ctx, t, env.VClusterClient, svc.Namespace, svc.Name)
		verifyAllLBResourcesDeleted(ctx, t, env, string(currentSvc.UID), lbName)
	})

	t.Run("InternalLoadBalancerAllowProjectsAndConversionToExternal", func(t *testing.T) {
		svcName := "ccm-ilb-lifecycle-" + cfg.CommitHash
		svc := newLoadBalancerService(svcName, corev1.NamespaceDefault, map[string]string{
			lbTypeAnnotationKey: lbTypeInternal,
		})

		t.Logf("Creating Internal LoadBalancer Service %s/%s in vcluster", svc.Namespace, svc.Name)
		createLoadBalancerService(ctx, t, env.VClusterClient, svc)
		t.Cleanup(func() {
			deleteServiceAndWait(context.Background(), t, env.VClusterClient, svc.Namespace, svc.Name)
		})

		ilbVIP, currentSvc := waitForServiceLoadBalancerIP(ctx, t, env.VClusterClient, svc.Namespace, svc.Name, "")
		lbName := "a" + string(currentSvc.UID)
		svcUID := string(currentSvc.UID)
		t.Logf("Internal LoadBalancer Service %s assigned VIP %s (lbName=%s)", svc.Name, ilbVIP, lbName)

		// 1. Verify initial Internal LB resources and default single-project PNP.
		verifyInternalLBResources(ctx, t, env, currentSvc, lbName, ilbVIP, nil, defaultHealthzPort, []string{env.Project}, false)

		// 2. Mutate networking.gke.io/load-balancer-allow-projects to allow an additional project ("sapbtp,platform").
		secondaryProject := "platform"
		t.Logf("Updating Service %s annotation %s=%q", svc.Name, internalLBAllowProjectsAnnotationKey, env.Project+","+secondaryProject)
		if err := env.VClusterClient.Get(ctx, client.ObjectKeyFromObject(svc), currentSvc); err != nil {
			t.Fatalf("failed to get Service %s: %v", svc.Name, err)
		}
		currentSvc.Annotations[internalLBAllowProjectsAnnotationKey] = env.Project + "," + secondaryProject
		if err := env.VClusterClient.Update(ctx, currentSvc); err != nil {
			t.Fatalf("failed to update Service %s allow-projects annotation: %v", svc.Name, err)
		}
		verifyInternalLBResources(ctx, t, env, currentSvc, lbName, ilbVIP, nil, defaultHealthzPort, []string{env.Project, secondaryProject}, false)

		// 3. Mutate networking.gke.io/load-balancer-allow-projects to wildcard "*" and verify per-project PNPs are pruned.
		t.Logf("Updating Service %s annotation %s=\"*\"", svc.Name, internalLBAllowProjectsAnnotationKey)
		if err := env.VClusterClient.Get(ctx, client.ObjectKeyFromObject(svc), currentSvc); err != nil {
			t.Fatalf("failed to get Service %s: %v", svc.Name, err)
		}
		currentSvc.Annotations[internalLBAllowProjectsAnnotationKey] = "*"
		if err := env.VClusterClient.Update(ctx, currentSvc); err != nil {
			t.Fatalf("failed to update Service %s allow-projects to wildcard: %v", svc.Name, err)
		}
		verifyInternalLBResources(ctx, t, env, currentSvc, lbName, ilbVIP, nil, defaultHealthzPort, nil, true)
		waitForObjectDeleted(ctx, t, env.GlobalClient, client.ObjectKey{Namespace: env.Project, Name: fmt.Sprintf("ilb-%s-%s", svcUID, env.Project)}, &globalnetworkingv1.ProjectNetworkPolicy{})
		waitForObjectDeleted(ctx, t, env.GlobalClient, client.ObjectKey{Namespace: env.Project, Name: fmt.Sprintf("ilb-%s-%s", svcUID, secondaryProject)}, &globalnetworkingv1.ProjectNetworkPolicy{})

		// 4. Remove networking.gke.io/load-balancer-allow-projects and verify wildcard PNP is pruned and default project PNP is restored.
		t.Logf("Removing annotation %s from Service %s", internalLBAllowProjectsAnnotationKey, svc.Name)
		if err := env.VClusterClient.Get(ctx, client.ObjectKeyFromObject(svc), currentSvc); err != nil {
			t.Fatalf("failed to get Service %s: %v", svc.Name, err)
		}
		delete(currentSvc.Annotations, internalLBAllowProjectsAnnotationKey)
		if err := env.VClusterClient.Update(ctx, currentSvc); err != nil {
			t.Fatalf("failed to remove allow-projects annotation from Service %s: %v", svc.Name, err)
		}
		verifyInternalLBResources(ctx, t, env, currentSvc, lbName, ilbVIP, nil, defaultHealthzPort, []string{env.Project}, false)
		waitForObjectDeleted(ctx, t, env.GlobalClient, client.ObjectKey{Namespace: env.Project, Name: fmt.Sprintf("ilb-%s-all", svcUID)}, &globalnetworkingv1.ProjectNetworkPolicy{})

		// 5. Convert Internal LoadBalancer (ILB) -> External LoadBalancer (ELB) in-place.
		t.Logf("Converting Service %s from Internal LB to External LB", svc.Name)
		if err := env.VClusterClient.Get(ctx, client.ObjectKeyFromObject(svc), currentSvc); err != nil {
			t.Fatalf("failed to get Service %s for conversion to ELB: %v", svc.Name, err)
		}
		delete(currentSvc.Annotations, lbTypeAnnotationKey)
		if err := env.VClusterClient.Update(ctx, currentSvc); err != nil {
			t.Fatalf("failed to update Service %s to External LB: %v", svc.Name, err)
		}

		elbVIP, currentSvc := waitForServiceLoadBalancerIP(ctx, t, env.VClusterClient, svc.Namespace, svc.Name, ilbVIP)
		t.Logf("Service %s converted from ILB VIP %s to ELB VIP %s", svc.Name, ilbVIP, elbVIP)
		waitForObjectDeleted(ctx, t, env.GlobalClient, client.ObjectKey{Namespace: env.Project, Name: lbName + fwdRuleSuffix}, &globalnetworkingv1.ForwardingRuleInternal{})
		waitForObjectDeleted(ctx, t, env.GlobalClient, client.ObjectKey{Namespace: env.Project, Name: fmt.Sprintf("ilb-%s-%s", svcUID, env.Project)}, &globalnetworkingv1.ProjectNetworkPolicy{})
		verifyExternalLBResources(ctx, t, env, currentSvc, lbName, elbVIP, nil, defaultHealthzPort)

		// 6. Delete Service and verify all Global & Zonal UNET resources are cleaned up.
		deleteServiceAndWait(ctx, t, env.VClusterClient, svc.Namespace, svc.Name)
		verifyAllLBResourcesDeleted(ctx, t, env, svcUID, lbName)
	})

	t.Run("CustomSubnetLoadBalancers", func(t *testing.T) {
		const elbParentBranchName = "gardener-release-pipeline-parent-public-subnet"
		elbLeafName := elbTestSubnetPrefix + cfg.CommitHash + leafSubnetSuffix
		ilbBranchName := ilbTestSubnetPrefix + cfg.CommitHash + branchSubnetSuffix
		ilbLeafName := ilbTestSubnetPrefix + cfg.CommitHash + leafSubnetSuffix

		t.Cleanup(func() {
			bgCtx := context.Background()
			deleteTestSubnet(bgCtx, t, env.GlobalClient, client.ObjectKey{Namespace: env.Project, Name: elbLeafName})
			deleteTestSubnet(bgCtx, t, env.GlobalClient, client.ObjectKey{Namespace: env.Project, Name: ilbLeafName})
			deleteTestSubnet(bgCtx, t, env.GlobalClient, client.ObjectKey{Namespace: env.Project, Name: ilbBranchName})
		})

		// Fetch the existing public Branch Subnet CIDR in the project.
		elbParentBranch := &ipamglobalv1.Subnet{}
		if err := env.GlobalClient.Get(ctx, client.ObjectKey{Namespace: env.Project, Name: elbParentBranchName}, elbParentBranch); err != nil {
			t.Fatalf("failed to get public parent Branch Subnet %s/%s: %v", env.Project, elbParentBranchName, err)
		}
		if elbParentBranch.Status.IPv4Allocation == nil || elbParentBranch.Status.IPv4Allocation.CIDR == "" {
			t.Fatalf("public parent Branch Subnet %s/%s has no IPv4 CIDR allocated", env.Project, elbParentBranchName)
		}
		elbBranchCIDR := elbParentBranch.Status.IPv4Allocation.CIDR

		// Create temporary private Branch (/31) and Leaf (/32) Subnets for Internal LB tests.
		ilbBranchCIDR := ensureTestSubnet(ctx, t, env.GlobalClient, testSubnetSpec{
			Namespace:       env.Project,
			Name:            ilbBranchName,
			SubnetType:      ipamv1.Branch,
			PrefixLength:    31,
			ParentName:      "gardener-release-pipeline-parent-subnet",
			ParentNamespace: env.Project,
			ParentType:      ipamv1.SingleSubnet,
			Labels: map[string]string{
				ipamv1.VPCLabel: "default-vpc",
			},
		})
		ilbLeafCIDR := ensureTestSubnet(ctx, t, env.GlobalClient, testSubnetSpec{
			Namespace:       env.Project,
			Name:            ilbLeafName,
			SubnetType:      ipamv1.Leaf,
			PrefixLength:    32,
			ParentName:      ilbBranchName,
			ParentNamespace: env.Project,
			ParentType:      ipamv1.SingleSubnet,
			Labels: map[string]string{
				ipamv1.VPCLabel: "default-vpc",
			},
		})

		t.Run("ExternalLBWithCustomLeafSubnet", func(t *testing.T) {
			elbLeafCIDR := ensureTestSubnet(ctx, t, env.GlobalClient, testSubnetSpec{
				Namespace:       env.Project,
				Name:            elbLeafName,
				SubnetType:      ipamv1.Leaf,
				PrefixLength:    32,
				ParentName:      elbParentBranchName,
				ParentNamespace: env.Project,
				ParentType:      ipamv1.SingleSubnet,
				Labels: map[string]string{
					"ipam.gdc.goog/network-segment": "data",
				},
			})

			svcName := "ccm-elb-leaf-" + cfg.CommitHash
			svc := newLoadBalancerService(svcName, corev1.NamespaceDefault, map[string]string{
				externalLBIPAddressesAnnotationKey: elbLeafName,
			})
			createLoadBalancerService(ctx, t, env.VClusterClient, svc)
			t.Cleanup(func() {
				deleteServiceAndWait(context.Background(), t, env.VClusterClient, svc.Namespace, svc.Name)
			})

			vip, currentSvc := waitForServiceLoadBalancerIP(ctx, t, env.VClusterClient, svc.Namespace, svc.Name, "")
			lbName := "a" + string(currentSvc.UID)
			assertIPInCIDR(t, vip, elbLeafCIDR)
			verifyExternalLBResources(ctx, t, env, currentSvc, lbName, vip, ptr.To(elbLeafName), defaultHealthzPort)

			// Verify no managed child subnet (<lbName>-sb) was created when a Leaf subnet is supplied directly.
			managedSubnet := &ipamglobalv1.Subnet{}
			err := env.GlobalClient.Get(ctx, client.ObjectKey{Namespace: env.Project, Name: lbName + subnetSuffix}, managedSubnet)
			if !apierrors.IsNotFound(err) {
				t.Fatalf("expected no managed Leaf subnet %s for direct Leaf subnet annotation, got err=%v", lbName+subnetSuffix, err)
			}

			deleteServiceAndWait(ctx, t, env.VClusterClient, svc.Namespace, svc.Name)
			verifyAllLBResourcesDeleted(ctx, t, env, string(currentSvc.UID), lbName)
			deleteTestSubnet(ctx, t, env.GlobalClient, client.ObjectKey{Namespace: env.Project, Name: elbLeafName})
		})

		t.Run("ExternalLBWithCustomBranchSubnet", func(t *testing.T) {
			svcName := "ccm-elb-branch-" + cfg.CommitHash
			svc := newLoadBalancerService(svcName, corev1.NamespaceDefault, map[string]string{
				externalLBIPAddressesAnnotationKey: elbParentBranchName,
			})
			createLoadBalancerService(ctx, t, env.VClusterClient, svc)
			t.Cleanup(func() {
				deleteServiceAndWait(context.Background(), t, env.VClusterClient, svc.Namespace, svc.Name)
			})

			vip, currentSvc := waitForServiceLoadBalancerIP(ctx, t, env.VClusterClient, svc.Namespace, svc.Name, "")
			lbName := "a" + string(currentSvc.UID)
			managedSubnetName := lbName + subnetSuffix
			assertIPInCIDR(t, vip, elbBranchCIDR)
			verifyExternalLBResources(ctx, t, env, currentSvc, lbName, vip, ptr.To(managedSubnetName), defaultHealthzPort)
			verifyManagedChildSubnet(ctx, t, env.GlobalClient, env.Project, managedSubnetName, elbParentBranchName, vip, "")

			deleteServiceAndWait(ctx, t, env.VClusterClient, svc.Namespace, svc.Name)
			verifyAllLBResourcesDeleted(ctx, t, env, string(currentSvc.UID), lbName)
		})

		t.Run("InternalLBWithCustomLeafSubnet", func(t *testing.T) {
			svcName := "ccm-ilb-leaf-" + cfg.CommitHash
			svc := newLoadBalancerService(svcName, corev1.NamespaceDefault, map[string]string{
				lbTypeAnnotationKey:           lbTypeInternal,
				internalLBSubnetAnnotationKey: ilbLeafName,
			})
			createLoadBalancerService(ctx, t, env.VClusterClient, svc)
			t.Cleanup(func() {
				deleteServiceAndWait(context.Background(), t, env.VClusterClient, svc.Namespace, svc.Name)
			})

			vip, currentSvc := waitForServiceLoadBalancerIP(ctx, t, env.VClusterClient, svc.Namespace, svc.Name, "")
			lbName := "a" + string(currentSvc.UID)
			assertIPInCIDR(t, vip, ilbLeafCIDR)
			verifyInternalLBResources(ctx, t, env, currentSvc, lbName, vip, ptr.To(ilbLeafName), defaultHealthzPort, []string{env.Project}, false)

			deleteServiceAndWait(ctx, t, env.VClusterClient, svc.Namespace, svc.Name)
			verifyAllLBResourcesDeleted(ctx, t, env, string(currentSvc.UID), lbName)
		})

		t.Run("InternalLBWithCustomBranchSubnet", func(t *testing.T) {
			svcName := "ccm-ilb-branch-" + cfg.CommitHash
			svc := newLoadBalancerService(svcName, corev1.NamespaceDefault, map[string]string{
				lbTypeAnnotationKey:           lbTypeInternal,
				internalLBSubnetAnnotationKey: ilbBranchName,
			})
			createLoadBalancerService(ctx, t, env.VClusterClient, svc)
			t.Cleanup(func() {
				deleteServiceAndWait(context.Background(), t, env.VClusterClient, svc.Namespace, svc.Name)
			})

			vip, currentSvc := waitForServiceLoadBalancerIP(ctx, t, env.VClusterClient, svc.Namespace, svc.Name, "")
			lbName := "a" + string(currentSvc.UID)
			managedSubnetName := lbName + subnetSuffix
			assertIPInCIDR(t, vip, ilbBranchCIDR)
			verifyInternalLBResources(ctx, t, env, currentSvc, lbName, vip, ptr.To(managedSubnetName), defaultHealthzPort, []string{env.Project}, false)
			// For ILB branch subnets, cloud-provider-gdc also sets an OwnerReference pointing to ForwardingRuleInternal.
			verifyManagedChildSubnet(ctx, t, env.GlobalClient, env.Project, managedSubnetName, ilbBranchName, vip, lbName+fwdRuleSuffix)

			deleteServiceAndWait(ctx, t, env.VClusterClient, svc.Namespace, svc.Name)
			verifyAllLBResourcesDeleted(ctx, t, env, string(currentSvc.UID), lbName)
		})
	})
}

func createLoadBalancerService(ctx context.Context, t *testing.T, vclusterClient client.Client, template *corev1.Service) {
	t.Helper()
	var lastErr error
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		svc := template.DeepCopy()
		if createErr := vclusterClient.Create(ctx, svc); createErr != nil {
			if apierrors.IsAlreadyExists(createErr) {
				return true, nil
			}
			lastErr = createErr
			return false, nil
		}
		return true, nil
	}); err != nil {
		t.Fatalf("failed to create Service %s/%s: %v (poll err: %v)", template.Namespace, template.Name, lastErr, err)
	}
}

func newLoadBalancerService(name, namespace string, annotations map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{
				"app": name,
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Protocol:   corev1.ProtocolTCP,
					Port:       80,
					TargetPort: intstr.FromInt32(8080),
				},
				{
					Name:       "https",
					Protocol:   corev1.ProtocolTCP,
					Port:       443,
					TargetPort: intstr.FromInt32(8443),
				},
			},
		},
	}
}

func waitForServiceLoadBalancerIP(
	ctx context.Context,
	t *testing.T,
	vclusterClient client.Client,
	namespace, name, previousIP string,
) (string, *corev1.Service) {
	t.Helper()

	var allocatedIP string
	latestSvc := &corev1.Service{}
	err := wait.PollUntilContextTimeout(ctx, pollInterval, reconcileTimeout, true, func(ctx context.Context) (bool, error) {
		if err := vclusterClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, latestSvc); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if len(latestSvc.Status.LoadBalancer.Ingress) == 0 {
			return false, nil
		}
		ip := latestSvc.Status.LoadBalancer.Ingress[0].IP
		if ip == "" || ip == previousIP {
			return false, nil
		}
		allocatedIP = ip
		return true, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for LoadBalancer IP on Service %s/%s (previousIP=%q): %v", namespace, name, previousIP, err)
	}
	return allocatedIP, latestSvc
}

func verifyExternalLBResources(
	ctx context.Context,
	t *testing.T,
	env *TestEnv,
	svc *corev1.Service,
	lbName, expectedVIP string,
	expectedCIDRRefName *string,
	expectedHealthCheckPort int32,
) {
	t.Helper()
	expectedCIDR := expectedVIP + "/32"

	// 1. Verify Global ForwardingRuleExternal (<lbName>-fr) and its dynamically allocated or custom Subnet.
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, reconcileTimeout, true, func(ctx context.Context) (bool, error) {
		globalEFR := &globalnetworkingv1.ForwardingRuleExternal{}
		if err := env.GlobalClient.Get(ctx, client.ObjectKey{Namespace: env.Project, Name: lbName + fwdRuleSuffix}, globalEFR); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if globalEFR.Spec.BackendServiceRef == nil || globalEFR.Spec.BackendServiceRef.Name != lbName+backendServiceSuffix {
			return false, nil
		}
		if expectedCIDRRefName == nil && globalEFR.Spec.CIDRRef != nil {
			return false, nil
		}
		if expectedCIDRRefName != nil && (globalEFR.Spec.CIDRRef == nil || globalEFR.Spec.CIDRRef.Name != *expectedCIDRRefName) {
			return false, nil
		}
		if len(globalEFR.Spec.Ports) != len(svc.Spec.Ports) {
			return false, nil
		}
		if globalEFR.Annotations[forwardingRuleCIDRKey] != expectedCIDR {
			return false, nil
		}
		zoneReady := false
		for _, z := range globalEFR.Status.Zones {
			if z.Name == env.Zone && z.ReplicaStatus.CIDR == expectedCIDR && meta.IsStatusConditionTrue(z.ReplicaStatus.Conditions, "Ready") {
				zoneReady = true
				break
			}
		}
		return zoneReady, nil
	}); err != nil {
		t.Fatalf("Global ForwardingRuleExternal %s/%s verification failed: %v", env.Project, lbName+fwdRuleSuffix, err)
	}

	// 2. Verify Zonal replicated ForwardingRuleExternal (<lbName>-fr) on Management API server.
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, reconcileTimeout, true, func(ctx context.Context) (bool, error) {
		zonalEFR := &networkingv1.ForwardingRuleExternal{}
		if err := env.MgmtClient.Get(ctx, client.ObjectKey{Namespace: env.Project, Name: lbName + fwdRuleSuffix}, zonalEFR); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return zonalEFR.Status.CIDR == expectedCIDR && meta.IsStatusConditionTrue(zonalEFR.Status.Conditions, "Ready"), nil
	}); err != nil {
		t.Fatalf("Zonal ForwardingRuleExternal %s/%s verification failed: %v", env.Project, lbName+fwdRuleSuffix, err)
	}

	// 3. Verify shared Global & Zonal BackendService, BackendServicePolicy, Backend, and HealthCheck.
	verifyCommonLBBackendAndHealthResources(ctx, t, env, svc, lbName, expectedHealthCheckPort)

	// 4. Verify Global & Zonal Open-All ProjectNetworkPolicy (<lbName>).
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, reconcileTimeout, true, func(ctx context.Context) (bool, error) {
		globalPNP := &globalnetworkingv1.ProjectNetworkPolicy{}
		if err := env.GlobalClient.Get(ctx, client.ObjectKey{Namespace: env.Project, Name: lbName}, globalPNP); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if globalPNP.Labels[pnpLabelServiceUID] != string(svc.UID) {
			return false, nil
		}
		if globalPNP.Spec.Subject.UserWorkloadSelector == nil ||
			globalPNP.Spec.Subject.UserWorkloadSelector.LabelSelector == nil ||
			globalPNP.Spec.Subject.UserWorkloadSelector.LabelSelector.Workloads == nil ||
			globalPNP.Spec.Subject.UserWorkloadSelector.LabelSelector.Workloads.MatchLabels[clusterSelectorLabel] != env.ClusterName {
			return false, nil
		}
		if len(globalPNP.Spec.Ingress) != 1 || len(globalPNP.Spec.Ingress[0].From) != 0 || len(globalPNP.Spec.Ingress[0].Ports) != len(svc.Spec.Ports) {
			return false, nil
		}
		zonalPNP := &networkingv1.ProjectNetworkPolicy{}
		if err := env.MgmtClient.Get(ctx, client.ObjectKey{Namespace: env.Project, Name: lbName}, zonalPNP); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return meta.IsStatusConditionTrue(zonalPNP.Status.Conditions, "Ready"), nil
	}); err != nil {
		t.Fatalf("Global/Zonal Open-All ProjectNetworkPolicy %s/%s verification failed: %v", env.Project, lbName, err)
	}
}

func verifyInternalLBResources(
	ctx context.Context,
	t *testing.T,
	env *TestEnv,
	svc *corev1.Service,
	lbName, expectedVIP string,
	expectedCIDRRefName *string,
	expectedHealthCheckPort int32,
	expectedAllowedProjects []string,
	expectWildcardPNP bool,
) {
	t.Helper()
	expectedCIDR := expectedVIP + "/32"

	// 1. Verify Global ForwardingRuleInternal (<lbName>-fr).
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, reconcileTimeout, true, func(ctx context.Context) (bool, error) {
		globalIFR := &globalnetworkingv1.ForwardingRuleInternal{}
		if err := env.GlobalClient.Get(ctx, client.ObjectKey{Namespace: env.Project, Name: lbName + fwdRuleSuffix}, globalIFR); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if globalIFR.Spec.BackendServiceRef == nil || globalIFR.Spec.BackendServiceRef.Name != lbName+backendServiceSuffix {
			return false, nil
		}
		if expectedCIDRRefName == nil && globalIFR.Spec.CIDRRef != nil {
			return false, nil
		}
		if expectedCIDRRefName != nil && (globalIFR.Spec.CIDRRef == nil || globalIFR.Spec.CIDRRef.Name != *expectedCIDRRefName) {
			return false, nil
		}
		if len(globalIFR.Spec.Ports) != len(svc.Spec.Ports) {
			return false, nil
		}
		if globalIFR.Annotations[forwardingRuleCIDRKey] != expectedCIDR {
			return false, nil
		}
		zoneReady := false
		for _, z := range globalIFR.Status.Zones {
			if z.Name == env.Zone && z.ReplicaStatus.CIDR == expectedCIDR && meta.IsStatusConditionTrue(z.ReplicaStatus.Conditions, "Ready") {
				zoneReady = true
				break
			}
		}
		return zoneReady, nil
	}); err != nil {
		t.Fatalf("Global ForwardingRuleInternal %s/%s verification failed: %v", env.Project, lbName+fwdRuleSuffix, err)
	}

	// 2. Verify Zonal replicated ForwardingRuleInternal (<lbName>-fr) on Management API server.
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, reconcileTimeout, true, func(ctx context.Context) (bool, error) {
		zonalIFR := &networkingv1.ForwardingRuleInternal{}
		if err := env.MgmtClient.Get(ctx, client.ObjectKey{Namespace: env.Project, Name: lbName + fwdRuleSuffix}, zonalIFR); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return zonalIFR.Status.CIDR == expectedCIDR && meta.IsStatusConditionTrue(zonalIFR.Status.Conditions, "Ready"), nil
	}); err != nil {
		t.Fatalf("Zonal ForwardingRuleInternal %s/%s verification failed: %v", env.Project, lbName+fwdRuleSuffix, err)
	}

	// 3. Verify shared Global & Zonal BackendService, BackendServicePolicy, Backend, and HealthCheck.
	verifyCommonLBBackendAndHealthResources(ctx, t, env, svc, lbName, expectedHealthCheckPort)

	// 4. Verify ILB ProjectNetworkPolicies (ilb-<UID>-<project> or ilb-<UID>-all).
	svcUID := string(svc.UID)
	if expectWildcardPNP {
		pnpName := fmt.Sprintf("ilb-%s-all", svcUID)
		if err := wait.PollUntilContextTimeout(ctx, pollInterval, reconcileTimeout, true, func(ctx context.Context) (bool, error) {
			pnp := &globalnetworkingv1.ProjectNetworkPolicy{}
			if err := env.GlobalClient.Get(ctx, client.ObjectKey{Namespace: env.Project, Name: pnpName}, pnp); err != nil {
				return false, client.IgnoreNotFound(err)
			}
			if pnp.Labels[pnpLabelServiceUID] != svcUID {
				return false, nil
			}
			return len(pnp.Spec.Ingress) == 1 && len(pnp.Spec.Ingress[0].From) == 0 && len(pnp.Spec.Ingress[0].Ports) == len(svc.Spec.Ports), nil
		}); err != nil {
			t.Fatalf("Wildcard ILB ProjectNetworkPolicy %s/%s verification failed: %v", env.Project, pnpName, err)
		}
		return
	}

	for _, allowedProj := range expectedAllowedProjects {
		pnpName := fmt.Sprintf("ilb-%s-%s", svcUID, allowedProj)
		if err := wait.PollUntilContextTimeout(ctx, pollInterval, reconcileTimeout, true, func(ctx context.Context) (bool, error) {
			pnp := &globalnetworkingv1.ProjectNetworkPolicy{}
			if err := env.GlobalClient.Get(ctx, client.ObjectKey{Namespace: env.Project, Name: pnpName}, pnp); err != nil {
				return false, client.IgnoreNotFound(err)
			}
			if pnp.Labels[pnpLabelServiceUID] != svcUID || pnp.Labels[ilbPnpLabelAllowedProject] != allowedProj {
				return false, nil
			}
			if len(pnp.Spec.Ingress) != 1 || len(pnp.Spec.Ingress[0].From) != 1 || pnp.Spec.Ingress[0].From[0].Projects == nil {
				return false, nil
			}
			matchNames := pnp.Spec.Ingress[0].From[0].Projects.MatchNames
			return len(matchNames) == 1 && matchNames[0] == allowedProj && len(pnp.Spec.Ingress[0].Ports) == len(svc.Spec.Ports), nil
		}); err != nil {
			t.Fatalf("Project-scoped ILB ProjectNetworkPolicy %s/%s verification failed: %v", env.Project, pnpName, err)
		}
	}
}

func verifyCommonLBBackendAndHealthResources(
	ctx context.Context,
	t *testing.T,
	env *TestEnv,
	svc *corev1.Service,
	lbName string,
	expectedHealthCheckPort int32,
) {
	t.Helper()

	besName := lbName + backendServiceSuffix
	bepName := lbName + backendServicePolicySuffix
	beName := lbName + backendSuffix
	hcName := lbName + healthCheckSuffix

	// 1. Verify Global & Zonal BackendService (<lbName>-bes).
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, reconcileTimeout, true, func(ctx context.Context) (bool, error) {
		globalBS := &globalnetworkingv1.BackendService{}
		if err := env.GlobalClient.Get(ctx, client.ObjectKey{Namespace: env.Project, Name: besName}, globalBS); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if globalBS.Labels[besSelectorLabel] != besName {
			return false, nil
		}
		if globalBS.Spec.HealthCheckName == nil || *globalBS.Spec.HealthCheckName != hcName {
			return false, nil
		}
		if len(globalBS.Spec.TargetPorts) != len(svc.Spec.Ports) {
			return false, nil
		}

		zonalBS := &networkingv1.BackendService{}
		if err := env.MgmtClient.Get(ctx, client.ObjectKey{Namespace: env.Project, Name: besName}, zonalBS); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if zonalBS.Status.BackendServicePolicyRef == nil || *zonalBS.Status.BackendServicePolicyRef != bepName {
			return false, nil
		}
		return meta.IsStatusConditionTrue(zonalBS.Status.Conditions, "Ready"), nil
	}); err != nil {
		t.Fatalf("Global/Zonal BackendService %s/%s verification failed: %v", env.Project, besName, err)
	}

	// 2. Verify Global & Zonal BackendServicePolicy (<lbName>-bep).
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, reconcileTimeout, true, func(ctx context.Context) (bool, error) {
		globalBSP := &globalnetworkingv1.BackendServicePolicy{}
		if err := env.GlobalClient.Get(ctx, client.ObjectKey{Namespace: env.Project, Name: bepName}, globalBSP); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if globalBSP.Spec.SessionAffinity != networkingv1.SessionAffinityClientIpDstPortProto {
			return false, nil
		}
		if globalBSP.Spec.Selectors.MatchLabels[besSelectorLabel] != besName {
			return false, nil
		}

		zonalBSP := &networkingv1.BackendServicePolicy{}
		if err := env.MgmtClient.Get(ctx, client.ObjectKey{Namespace: env.Project, Name: bepName}, zonalBSP); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return zonalBSP.Spec.SessionAffinity == networkingv1.SessionAffinityClientIpDstPortProto, nil
	}); err != nil {
		t.Fatalf("Global/Zonal BackendServicePolicy %s/%s verification failed: %v", env.Project, bepName, err)
	}

	// 3. Verify Zonal Backend (<lbName>-be).
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, reconcileTimeout, true, func(ctx context.Context) (bool, error) {
		zonalBE := &networkingv1.Backend{}
		if err := env.MgmtClient.Get(ctx, client.ObjectKey{Namespace: env.Project, Name: beName}, zonalBE); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if zonalBE.Spec.EndpointsLabels.MatchLabels[clusterSelectorLabel] != env.ClusterName {
			return false, nil
		}
		if !strings.Contains(zonalBE.Annotations[backendPoliciesAnnotation], bepName) {
			return false, nil
		}
		return meta.IsStatusConditionTrue(zonalBE.Status.Conditions, "Ready"), nil
	}); err != nil {
		t.Fatalf("Zonal Backend %s/%s verification failed: %v", env.Project, beName, err)
	}

	// 4. Verify Global & Zonal HealthCheck (<lbName>-hc).
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, reconcileTimeout, true, func(ctx context.Context) (bool, error) {
		globalHC := &globalnetworkingv1.HealthCheck{}
		if err := env.GlobalClient.Get(ctx, client.ObjectKey{Namespace: env.Project, Name: hcName}, globalHC); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if globalHC.Spec.HTTPHealthCheck == nil ||
			globalHC.Spec.HTTPHealthCheck.Port != expectedHealthCheckPort ||
			globalHC.Spec.HTTPHealthCheck.RequestPath != healthzRequestPath {
			return false, nil
		}

		zonalHC := &networkingv1.HealthCheck{}
		if err := env.MgmtClient.Get(ctx, client.ObjectKey{Namespace: env.Project, Name: hcName}, zonalHC); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return zonalHC.Spec.HTTPHealthCheck != nil &&
			zonalHC.Spec.HTTPHealthCheck.Port == expectedHealthCheckPort &&
			meta.IsStatusConditionTrue(zonalHC.Status.Conditions, "Ready"), nil
	}); err != nil {
		t.Fatalf("Global/Zonal HealthCheck %s/%s verification failed (expected port %d): %v", env.Project, hcName, expectedHealthCheckPort, err)
	}
}

func verifyManagedChildSubnet(
	ctx context.Context,
	t *testing.T,
	globalClient client.Client,
	namespace, managedSubnetName, expectedParentName, expectedVIP, expectedOwnerFRName string,
) {
	t.Helper()

	s := &ipamglobalv1.Subnet{}
	if err := globalClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: managedSubnetName}, s); err != nil {
		t.Fatalf("failed to get managed Leaf Subnet %s/%s: %v", namespace, managedSubnetName, err)
	}
	if s.Spec.Type != ipamv1.Leaf {
		t.Fatalf("expected managed Subnet %s/%s to have type Leaf, got %s", namespace, managedSubnetName, s.Spec.Type)
	}
	if s.Spec.ParentReference == nil || s.Spec.ParentReference.Name != expectedParentName {
		t.Fatalf("expected managed Subnet %s/%s parent %q, got %+v", namespace, managedSubnetName, expectedParentName, s.Spec.ParentReference)
	}
	if s.Status.IPv4Allocation == nil || s.Status.IPv4Allocation.CIDR != expectedVIP+"/32" {
		t.Fatalf("expected managed Subnet %s/%s CIDR %q, got %+v", namespace, managedSubnetName, expectedVIP+"/32", s.Status.IPv4Allocation)
	}
	if expectedOwnerFRName != "" {
		hasOwner := false
		for _, ref := range s.OwnerReferences {
			if ref.Name == expectedOwnerFRName {
				hasOwner = true
				break
			}
		}
		if !hasOwner {
			t.Fatalf("expected managed Subnet %s/%s to have OwnerReference to %q, got %+v", namespace, managedSubnetName, expectedOwnerFRName, s.OwnerReferences)
		}
	}
}

func verifyAllLBResourcesDeleted(ctx context.Context, t *testing.T, env *TestEnv, svcUID, lbName string) {
	t.Helper()

	waitForObjectDeleted(ctx, t, env.GlobalClient, client.ObjectKey{Namespace: env.Project, Name: lbName + fwdRuleSuffix}, &globalnetworkingv1.ForwardingRuleExternal{})
	waitForObjectDeleted(ctx, t, env.GlobalClient, client.ObjectKey{Namespace: env.Project, Name: lbName + fwdRuleSuffix}, &globalnetworkingv1.ForwardingRuleInternal{})
	waitForObjectDeleted(ctx, t, env.GlobalClient, client.ObjectKey{Namespace: env.Project, Name: lbName + subnetSuffix}, &ipamglobalv1.Subnet{})
	waitForObjectDeleted(ctx, t, env.GlobalClient, client.ObjectKey{Namespace: env.Project, Name: lbName + backendServiceSuffix}, &globalnetworkingv1.BackendService{})
	waitForObjectDeleted(ctx, t, env.GlobalClient, client.ObjectKey{Namespace: env.Project, Name: lbName + backendServicePolicySuffix}, &globalnetworkingv1.BackendServicePolicy{})
	waitForObjectDeleted(ctx, t, env.GlobalClient, client.ObjectKey{Namespace: env.Project, Name: lbName + healthCheckSuffix}, &globalnetworkingv1.HealthCheck{})
	waitForObjectDeleted(ctx, t, env.GlobalClient, client.ObjectKey{Namespace: env.Project, Name: lbName}, &globalnetworkingv1.ProjectNetworkPolicy{})
	waitForObjectDeleted(ctx, t, env.MgmtClient, client.ObjectKey{Namespace: env.Project, Name: lbName + backendSuffix}, &networkingv1.Backend{})

	if err := wait.PollUntilContextTimeout(ctx, pollInterval, cleanupTimeout, true, func(ctx context.Context) (bool, error) {
		pnpList := &globalnetworkingv1.ProjectNetworkPolicyList{}
		if listErr := env.GlobalClient.List(ctx, pnpList, client.InNamespace(env.Project), client.MatchingLabels{pnpLabelServiceUID: svcUID}); listErr != nil {
			t.Logf("Transient error listing PNPs during cleanup check: %v", listErr)
			return false, nil
		}
		return len(pnpList.Items) == 0, nil
	}); err != nil {
		t.Fatalf("timed out waiting for ProjectNetworkPolicies with label %s=%s to be deleted: %v", pnpLabelServiceUID, svcUID, err)
	}
}

func waitForObjectDeleted(ctx context.Context, t *testing.T, c client.Client, key client.ObjectKey, obj client.Object) {
	t.Helper()
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, cleanupTimeout, true, func(ctx context.Context) (bool, error) {
		err := c.Get(ctx, key, obj)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, nil
	}); err != nil {
		t.Fatalf("timed out waiting for %T %s to be deleted: %v", obj, key, err)
	}
}

func deleteServiceAndWait(ctx context.Context, t *testing.T, vclusterClient client.Client, namespace, name string) {
	t.Helper()
	key := client.ObjectKey{Namespace: namespace, Name: name}
	svc := &corev1.Service{}
	if err := vclusterClient.Get(ctx, key, svc); apierrors.IsNotFound(err) {
		return
	}
	if err := vclusterClient.Delete(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
	}); client.IgnoreNotFound(err) != nil {
		t.Logf("Warning: failed to delete Service %s/%s: %v", namespace, name, err)
		return
	}
	if t.Failed() {
		// When cleaning up after a test failure, do not block for 5 minutes if CCM could not remove the finalizer.
		_ = wait.PollUntilContextTimeout(ctx, pollInterval, 15*time.Second, true, func(ctx context.Context) (bool, error) {
			return apierrors.IsNotFound(vclusterClient.Get(ctx, key, svc)), nil
		})
		if err := vclusterClient.Get(ctx, key, svc); err == nil && len(svc.Finalizers) > 0 {
			svc.Finalizers = nil
			_ = vclusterClient.Update(ctx, svc)
		}
		return
	}
	waitForObjectDeleted(ctx, t, vclusterClient, key, &corev1.Service{})
}

type testSubnetSpec struct {
	Namespace       string
	Name            string
	SubnetType      ipamv1.SubnetType
	PrefixLength    int32
	ParentName      string
	ParentNamespace string
	ParentType      ipamv1.ReferenceType
	Labels          map[string]string
}

func ensureTestSubnet(ctx context.Context, t *testing.T, globalClient client.Client, spec testSubnetSpec) string {
	t.Helper()
	t.Logf("Creating %s Subnet %s/%s (/%d, parent=%s/%s)", spec.SubnetType, spec.Namespace, spec.Name, spec.PrefixLength, spec.ParentNamespace, spec.ParentName)

	s := &ipamglobalv1.Subnet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: spec.Namespace,
			Name:      spec.Name,
		},
	}
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, time.Minute, true, func(ctx context.Context) (bool, error) {
		_, err := controllerutil.CreateOrUpdate(ctx, globalClient, s, func() error {
			if s.Annotations == nil {
				s.Annotations = make(map[string]string)
			}
			s.Annotations[ipamv1.PauseAutoDivisionAnnotation] = "true"
			s.Labels = spec.Labels
			s.Spec = ipamglobalv1.SubnetSpec{
				Type: spec.SubnetType,
				IPv4Request: &ipamv1.SubnetRequest{
					PrefixLength: ptr.To(spec.PrefixLength),
				},
				ParentReference: &ipamv1.SubnetReference{
					Name:      spec.ParentName,
					Namespace: ptr.To(spec.ParentNamespace),
					Type:      spec.ParentType,
				},
			}
			return nil
		})
		return err == nil, nil
	}); err != nil {
		t.Fatalf("failed to create Subnet %s/%s: %v", spec.Namespace, spec.Name, err)
	}

	var cidr string
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		current := &ipamglobalv1.Subnet{}
		if getErr := globalClient.Get(ctx, client.ObjectKey{Namespace: spec.Namespace, Name: spec.Name}, current); getErr != nil {
			return false, client.IgnoreNotFound(getErr)
		}
		if meta.IsStatusConditionTrue(current.Status.Conditions, "Ready") && current.Status.IPv4Allocation != nil && current.Status.IPv4Allocation.CIDR != "" {
			cidr = current.Status.IPv4Allocation.CIDR
			return true, nil
		}
		return false, nil
	}); err != nil {
		t.Fatalf("Subnet %s/%s did not become Ready with IPv4 allocation: %v", spec.Namespace, spec.Name, err)
	}

	t.Logf("Subnet %s/%s is Ready with CIDR %s", spec.Namespace, spec.Name, cidr)
	return cidr
}

func deleteTestSubnet(ctx context.Context, t *testing.T, globalClient client.Client, key client.ObjectKey) {
	t.Helper()
	_ = wait.PollUntilContextTimeout(ctx, pollInterval, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		s := &ipamglobalv1.Subnet{}
		if err := globalClient.Get(ctx, key, s); err != nil {
			return apierrors.IsNotFound(err), nil
		}
		if s.DeletionTimestamp.IsZero() {
			_ = globalClient.Delete(ctx, s)
		}
		return false, nil
	})
}

func assertIPInCIDR(t *testing.T, ipStr, cidrStr string) {
	t.Helper()
	_, ipNet, err := net.ParseCIDR(cidrStr)
	if err != nil {
		t.Fatalf("failed to parse CIDR %q: %v", cidrStr, err)
	}
	parsedIP := net.ParseIP(ipStr)
	if parsedIP == nil {
		t.Fatalf("failed to parse IP %q", ipStr)
	}
	if !ipNet.Contains(parsedIP) {
		t.Fatalf("expected IP %s to be within subnet CIDR %s", ipStr, cidrStr)
	}
	t.Logf("Verified VIP %s is within custom subnet CIDR %s", ipStr, cidrStr)
}
