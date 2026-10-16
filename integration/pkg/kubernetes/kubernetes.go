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

package kubernetes

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	clientwatch "k8s.io/client-go/tools/watch"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CreateNamespace creates a new namespace.
func CreateNamespace(ctx context.Context, k8sclient client.WithWatch, name string) error {
	namespaceObj := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	if err := k8sclient.Create(ctx, namespaceObj); err != nil {
		return fmt.Errorf("failed to create Namespace: %w", err)
	}
	return nil
}

// CleanupResources cascades deletion of a test namespace.
func CleanupResources(t *testing.T, k8sclient client.WithWatch, namespace string) {
	t.Helper()

	ctx := context.Background()
	namespaceObj := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	}
	t.Logf("Cleaning up resources in namespace %s", namespace)
	if err := k8sclient.Delete(ctx, namespaceObj); err != nil && !apierrors.IsNotFound(err) {
		t.Logf("Failed to cascade deleting resources in namespace %q: %v", namespace, err)
	} else {
		t.Logf("Cascade deleted all the resources in namespace %q", namespace)
	}
}

// WaitForPodReady waits for a Pod to reach 'Running' phase and 'Ready' condition.
func WaitForPodReady(ctx context.Context, k8sclient client.WithWatch, namespace, podName string, timeout time.Duration) error {
	podList := &corev1.PodList{}
	listOptions := []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingFields{"metadata.name": podName},
	}

	err := WaitForCondition[*corev1.Pod](
		ctx, timeout,
		func() (watch.Interface, error) {
			return k8sclient.Watch(ctx, podList, listOptions...)
		},
		func(pod *corev1.Pod) bool {
			if pod.Status.Phase != corev1.PodRunning {
				return false
			}
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					return true
				}
			}
			return false
		},
	)
	if err != nil {
		return fmt.Errorf("pod %s was not Ready: %w", podName, err)
	}
	return nil
}

// WaitForDeploymentReady waits for a Deployment's 'Available' condition to be 'True'.
func WaitForDeploymentReady(ctx context.Context, k8sclient client.WithWatch, namespace, deploymentName string, timeout time.Duration) error {
	deploymentList := &appsv1.DeploymentList{}
	listOptions := []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingFields{"metadata.name": deploymentName},
	}

	err := WaitForCondition[*appsv1.Deployment](
		ctx, timeout,
		func() (watch.Interface, error) {
			return k8sclient.Watch(ctx, deploymentList, listOptions...)
		},
		func(deployment *appsv1.Deployment) bool {
			if deployment.Status.ObservedGeneration < deployment.Generation {
				return false
			}

			replicas := ptr.Deref(deployment.Spec.Replicas, 1)
			if deployment.Status.UpdatedReplicas == replicas &&
				deployment.Status.AvailableReplicas == replicas {
				return true
			}

			return false
		},
	)
	if err != nil {
		return fmt.Errorf("deployment %s was not ready: %w", deploymentName, err)
	}
	return nil
}

// WaitForCondition watches a Kubernetes resource until the isReady predicate returns true
// or the timeout is reached.
func WaitForCondition[T runtime.Object](
	ctx context.Context,
	timeout time.Duration,
	startWatch func() (watch.Interface, error),
	isReady func(obj T) bool,
) error {
	return wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		watcher, err := startWatch()
		if err != nil {
			return false, nil
		}

		watchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		_, err = clientwatch.UntilWithoutRetry(watchCtx, watcher, func(event watch.Event) (bool, error) {
			obj, ok := event.Object.(T)
			return ok && isReady(obj), nil
		})
		if err != nil {
			return false, ctx.Err()
		}
		return true, nil
	})
}
