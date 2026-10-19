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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	ipamglobalv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/ipam/v1"
	globalnetworkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/networking/v1"
	ipamv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/ipam/v1"
	networkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/networking/v1"
	vmv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/virtualmachine/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	kubernetesclientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/googlecloudplatform/cloud-provider-gdc/gdc/pkg/auth"
	gdcclient "github.com/googlecloudplatform/cloud-provider-gdc/gdc/pkg/client"
	"github.com/googlecloudplatform/cloud-provider-gdc/integration/pkg/gdc"
	"github.com/googlecloudplatform/cloud-provider-gdc/integration/pkg/gdcloud"
	"github.com/googlecloudplatform/cloud-provider-gdc/integration/pkg/helm"
	"github.com/googlecloudplatform/cloud-provider-gdc/integration/pkg/kubernetes"
)

func setupTestEnvironment(ctx context.Context, t *testing.T) *TestEnv {
	t.Helper()

	scheme := buildScheme(t)

	// 1. Initialize hermetic gdcloud client and obtain User Cluster kubeconfig/client.
	consoleURL := fmt.Sprintf("https://console.%s.%s.%s", cfg.Org, cfg.Zone, cfg.LabURL)
	gdcTestingClient, err := gdcloud.NewTestingClient(cfg.CAFile, cfg.SAFile, consoleURL)
	if err != nil {
		t.Fatalf("failed to initialize gdcloud client: %v", err)
	}
	t.Cleanup(func() {
		if err := gdcTestingClient.Cleanup(); err != nil {
			t.Logf("Warning: failed to cleanup gdcloud client: %v", err)
		}
	})

	hostKubeconfig, err := gdc.GetUserClusterKubeconfig(gdcTestingClient, cfg.Zone, cfg.Project, cfg.VUC)
	if err != nil {
		t.Fatalf("failed to get kubeconfig for VUC %q: %v", cfg.VUC, err)
	}

	hostClient, err := gdc.GetUserClusterClient(gdcTestingClient, cfg.Zone, cfg.Project, cfg.VUC, scheme)
	if err != nil {
		t.Fatalf("failed to create client for VUC %q: %v", cfg.VUC, err)
	}

	// 2. Create Global API and Zonal Management API clients using GDC ServiceAccount STS exchange.
	rawCA, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		t.Fatalf("failed to read CA file %q: %v", cfg.CAFile, err)
	}
	encodedCA := base64.StdEncoding.EncodeToString(rawCA)

	saBytes, err := os.ReadFile(cfg.SAFile)
	if err != nil {
		t.Fatalf("failed to read ServiceAccount file %q: %v", cfg.SAFile, err)
	}
	var sa auth.ServiceAccount
	if err := json.Unmarshal(saBytes, &sa); err != nil {
		t.Fatalf("failed to unmarshal ServiceAccount JSON: %v", err)
	}

	globalURL := fmt.Sprintf("https://global-api.%s.%s.%s", cfg.Org, cfg.Zone, cfg.LabURL)
	globalClient, err := gdcclient.Get(&gdcclient.OrgClusterConfig{
		OrgClusterURL: globalURL,
		CAData:        encodedCA,
	}, &sa, scheme)
	if err != nil {
		t.Fatalf("failed to create Global API client: %v", err)
	}

	mgmtURL := fmt.Sprintf("https://management-kube.apiserver.%s.%s.%s", cfg.Org, cfg.Zone, cfg.LabURL)
	mgmtClient, err := gdcclient.Get(&gdcclient.OrgClusterConfig{
		OrgClusterURL: mgmtURL,
		CAData:        encodedCA,
	}, &sa, scheme)
	if err != nil {
		t.Fatalf("failed to create Zonal Management API client: %v", err)
	}

	// Clean up any stale custom test subnets from previous interrupted runs.
	cleanupStaleCustomSubnets(ctx, t, globalClient, cfg.Project)

	// 3. Create isolated host namespace on the User Cluster.
	hostNamespace := "ccm-e2e-" + cfg.CommitHash
	clusterName := hostNamespace
	t.Logf("Creating host namespace %q on VUC %q", hostNamespace, cfg.VUC)
	if err := kubernetes.CreateNamespace(ctx, hostClient, hostNamespace); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("failed to create host namespace %q: %v", hostNamespace, err)
	}
	t.Cleanup(func() {
		kubernetes.CleanupResources(t, hostClient, hostNamespace)
	})

	// 4. Deploy isolated vcluster inside hostNamespace (with service syncing to host disabled).
	vclusterName := "vcluster-" + cfg.CommitHash
	vclusterClient, vclusterLocalKubeconfig, vclusterInClusterKubeconfig := setupVCluster(
		ctx, t, hostClient, hostNamespace, vclusterName, scheme, hostKubeconfig,
	)

	// 5. Deploy gdch-cloud-controller-manager on the host cluster connected to vcluster.
	deployCloudControllerManager(
		ctx, t, hostClient, hostNamespace, clusterName,
		encodedCA, globalURL, mgmtURL, saBytes, vclusterInClusterKubeconfig,
	)

	// Dump CCM pod logs if any test fails, before vcluster and namespace teardown run.
	t.Cleanup(func() {
		if t.Failed() {
			dumpPodLogsOnFailure(ctx, t, hostKubeconfig, hostNamespace)
		}
	})

	t.Logf("Waiting for %s deployment in namespace %q to become Ready...", ccmAppName, hostNamespace)
	if err := kubernetes.WaitForDeploymentReady(ctx, hostClient, hostNamespace, ccmAppName, provisioningTimeout); err != nil {
		t.Fatalf("deployment %s in namespace %q did not become ready: %v", ccmAppName, hostNamespace, err)
	}
	t.Logf("Deployment %s is Ready", ccmAppName)

	return &TestEnv{
		HostClient:         hostClient,
		VClusterClient:     vclusterClient,
		GlobalClient:       globalClient,
		MgmtClient:         mgmtClient,
		Scheme:             scheme,
		HostNamespace:      hostNamespace,
		ClusterName:        clusterName,
		Project:            cfg.Project,
		Zone:               cfg.Zone,
		Region:             cfg.Region,
		HostKubeconfig:     hostKubeconfig,
		VClusterKubeconfig: vclusterLocalKubeconfig,
	}
}

func buildScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, addFn := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		globalnetworkingv1.AddToScheme,
		ipamglobalv1.AddToScheme,
		networkingv1.AddToScheme,
		ipamv1.AddToScheme,
		vmv1.AddToScheme,
	} {
		if err := addFn(scheme); err != nil {
			t.Fatalf("failed to register scheme: %v", err)
		}
	}
	return scheme
}

func setupVCluster(
	ctx context.Context,
	t *testing.T,
	hostClient client.WithWatch,
	hostNamespace, vclusterName string,
	scheme *runtime.Scheme,
	hostKubeconfig string,
) (client.WithWatch, string, []byte) {
	t.Helper()

	values := map[string]interface{}{
		"controlPlane": map[string]interface{}{
			"distro": map[string]interface{}{
				"k8s": map[string]interface{}{
					"image": map[string]interface{}{
						"tag": cfg.VClusterK8sTag,
					},
				},
			},
			"statefulSet": map[string]interface{}{
				"persistence": map[string]interface{}{
					"volumeClaim": map[string]interface{}{
						"enabled": false,
					},
				},
			},
		},
		"networking": map[string]interface{}{
			"advanced": map[string]interface{}{
				"fallbackHostCluster": true,
			},
		},
		// Disable syncing Services and Pods to the host cluster so only our test
		// gdch-cloud-controller-manager reconciles LoadBalancer Services created in vcluster.
		"sync": map[string]interface{}{
			"toHost": map[string]interface{}{
				"services": map[string]interface{}{
					"enabled": false,
				},
				"pods": map[string]interface{}{
					"enabled": false,
				},
			},
		},
	}

	t.Logf("Installing vcluster %q in namespace %q", vclusterName, hostNamespace)
	if _, err := helm.InstallOrUpgrade(helm.InstallOptions{
		ChartPath:      cfg.VClusterChart,
		KubeconfigPath: hostKubeconfig,
		ReleaseName:    vclusterName,
		Namespace:      hostNamespace,
		Values:         values,
		Timeout:        5 * time.Minute,
	}); err != nil {
		t.Fatalf("failed to install vcluster helm chart: %v", err)
	}

	t.Cleanup(func() {
		t.Logf("Uninstalling vcluster %q in namespace %q", vclusterName, hostNamespace)
		if _, err := helm.Uninstall(helm.UninstallOptions{
			KubeconfigPath: hostKubeconfig,
			ReleaseName:    vclusterName,
			Namespace:      hostNamespace,
			IgnoreNotFound: true,
			Wait:           true,
		}); err != nil {
			t.Logf("Warning: failed to uninstall vcluster %q: %v", vclusterName, err)
		}
	})

	// Wait for the vcluster kubeconfig secret to be generated.
	secretName := "vc-" + vclusterName
	vcSecret := &corev1.Secret{}
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		if err := hostClient.Get(ctx, client.ObjectKey{Namespace: hostNamespace, Name: secretName}, vcSecret); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return len(vcSecret.Data["config"]) > 0, nil
	}); err != nil {
		t.Fatalf("timed out waiting for vcluster secret %s/%s: %v", hostNamespace, secretName, err)
	}

	// Wait for vcluster pods to be Ready before port-forwarding.
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		vcPods := &corev1.PodList{}
		if listErr := hostClient.List(ctx, vcPods, client.InNamespace(hostNamespace), client.MatchingLabels{"app": "vcluster", "release": vclusterName}); listErr != nil {
			t.Logf("Transient error listing vcluster pods: %v", listErr)
			return false, nil
		}
		if len(vcPods.Items) == 0 {
			return false, nil
		}
		for _, p := range vcPods.Items {
			ready := false
			for _, cond := range p.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					ready = true
					break
				}
			}
			if !ready {
				return false, nil
			}
		}
		return true, nil
	}); err != nil {
		t.Fatalf("vcluster pods in %q did not become ready: %v", hostNamespace, err)
	}

	// Allocate a local port for kubectl port-forward.
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to allocate local port for vcluster port-forward: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	pfCtx, pfCancel := context.WithCancel(ctx)
	go func() {
		for {
			select {
			case <-pfCtx.Done():
				return
			default:
				cmd := exec.CommandContext(pfCtx, "kubectl", "--kubeconfig", hostKubeconfig, "port-forward", "svc/"+vclusterName, fmt.Sprintf("%d:443", port), "-n", hostNamespace)
				output, err := cmd.CombinedOutput()
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Logf("vcluster port-forward exited: %v (output: %s)", err, string(output))
				}
				time.Sleep(1 * time.Second)
			}
		}
	}()
	t.Cleanup(func() {
		pfCancel()
	})

	restConfig, err := clientcmd.RESTConfigFromKubeConfig(vcSecret.Data["config"])
	if err != nil {
		t.Fatalf("failed to parse vcluster REST config: %v", err)
	}
	restConfig.Host = fmt.Sprintf("https://localhost:%d", port)
	restConfig.QPS = 100.0
	restConfig.Burst = 150

	vclusterClient, err := client.NewWithWatch(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("failed to create vcluster client: %v", err)
	}

	// Wait for the vcluster API server to respond over port-forward.
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		_, err := vclusterClient.RESTMapper().RESTMapping(schema.GroupKind{Group: "", Kind: "Service"}, "v1")
		return err == nil, nil
	}); err != nil {
		t.Fatalf("vcluster API server not reachable via port-forward: %v", err)
	}

	// Build local kubeconfig file for vcluster.
	localAPIConfig, err := clientcmd.Load(vcSecret.Data["config"])
	if err != nil {
		t.Fatalf("failed to load vcluster kubeconfig: %v", err)
	}
	for _, cluster := range localAPIConfig.Clusters {
		cluster.Server = fmt.Sprintf("https://localhost:%d", port)
	}
	localKubeconfigBytes, err := clientcmd.Write(*localAPIConfig)
	if err != nil {
		t.Fatalf("failed to serialize local vcluster kubeconfig: %v", err)
	}
	tmpFile, err := os.CreateTemp("", "vcluster-kubeconfig-*")
	if err != nil {
		t.Fatalf("failed to create temp vcluster kubeconfig file: %v", err)
	}
	if _, err := tmpFile.Write(localKubeconfigBytes); err != nil {
		t.Fatalf("failed to write temp vcluster kubeconfig file: %v", err)
	}
	_ = tmpFile.Close()
	t.Cleanup(func() {
		_ = os.Remove(tmpFile.Name())
	})

	// Build in-cluster kubeconfig bytes pointing to the vcluster Service DNS name
	// so the gdch-cloud-controller-manager Pod can connect to vcluster directly.
	inClusterAPIConfig, err := clientcmd.Load(vcSecret.Data["config"])
	if err != nil {
		t.Fatalf("failed to load vcluster kubeconfig for in-cluster use: %v", err)
	}
	for _, cluster := range inClusterAPIConfig.Clusters {
		cluster.Server = fmt.Sprintf("https://%s.%s:443", vclusterName, hostNamespace)
	}
	inClusterKubeconfigBytes, err := clientcmd.Write(*inClusterAPIConfig)
	if err != nil {
		t.Fatalf("failed to serialize in-cluster vcluster kubeconfig: %v", err)
	}

	return vclusterClient, tmpFile.Name(), inClusterKubeconfigBytes
}

func deployCloudControllerManager(
	ctx context.Context,
	t *testing.T,
	hostClient client.WithWatch,
	hostNamespace, clusterName, encodedCA, globalURL, mgmtURL string,
	saBytes, vclusterInClusterKubeconfig []byte,
) {
	t.Helper()

	cloudProviderConf := fmt.Sprintf(`[Global]
project = %s
org-admin-name = %s
global-management-server-url = %s
ca = %s
[zone_0]
name = %s
management-api = %s
`, cfg.Project, cfg.Org, globalURL, encodedCA, cfg.Zone, mgmtURL)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ccmConfigMapName,
			Namespace: hostNamespace,
		},
		Data: map[string]string{
			"cloudprovider.conf": cloudProviderConf,
		},
	}
	if err := hostClient.Create(ctx, cm); err != nil {
		t.Fatalf("failed to create ConfigMap %s: %v", ccmConfigMapName, err)
	}

	saSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ccmSASecretName,
			Namespace: hostNamespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"serviceaccount.json": saBytes,
		},
	}
	if err := hostClient.Create(ctx, saSecret); err != nil {
		t.Fatalf("failed to create Secret %s: %v", ccmSASecretName, err)
	}

	vcKubeconfigSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ccmVClusterSecretName,
			Namespace: hostNamespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"kubeconfig": vclusterInClusterKubeconfig,
		},
	}
	if err := hostClient.Create(ctx, vcKubeconfigSecret); err != nil {
		t.Fatalf("failed to create Secret %s: %v", ccmVClusterSecretName, err)
	}

	labels := map[string]string{
		"app":  "kubernetes",
		"role": ccmAppName,
	}
	ccmDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ccmAppName,
			Namespace: hostNamespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr.To(false),
					Containers: []corev1.Container{
						{
							Name:            ccmAppName,
							Image:           cfg.CCMImageTag,
							ImagePullPolicy: corev1.PullAlways,
							Command: []string{
								"/gdch-cloud-controller-manager",
								"--allocate-node-cidrs=false",
								"--cloud-provider=gdch",
								"--cloud-config=/etc/kubernetes/cloudprovider/cloudprovider.conf",
								"--cluster-name=" + clusterName,
								"--concurrent-service-syncs=10",
								"--configure-cloud-routes=false",
								"--kubeconfig=/etc/kubernetes/vcluster/kubeconfig",
								"--authentication-kubeconfig=/etc/kubernetes/vcluster/kubeconfig",
								"--authorization-kubeconfig=/etc/kubernetes/vcluster/kubeconfig",
								"--leader-elect=false",
								"--node-monitor-period=5s",
								"--v=2",
							},
							Env: []corev1.EnvVar{
								{
									Name:  "GDCH_ORG_ADMIN_SERVICE_ACCOUNT",
									Value: "/srv/cloudprovider/serviceaccount.json",
								},
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path:   "/healthz",
										Port:   intstr.FromInt32(10258),
										Scheme: corev1.URISchemeHTTPS,
									},
								},
								InitialDelaySeconds: 10,
								PeriodSeconds:       10,
								TimeoutSeconds:      10,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path:   "/healthz",
										Port:   intstr.FromInt32(10258),
										Scheme: corev1.URISchemeHTTPS,
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       5,
								TimeoutSeconds:      5,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "cloud-provider-config",
									MountPath: "/etc/kubernetes/cloudprovider",
									ReadOnly:  true,
								},
								{
									Name:      "cloudprovider-sa",
									MountPath: "/srv/cloudprovider",
									ReadOnly:  true,
								},
								{
									Name:      "vcluster-kubeconfig",
									MountPath: "/etc/kubernetes/vcluster",
									ReadOnly:  true,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "cloud-provider-config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: ccmConfigMapName,
									},
								},
							},
						},
						{
							Name: "cloudprovider-sa",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: ccmSASecretName,
								},
							},
						},
						{
							Name: "vcluster-kubeconfig",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: ccmVClusterSecretName,
								},
							},
						},
					},
				},
			},
		},
	}

	if err := hostClient.Create(ctx, ccmDeployment); err != nil {
		t.Fatalf("failed to create Deployment %s: %v", ccmAppName, err)
	}
}

func cleanupStaleCustomSubnets(ctx context.Context, t *testing.T, globalClient client.Client, project string) {
	t.Helper()

	deleteBySuffix := func(suffix string) {
		_ = wait.PollUntilContextTimeout(ctx, pollInterval, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
			var subnetList ipamglobalv1.SubnetList
			if listErr := globalClient.List(ctx, &subnetList, client.InNamespace(project)); listErr != nil {
				t.Logf("Transient error listing subnets during cleanup: %v", listErr)
				return false, nil
			}
			remaining := 0
			for i := range subnetList.Items {
				s := &subnetList.Items[i]
				if (strings.HasPrefix(s.Name, elbTestSubnetPrefix) || strings.HasPrefix(s.Name, ilbTestSubnetPrefix)) && strings.HasSuffix(s.Name, suffix) {
					remaining++
					if s.DeletionTimestamp.IsZero() {
						t.Logf("Deleting stale test Subnet %s/%s...", project, s.Name)
						_ = globalClient.Delete(ctx, s)
					}
				}
			}
			return remaining == 0, nil
		})
	}

	deleteBySuffix(leafSubnetSuffix)
	deleteBySuffix(branchSubnetSuffix)
}

func dumpPodLogsOnFailure(ctx context.Context, t *testing.T, kubeconfigPath, namespace string) {
	t.Helper()
	t.Logf("--- DUMPING POD LOGS FOR NAMESPACE %q ---", namespace)
	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		t.Logf("Failed to build rest config for log dumping: %v", err)
		return
	}
	restConfig.Timeout = 10 * time.Second

	clientset, err := kubernetesclientset.NewForConfig(restConfig)
	if err != nil {
		t.Logf("Failed to create clientset for log dumping: %v", err)
		return
	}

	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Logf("Failed to list pods in namespace %q: %v", namespace, err)
		return
	}

	for _, pod := range pods.Items {
		t.Logf("Pod: %s, Status: %s, Message: %s, Reason: %s", pod.Name, pod.Status.Phase, pod.Status.Message, pod.Status.Reason)
		for _, container := range pod.Spec.Containers {
			data, err := clientset.CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: container.Name}).DoRaw(ctx)
			if err != nil {
				t.Logf("  Failed to get logs for %s/%s: %v", pod.Name, container.Name, err)
				continue
			}
			t.Logf("  Logs for %s/%s:\n%s", pod.Name, container.Name, string(data))
		}
	}
	t.Log("--- END OF POD LOGS DUMP ---")
}
