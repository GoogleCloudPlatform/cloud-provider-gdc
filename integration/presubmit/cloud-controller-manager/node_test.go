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
	"strings"
	"testing"

	vmv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/virtualmachine/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func runNodeControllerTests(ctx context.Context, t *testing.T, env *TestEnv) {
	t.Run("UninitializedNodeRegistrationAndMetadataSync", func(t *testing.T) {
		targetVM, vmIP := findRunningVirtualMachine(ctx, t, env)
		expectedProviderID := fmt.Sprintf("gdch://%s/%s/%s", env.Project, env.Zone, targetVM.Name)
		expectedInstanceType := targetVM.Spec.Compute.VirtualMachineType

		t.Logf("Using existing running VirtualMachine %s/%s (type=%s, ip=%s) to test cloud-node-controller registration",
			env.Project, targetVM.Name, expectedInstanceType, vmIP)

		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: targetVM.Name,
			},
			Spec: corev1.NodeSpec{
				Taints: []corev1.Taint{
					{
						Key:    uninitializedNodeTaintKey,
						Value:  "true",
						Effect: corev1.TaintEffectNoSchedule,
					},
				},
			},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{
					{
						Type:   corev1.NodeReady,
						Status: corev1.ConditionTrue,
					},
				},
			},
		}

		// Create a scheduled Pod referencing targetVM.Name so vcluster's fake-node controller
		// considers the Node needed and does not garbage-collect it before CCM initializes it.
		pinPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ccm-node-pin-" + cfg.CommitHash,
				Namespace: corev1.NamespaceDefault,
			},
			Spec: corev1.PodSpec{
				NodeName: targetVM.Name,
				Tolerations: []corev1.Toleration{
					{Operator: corev1.TolerationOpExists},
				},
				Containers: []corev1.Container{
					{
						Name:  "pause",
						Image: "registry.k8s.io/pause:3.10",
					},
				},
			},
		}
		if err := env.VClusterClient.Create(ctx, pinPod); err != nil {
			t.Fatalf("failed to create node-pin Pod %q in vcluster: %v", pinPod.Name, err)
		}
		t.Cleanup(func() {
			_ = env.VClusterClient.Delete(context.Background(), pinPod)
		})

		// Ensure the Node exists and has the uninitialized taint + empty ProviderID so
		// gdch-cloud-controller-manager's cloud-node-controller initializes it.
		if err := wait.PollUntilContextTimeout(ctx, pollInterval, reconcileTimeout, true, func(ctx context.Context) (bool, error) {
			existing := &corev1.Node{}
			if getErr := env.VClusterClient.Get(ctx, client.ObjectKey{Name: node.Name}, existing); getErr != nil {
				if apierrors.IsNotFound(getErr) {
					return env.VClusterClient.Create(ctx, node.DeepCopy()) == nil, nil
				}
				return false, nil
			}
			existing.Spec.ProviderID = ""
			existing.Spec.Taints = []corev1.Taint{
				{
					Key:    uninitializedNodeTaintKey,
					Value:  "true",
					Effect: corev1.TaintEffectNoSchedule,
				},
			}
			delete(existing.Labels, corev1.LabelTopologyZone)
			delete(existing.Labels, corev1.LabelTopologyRegion)
			delete(existing.Labels, corev1.LabelInstanceTypeStable)
			return env.VClusterClient.Update(ctx, existing) == nil, nil
		}); err != nil {
			t.Fatalf("failed to prepare uninitialized Node %q in vcluster: %v", node.Name, err)
		}
		t.Cleanup(func() {
			_ = env.VClusterClient.Delete(context.Background(), &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: node.Name},
			})
		})

		t.Logf("Waiting for gdch-cloud-controller-manager to initialize Node %q (expected providerID=%q)...", node.Name, expectedProviderID)
		if err := wait.PollUntilContextTimeout(ctx, pollInterval, reconcileTimeout, true, func(ctx context.Context) (bool, error) {
			current := &corev1.Node{}
			if err := env.VClusterClient.Get(ctx, client.ObjectKey{Name: node.Name}, current); err != nil {
				return false, client.IgnoreNotFound(err)
			}

			// Verify uninitialized taint is removed.
			for _, taint := range current.Spec.Taints {
				if taint.Key == uninitializedNodeTaintKey {
					return false, nil
				}
			}

			if current.Spec.ProviderID != expectedProviderID {
				return false, nil
			}
			if current.Labels[corev1.LabelTopologyZone] != env.Zone {
				return false, nil
			}
			if current.Labels[corev1.LabelTopologyRegion] != env.Region {
				return false, nil
			}
			if current.Labels[corev1.LabelInstanceTypeStable] != expectedInstanceType {
				return false, nil
			}

			hasInternalIP := false
			for _, addr := range current.Status.Addresses {
				if addr.Type == corev1.NodeInternalIP && addr.Address != "" {
					hasInternalIP = true
					break
				}
			}
			return hasInternalIP, nil
		}); err != nil {
			current := &corev1.Node{}
			_ = env.VClusterClient.Get(ctx, client.ObjectKey{Name: node.Name}, current)
			t.Fatalf("Node %q was not initialized as expected within timeout (providerID=%q, labels=%v, addresses=%v, taints=%v): %v",
				node.Name, current.Spec.ProviderID, current.Labels, current.Status.Addresses, current.Spec.Taints, err)
		}

		t.Logf("Verified Node %q initialized with ProviderID=%q, zone=%q, region=%q, instance-type=%q, InternalIP=%q, and uninitialized taint removed",
			node.Name, expectedProviderID, env.Zone, env.Region, expectedInstanceType, vmIP)
	})

	t.Run("NonExistentNodeLifecycleCleanup", func(t *testing.T) {
		nonExistentName := "ccm-nonexistent-vm-" + cfg.CommitHash
		nonExistentProviderID := fmt.Sprintf("gdch://%s/%s/%s", env.Project, env.Zone, nonExistentName)

		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nonExistentName,
			},
			Spec: corev1.NodeSpec{
				ProviderID: nonExistentProviderID,
			},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{
					{
						Type:   corev1.NodeReady,
						Status: corev1.ConditionFalse,
					},
				},
			},
		}

		pinPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ccm-nonexistent-pin-" + cfg.CommitHash,
				Namespace: corev1.NamespaceDefault,
			},
			Spec: corev1.PodSpec{
				NodeName: nonExistentName,
				Tolerations: []corev1.Toleration{
					{Operator: corev1.TolerationOpExists},
				},
				Containers: []corev1.Container{
					{
						Name:  "pause",
						Image: "registry.k8s.io/pause:3.10",
					},
				},
			},
		}
		if err := env.VClusterClient.Create(ctx, pinPod); err != nil {
			t.Fatalf("failed to create nonexistent-node pin Pod %q in vcluster: %v", pinPod.Name, err)
		}
		t.Cleanup(func() {
			_ = env.VClusterClient.Delete(context.Background(), pinPod)
		})

		t.Logf("Creating NotReady Node %q with non-existent ProviderID %q in vcluster", nonExistentName, nonExistentProviderID)
		if err := wait.PollUntilContextTimeout(ctx, pollInterval, reconcileTimeout, true, func(ctx context.Context) (bool, error) {
			existing := &corev1.Node{}
			if getErr := env.VClusterClient.Get(ctx, client.ObjectKey{Name: nonExistentName}, existing); getErr != nil {
				if !apierrors.IsNotFound(getErr) {
					return false, nil
				}
				if createErr := env.VClusterClient.Create(ctx, node.DeepCopy()); createErr != nil {
					t.Logf("Transient error creating Node %s: %v", nonExistentName, createErr)
					return false, nil
				}
			}
			if getErr := env.VClusterClient.Get(ctx, client.ObjectKey{Name: nonExistentName}, existing); getErr != nil {
				t.Logf("Transient error getting Node %s: %v", nonExistentName, getErr)
				return false, nil
			}
			existing.Spec.ProviderID = nonExistentProviderID
			if updateErr := env.VClusterClient.Update(ctx, existing); updateErr != nil {
				t.Logf("Transient error updating Node %s: %v", nonExistentName, updateErr)
				return false, nil
			}
			existing.Status.Conditions = []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionFalse,
				},
			}
			return env.VClusterClient.Status().Update(ctx, existing) == nil, nil
		}); err != nil {
			t.Fatalf("failed to prepare NotReady Node %q in vcluster: %v", nonExistentName, err)
		}
		t.Cleanup(func() {
			_ = env.VClusterClient.Delete(context.Background(), &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: nonExistentName},
			})
		})

		t.Logf("Waiting for cloud-node-lifecycle-controller to delete Node %q (InstanceExists == false)...", nonExistentName)
		if err := wait.PollUntilContextTimeout(ctx, pollInterval, reconcileTimeout, true, func(ctx context.Context) (bool, error) {
			events := &corev1.EventList{}
			if listErr := env.VClusterClient.List(ctx, events, client.InNamespace(corev1.NamespaceDefault)); listErr == nil {
				for _, ev := range events.Items {
					if ev.InvolvedObject.Kind == "Node" && ev.InvolvedObject.Name == nonExistentName && ev.Reason == "DeletingNode" {
						return true, nil
					}
				}
			}
			current := &corev1.Node{}
			err := env.VClusterClient.Get(ctx, client.ObjectKey{Name: nonExistentName}, current)
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, nil
		}); err != nil {
			t.Fatalf("expected non-existent Node %q to be deleted by cloud-node-lifecycle-controller, but it was not: %v", nonExistentName, err)
		}

		t.Logf("Verified non-existent Node %q was automatically deleted by cloud-node-lifecycle-controller", nonExistentName)
	})
}

func findRunningVirtualMachine(ctx context.Context, t *testing.T, env *TestEnv) (*vmv1.VirtualMachine, string) {
	t.Helper()

	vmList := &vmv1.VirtualMachineList{}
	if err := env.MgmtClient.List(ctx, vmList, client.InNamespace(env.Project)); err != nil {
		t.Fatalf("failed to list VirtualMachines in namespace %q: %v", env.Project, err)
	}

	for i := range vmList.Items {
		vm := &vmList.Items[i]
		if !vm.DeletionTimestamp.IsZero() || vm.Status.State != vmv1.VirtualMachineStateRunning {
			continue
		}
		if vm.Spec.Compute.VirtualMachineType == "" || len(vm.Status.Network.Interfaces) == 0 {
			continue
		}
		for _, rawIP := range vm.Status.Network.Interfaces[0].IpAddresses {
			ip := strings.Split(rawIP, "/")[0]
			if ip != "" {
				return vm, ip
			}
		}
	}

	t.Fatalf("no running VirtualMachine with an IP address found in project %q (zone %q)", env.Project, env.Zone)
	return nil, ""
}
