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
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/hashicorp/go-multierror"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	globalv1alpha1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/common/global/v1alpha1"
	ipamglobalv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/ipam/v1"
	globalnetworkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/networking/v1"
	ipamv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/ipam/v1"
	networkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/networking/v1"
)

const (
	zone       = "fake-region-zone"
	zoneUnused = "fake-region-zone-unused"
)

type lbResponse struct {
	Status *v1.LoadBalancerStatus
	Exists bool
}

const (
	externalFwdRuleCIDR = "1.2.3.4/21"
	externalFwdRuleIP   = "1.2.3.4"
	internalFwdRuleCIDR = "2.3.4.5/20"
	internalfwdRuleIP   = "2.3.4.5"

	lbName             = "ashoot-uuid"
	lbNamespace        = "test-project"
	lbBranchSubnetName = "lb-branch-parent-subnet"
	lbLeafSubnetName   = "lb-leaf-parent-subnet"
	clusterName        = "shoot-cluster"

	shootName      = "shoot-service"
	shootNamespace = "shoot-namespace"
	shootUUID      = "shoot-uuid"

	// global Obj indexes
	forwardingRuleIndex       = 0
	backendServiceIndex       = 1
	backendServicePolicyIndex = 2
	healthCheckIndex          = 3
	pnpIndex                  = 4
	subnetIndex               = 5
	lbBranchSubnetIndex       = 6
	lbLeafSubnetIndex         = 7

	// zonal Obj indexes
	backendIndex = 0
)

func TestGetLoadBalancer(t *testing.T) {
	gdc, globalScheme, _, err := initFakeGDC()
	if err != nil {
		t.Fatalf("initFakeGDC throws error - %s", err.Error())
	}
	ctx := context.Background()

	// useful test objects
	externalfwdR := &globalnetworkingv1.ForwardingRuleExternal{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lbName + fwdRuleSuffix,
			Namespace: lbNamespace,
		},
		Status: globalnetworkingv1.ForwardingRuleExternalStatus{
			Zones: []globalnetworkingv1.ForwardingRuleExternalZoneStatus{
				{
					ReplicaStatus: networkingv1.ForwardingRuleExternalStatus{
						ForwardingRuleStatusCommon: networkingv1.ForwardingRuleStatusCommon{}, // Simulate missing cidr
					},
					ZoneStatus: globalv1alpha1.ZoneStatus{
						Name: zone,
					},
				},
				{
					ReplicaStatus: networkingv1.ForwardingRuleExternalStatus{
						ForwardingRuleStatusCommon: networkingv1.ForwardingRuleStatusCommon{
							CIDR: externalFwdRuleCIDR,
							Conditions: []metav1.Condition{
								{
									Type:   "Ready",
									Status: metav1.ConditionTrue,
								},
							},
						},
					},
					ZoneStatus: globalv1alpha1.ZoneStatus{
						Name: zone,
					},
				},
			},
		},
	}

	externalfwdRWithNoReadyZone := &globalnetworkingv1.ForwardingRuleExternal{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lbName + fwdRuleSuffix,
			Namespace: lbNamespace,
		},
		Status: globalnetworkingv1.ForwardingRuleExternalStatus{
			Zones: []globalnetworkingv1.ForwardingRuleExternalZoneStatus{
				{
					ReplicaStatus: networkingv1.ForwardingRuleExternalStatus{
						ForwardingRuleStatusCommon: networkingv1.ForwardingRuleStatusCommon{
							CIDR: externalFwdRuleCIDR,
							Conditions: []metav1.Condition{
								{
									Type:   "Ready",
									Status: metav1.ConditionFalse,
								},
							},
						},
					},
					ZoneStatus: globalv1alpha1.ZoneStatus{
						Name: zone,
					},
				},
			},
		},
	}

	noVIPexternalfwdR := &globalnetworkingv1.ForwardingRuleExternal{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lbName + fwdRuleSuffix,
			Namespace: lbNamespace,
		},
	}

	internalfwdR := &globalnetworkingv1.ForwardingRuleInternal{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lbName + fwdRuleSuffix,
			Namespace: lbNamespace,
		},
		Status: globalnetworkingv1.ForwardingRuleInternalStatus{
			Zones: []globalnetworkingv1.ForwardingRuleInternalZoneStatus{
				{
					ReplicaStatus: networkingv1.ForwardingRuleInternalStatus{
						ForwardingRuleStatusCommon: networkingv1.ForwardingRuleStatusCommon{}, // Simulate missing cidr
					},
					ZoneStatus: globalv1alpha1.ZoneStatus{
						Name: zone,
					},
				},
				{
					ReplicaStatus: networkingv1.ForwardingRuleInternalStatus{
						ForwardingRuleStatusCommon: networkingv1.ForwardingRuleStatusCommon{
							CIDR: internalFwdRuleCIDR,
							Conditions: []metav1.Condition{
								{
									Type:   "Ready",
									Status: metav1.ConditionTrue,
								},
							},
						},
					},
					ZoneStatus: globalv1alpha1.ZoneStatus{
						Name: zone,
					},
				},
			},
		},
	}

	internalfwdRWithNoReadyZone := &globalnetworkingv1.ForwardingRuleInternal{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lbName + fwdRuleSuffix,
			Namespace: lbNamespace,
		},
		Status: globalnetworkingv1.ForwardingRuleInternalStatus{
			Zones: []globalnetworkingv1.ForwardingRuleInternalZoneStatus{
				{
					ReplicaStatus: networkingv1.ForwardingRuleInternalStatus{
						ForwardingRuleStatusCommon: networkingv1.ForwardingRuleStatusCommon{
							CIDR: externalFwdRuleCIDR,
							Conditions: []metav1.Condition{
								{
									Type:   "Ready",
									Status: metav1.ConditionFalse,
								},
							},
						},
					},
					ZoneStatus: globalv1alpha1.ZoneStatus{
						Name: zone,
					},
				},
			},
		},
	}

	noVIPinternalfwdR := &globalnetworkingv1.ForwardingRuleInternal{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lbName + fwdRuleSuffix,
			Namespace: lbNamespace,
		},
	}

	externalLBService := initFakeExternalService()
	internalLBService := initFakeInternalService()

	tests := []struct {
		name           string
		service        *v1.Service
		globalMPClient client.Client
		want           lbResponse
		wantErr        bool
	}{
		{
			name:           "Shoot Service missing LB annotation",
			globalMPClient: initFakeClient(globalScheme, []client.Object{internalfwdR, externalfwdR}...),
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      shootName,
					Namespace: shootNamespace,
					UID:       shootUUID,
				},
			},
			want: lbResponse{
				Status: &v1.LoadBalancerStatus{
					Ingress: []v1.LoadBalancerIngress{
						{
							IP: externalFwdRuleIP,
						},
					},
				},
				Exists: true,
			},
		},
		{
			name:           "External LB forwarding rule exists",
			globalMPClient: initFakeClient(globalScheme, []client.Object{internalfwdR, externalfwdR}...),
			service:        externalLBService,
			want: lbResponse{
				Status: &v1.LoadBalancerStatus{
					Ingress: []v1.LoadBalancerIngress{
						{
							IP: externalFwdRuleIP,
						},
					},
				},
				Exists: true,
			},
		},
		{
			name:           "External LB forwarding rule with no ready zone",
			globalMPClient: initFakeClient(globalScheme, []client.Object{externalfwdRWithNoReadyZone}...),
			service:        externalLBService,
			want: lbResponse{
				Status: &v1.LoadBalancerStatus{},
				Exists: true,
			},
		},
		{
			name:           "External LB forwarding rule exists but missing VIP",
			globalMPClient: initFakeClient(globalScheme, []client.Object{noVIPexternalfwdR}...),
			service:        externalLBService,
			want: lbResponse{
				Status: &v1.LoadBalancerStatus{},
				Exists: true,
			},
			wantErr: false,
		},
		{
			name:           "External LB forwarding rule does not exist",
			globalMPClient: initFakeClient(globalScheme, []client.Object{internalfwdR}...),
			service:        externalLBService,
			want: lbResponse{
				Status: nil,
				Exists: false,
			},
		},
		{
			name:           "Internal LB forwarding rule exists",
			globalMPClient: initFakeClient(globalScheme, []client.Object{internalfwdR, externalfwdR}...),
			service:        internalLBService,
			want: lbResponse{
				Status: &v1.LoadBalancerStatus{
					Ingress: []v1.LoadBalancerIngress{
						{
							IP: internalfwdRuleIP,
						},
					},
				},
				Exists: true,
			},
		},
		{
			name:           "Internal LB forwarding rule exists but missing VIP",
			globalMPClient: initFakeClient(globalScheme, []client.Object{noVIPinternalfwdR, externalfwdR}...),
			service:        internalLBService,
			want: lbResponse{
				Status: &v1.LoadBalancerStatus{},
				Exists: true,
			},
		},
		{
			name:           "Internal LB forwarding rule does not exist",
			globalMPClient: initFakeClient(globalScheme, []client.Object{externalfwdR}...),
			service:        internalLBService,
			want: lbResponse{
				Status: nil,
				Exists: false,
			},
		},
		{
			name:           "Internal LB forwarding rule with no ready zone",
			globalMPClient: initFakeClient(globalScheme, []client.Object{internalfwdRWithNoReadyZone}...),
			service:        internalLBService,
			want: lbResponse{
				Status: &v1.LoadBalancerStatus{},
				Exists: true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gdc.lb.(*lb).globalMPClient = tt.globalMPClient
			status, exists, err := gdc.lb.GetLoadBalancer(ctx, clusterName, tt.service)
			gotResponse := lbResponse{
				status,
				exists,
			}
			if (tt.wantErr && err == nil) || (!tt.wantErr && err != nil) {
				t.Fatalf("GetLoadBalancer(%q) returned unexpected error (-want %v\n+got%v)", tt.name, tt.wantErr, err)
			}
			if err != nil {
				return
			}
			if diff := cmp.Diff(tt.want, gotResponse); diff != "" {
				t.Errorf("GetLoadBalancer(%q) returned unexpected diff (-want +got):\n%s", tt.name, diff)
			}
		})
	}
}
func TestGetLoadBalancerWithSmallerActiveZones(t *testing.T) {
	gdc, globalScheme, _, err := initFakeGDC()
	if err != nil {
		t.Fatalf("initFakeGDC throws error - %s", err.Error())
	}
	ctx := context.Background()

	// useful test objects
	externalfwdR := &globalnetworkingv1.ForwardingRuleExternal{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lbName + fwdRuleSuffix,
			Namespace: lbNamespace,
		},
		Status: globalnetworkingv1.ForwardingRuleExternalStatus{
			Zones: []globalnetworkingv1.ForwardingRuleExternalZoneStatus{
				{
					ReplicaStatus: networkingv1.ForwardingRuleExternalStatus{
						ForwardingRuleStatusCommon: networkingv1.ForwardingRuleStatusCommon{
							CIDR: externalFwdRuleCIDR,
						},
					},
					ZoneStatus: globalv1alpha1.ZoneStatus{
						Name: zoneUnused, // Simulate different active zone
					},
				},
			},
		},
	}

	internalfwdR := &globalnetworkingv1.ForwardingRuleInternal{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lbName + fwdRuleSuffix,
			Namespace: lbNamespace,
		},
		Status: globalnetworkingv1.ForwardingRuleInternalStatus{
			Zones: []globalnetworkingv1.ForwardingRuleInternalZoneStatus{
				{
					ReplicaStatus: networkingv1.ForwardingRuleInternalStatus{
						ForwardingRuleStatusCommon: networkingv1.ForwardingRuleStatusCommon{
							CIDR: internalFwdRuleCIDR,
						},
					},
					ZoneStatus: globalv1alpha1.ZoneStatus{
						Name: zoneUnused, // Simulate different active zone
					},
				},
			},
		},
	}

	externalLBService := initFakeExternalService()
	internalLBService := initFakeInternalService()

	tests := []struct {
		name           string
		service        *v1.Service
		globalMPClient client.Client
		want           lbResponse
		wantErr        bool
	}{
		{
			name:           "External LB forwarding rule status missing the ingress IP",
			globalMPClient: initFakeClient(globalScheme, []client.Object{externalfwdR}...),
			service:        externalLBService,
			want: lbResponse{
				Status: &v1.LoadBalancerStatus{},
				Exists: true,
			},
			wantErr: false,
		},
		{
			name:           "Internal LB forwarding rule status missing the ingress IP",
			globalMPClient: initFakeClient(globalScheme, []client.Object{internalfwdR}...),
			service:        internalLBService,
			want: lbResponse{
				Status: &v1.LoadBalancerStatus{},
				Exists: true,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gdc.lb.(*lb).globalMPClient = tt.globalMPClient
			status, exists, err := gdc.lb.GetLoadBalancer(ctx, clusterName, tt.service)
			gotResponse := lbResponse{
				status,
				exists,
			}
			if (tt.wantErr && err == nil) || (!tt.wantErr && err != nil) {
				t.Fatalf("GetLoadBalancer(%q) returned unexpected error (-want %v\n+got%v)", tt.name, tt.wantErr, err)
			}
			if err != nil {
				return
			}
			if diff := cmp.Diff(tt.want, gotResponse); diff != "" {
				t.Errorf("GetLoadBalancer(%q) returned unexpected diff (-want +got):\n%s", tt.name, diff)
			}
		})
	}
}

func TestGetLoadBalancerName(t *testing.T) {
	shootService := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      shootName,
			Namespace: shootNamespace,
			UID:       shootUUID,
		},
	}
	gdc, _, _, _ := initFakeGDC()
	ctx := context.Background()
	name := gdc.lb.GetLoadBalancerName(ctx, clusterName, shootService)
	if name != lbName {
		t.Errorf("GetLoadBalancerName() got = %v, want = %v", name, lbName)
	}
}

func TestEnsureLoadBalancer(t *testing.T) {
	gdc, globalScheme, zonalScheme, err := initFakeGDC()
	if err != nil {
		t.Fatalf("initFakeGDC throws error - %s", err.Error())
	}

	ctx := context.Background()

	// useful test objects
	validExternalService := initFakeExternalService()
	validInternalService := initFakeInternalService()

	validExternalServiceWithHCPort := initFakeExternalService()
	validExternalServiceWithHCPort.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
	validExternalServiceWithHCPort.Spec.HealthCheckNodePort = 32123

	validInternalServiceWithHCPort := initFakeInternalService()
	validInternalServiceWithHCPort.Spec.HealthCheckNodePort = 32124
	validInternalServiceWithHCPort.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal

	extGlobalObjs, extZonalObjs := initFakeExternalLBObjs(lbName, lbNamespace, clusterName, validExternalService)
	intGlobalObjs, intZonalObjs := initFakeInternalLBObjs(lbName, lbNamespace, clusterName, validInternalService)
	extGlobalObjsWithHCPort, extZonalObjsWithHCPort := initFakeExternalLBObjs(lbName, lbNamespace, clusterName, validExternalServiceWithHCPort)
	intGlobalObjsWithHCPort, intZonalObjsWithHCPort := initFakeInternalLBObjs(lbName, lbNamespace, clusterName, validInternalServiceWithHCPort)

	tests := []struct {
		name           string
		service        *v1.Service
		globalClient   client.Client
		zonalClients   map[string]client.Client
		wantErr        bool
		wantGlobalObjs []client.Object
		wantZonalObjs  []client.Object
	}{
		{
			name: "LB service with mixed protocols",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      shootName,
					Namespace: shootNamespace,
					UID:       shootUUID,
					Annotations: map[string]string{
						lbType: lbExternal,
					},
				},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeLoadBalancer,
					Ports: []v1.ServicePort{
						{
							Name:       "port-0",
							Port:       8080,
							TargetPort: intstr.FromInt(80),
							Protocol:   v1.ProtocolUDP,
							NodePort:   30000,
						},
						{
							Name:       "port-1",
							Port:       8081,
							TargetPort: intstr.FromInt(81),
							Protocol:   v1.ProtocolTCP,
							NodePort:   30001,
						},
					},
				},
			},
			globalClient: initFakeClient(globalScheme),
			wantErr:      true,
		},
		{
			name: "External Service with local traffic policy and no health check port",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      shootName,
					Namespace: shootNamespace,
					UID:       shootUUID,
					Annotations: map[string]string{
						lbType: lbExternal,
					},
				},
				Spec: v1.ServiceSpec{
					Type:                  v1.ServiceTypeLoadBalancer,
					ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
					Ports: []v1.ServicePort{
						{
							Name:       "port-0",
							Port:       8080,
							TargetPort: intstr.FromInt(80),
							Protocol:   v1.ProtocolTCP,
							NodePort:   30000,
						},
					},
				},
			},
			globalClient: initFakeClient(globalScheme),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme),
			},
			wantErr: true,
		},
		{
			name:         "External Service - Internal LB FR already exists",
			service:      validExternalService,
			globalClient: initFakeClient(globalScheme, intGlobalObjs...),
			zonalClients: map[string]client.Client{zone: initFakeClient(zonalScheme, intZonalObjs...)},
			wantErr:      false,
			wantGlobalObjs: []client.Object{
				extGlobalObjs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleExternal),
				nil,
				extGlobalObjs[backendServiceIndex].(*globalnetworkingv1.BackendService),
				extGlobalObjs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
				extGlobalObjs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
				extGlobalObjs[pnpIndex].(*globalnetworkingv1.ProjectNetworkPolicy),
				nil,
			},
			wantZonalObjs: []client.Object{
				extZonalObjs[backendIndex].(*networkingv1.Backend),
			},
		},
		{
			name:         "External Service - External LB FR already exists",
			service:      validExternalService,
			globalClient: initFakeClient(globalScheme, extGlobalObjs...),
			zonalClients: map[string]client.Client{zone: initFakeClient(zonalScheme, extZonalObjs...)},
			wantErr:      false,
			wantGlobalObjs: []client.Object{
				extGlobalObjs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleExternal),
				nil,
				extGlobalObjs[backendServiceIndex].(*globalnetworkingv1.BackendService),
				extGlobalObjs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
				extGlobalObjs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
				extGlobalObjs[pnpIndex].(*globalnetworkingv1.ProjectNetworkPolicy),
				nil,
			},
			wantZonalObjs: []client.Object{
				extZonalObjs[backendIndex].(*networkingv1.Backend),
			},
		},
		{
			name: "External Service with Leaf Subnet Annotation - LB FR does not exist",
			service: func() *v1.Service {
				svc := validExternalService.DeepCopy()
				if svc.Annotations == nil {
					svc.Annotations = make(map[string]string)
				}
				svc.Annotations[externalLBIPAddressesAnnotationKey] = lbLeafSubnetName
				return svc
			}(),
			globalClient: initFakeClient(globalScheme, intGlobalObjs[lbLeafSubnetIndex]),
			zonalClients: map[string]client.Client{zone: initFakeClient(zonalScheme)},
			wantErr:      false,
			wantGlobalObjs: func() []client.Object {
				fwdRuleWithCIDRRef := extGlobalObjs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleExternal).DeepCopy()
				fwdRuleWithCIDRRef.Spec.CIDRRef = &networkingv1.CIDRRef{
					Name: lbLeafSubnetName,
				}

				return []client.Object{
					fwdRuleWithCIDRRef,
					nil,
					extGlobalObjs[backendServiceIndex].(*globalnetworkingv1.BackendService),
					extGlobalObjs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
					extGlobalObjs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
					extGlobalObjs[pnpIndex].(*globalnetworkingv1.ProjectNetworkPolicy),
					nil,
				}
			}(),
			wantZonalObjs: []client.Object{
				extZonalObjs[backendIndex].(*networkingv1.Backend),
			},
		},
		{
			name:         "External Service - LB FR does not exist",
			service:      validExternalService,
			globalClient: initFakeClient(globalScheme),
			zonalClients: map[string]client.Client{zone: initFakeClient(zonalScheme)},
			wantErr:      false,
			wantGlobalObjs: []client.Object{
				extGlobalObjs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleExternal),
				nil,
				extGlobalObjs[backendServiceIndex].(*globalnetworkingv1.BackendService),
				extGlobalObjs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
				extGlobalObjs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
				extGlobalObjs[pnpIndex].(*globalnetworkingv1.ProjectNetworkPolicy),
				nil,
			},
			wantZonalObjs: []client.Object{
				extZonalObjs[backendIndex].(*networkingv1.Backend),
			},
		},
		{
			name:         "Internal Service - Internal LB FR already exists",
			service:      validInternalService,
			globalClient: initFakeClient(globalScheme, intGlobalObjs[:5]...),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme, intZonalObjs...),
			},
			wantErr: false,
			wantGlobalObjs: []client.Object{
				nil,
				intGlobalObjs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleInternal),
				intGlobalObjs[backendServiceIndex].(*globalnetworkingv1.BackendService),
				intGlobalObjs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
				intGlobalObjs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
				intGlobalObjs[pnpIndex].(*globalnetworkingv1.ProjectNetworkPolicy),
				nil,
			},
			wantZonalObjs: []client.Object{
				intZonalObjs[backendIndex].(*networkingv1.Backend),
			},
		},
		{
			name:         "Internal Service - External LB FR already exists",
			service:      validInternalService,
			globalClient: initFakeClient(globalScheme, extGlobalObjs...),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme, extZonalObjs...),
			},
			wantErr: false,
			wantGlobalObjs: []client.Object{
				nil,
				intGlobalObjs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleInternal),
				intGlobalObjs[backendServiceIndex].(*globalnetworkingv1.BackendService),
				intGlobalObjs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
				intGlobalObjs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
				intGlobalObjs[pnpIndex].(*globalnetworkingv1.ProjectNetworkPolicy),
				nil,
			},
			wantZonalObjs: []client.Object{
				intZonalObjs[backendIndex].(*networkingv1.Backend),
			},
		},
		{
			name:         "Internal Service - LB FR does not exist",
			service:      validInternalService,
			globalClient: initFakeClient(globalScheme),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme),
			},
			wantErr: false,
			wantGlobalObjs: func() []client.Object {
				ports := servicePortToPnpPorts(validInternalService.Spec.Ports)
				pnp := createExpectedPNP(lbName, lbNamespace, clusterName, string(shootUUID), lbNamespace, ports)
				return []client.Object{
					nil,
					intGlobalObjs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleInternal),
					intGlobalObjs[backendServiceIndex].(*globalnetworkingv1.BackendService),
					intGlobalObjs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
					intGlobalObjs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
					pnp,
					nil,
				}
			}(),
			wantZonalObjs: []client.Object{
				intZonalObjs[backendIndex].(*networkingv1.Backend),
			},
		},
		{
			name: "Internal Service with Branch Subnet Annotation - LB FR does not exist",
			service: func() *v1.Service {
				svc := validInternalService.DeepCopy()
				if svc.Annotations == nil {
					svc.Annotations = make(map[string]string)
				}
				svc.Annotations[internalLBSubnetAnnotationKey] = lbBranchSubnetName
				return svc
			}(),
			globalClient: initFakeClient(globalScheme, intGlobalObjs[lbBranchSubnetIndex]),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme),
			},
			wantErr: false,
			wantGlobalObjs: func() []client.Object {
				fwdRuleWithCIDRRef := intGlobalObjs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleInternal).DeepCopy()
				fwdRuleWithCIDRRef.Spec.CIDRRef = &networkingv1.CIDRRef{
					Name: lbName + subnetSuffix,
				}

				ports := servicePortToPnpPorts(validInternalService.Spec.Ports)
				pnp := createExpectedPNP(lbName, lbNamespace, clusterName, string(shootUUID), lbNamespace, ports)

				return []client.Object{
					nil,
					fwdRuleWithCIDRRef,
					intGlobalObjs[backendServiceIndex].(*globalnetworkingv1.BackendService),
					intGlobalObjs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
					intGlobalObjs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
					pnp,
					intGlobalObjs[subnetIndex].(*ipamglobalv1.Subnet),
				}
			}(),
			wantZonalObjs: []client.Object{
				intZonalObjs[backendIndex].(*networkingv1.Backend),
			},
		},
		{
			name: "Internal Service with Leaf Subnet Annotation - LB FR does not exist",
			service: func() *v1.Service {
				svc := validInternalService.DeepCopy()
				if svc.Annotations == nil {
					svc.Annotations = make(map[string]string)
				}
				svc.Annotations[internalLBSubnetAnnotationKey] = lbLeafSubnetName
				return svc
			}(),
			globalClient: initFakeClient(globalScheme, intGlobalObjs[lbLeafSubnetIndex]),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme),
			},
			wantErr: false,
			wantGlobalObjs: func() []client.Object {
				fwdRuleWithCIDRRef := intGlobalObjs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleInternal).DeepCopy()
				fwdRuleWithCIDRRef.Spec.CIDRRef = &networkingv1.CIDRRef{
					Name: lbLeafSubnetName,
				}

				ports := servicePortToPnpPorts(validInternalService.Spec.Ports)
				pnp := createExpectedPNP(lbName, lbNamespace, clusterName, string(shootUUID), lbNamespace, ports)

				return []client.Object{
					nil,
					fwdRuleWithCIDRRef,
					intGlobalObjs[backendServiceIndex].(*globalnetworkingv1.BackendService),
					intGlobalObjs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
					intGlobalObjs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
					pnp,
					nil,
				}
			}(),
			wantZonalObjs: []client.Object{
				intZonalObjs[backendIndex].(*networkingv1.Backend),
			},
		},
		{
			name:         "External Service with HealthCheckNodePort - LB FR does not exist",
			service:      validExternalServiceWithHCPort,
			globalClient: initFakeClient(globalScheme),
			zonalClients: map[string]client.Client{zone: initFakeClient(zonalScheme)},
			wantErr:      false,
			wantGlobalObjs: []client.Object{
				extGlobalObjsWithHCPort[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleExternal),
				nil,
				extGlobalObjsWithHCPort[backendServiceIndex].(*globalnetworkingv1.BackendService),
				extGlobalObjsWithHCPort[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
				extGlobalObjsWithHCPort[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
				extGlobalObjsWithHCPort[pnpIndex].(*globalnetworkingv1.ProjectNetworkPolicy),
				nil,
			},
			wantZonalObjs: []client.Object{
				extZonalObjsWithHCPort[backendIndex].(*networkingv1.Backend),
			},
		},
		{
			name:         "Internal Service with HealthCheckNodePort - LB FR does not exist",
			service:      validInternalServiceWithHCPort,
			globalClient: initFakeClient(globalScheme),
			zonalClients: map[string]client.Client{zone: initFakeClient(zonalScheme)},
			wantErr:      false,
			wantGlobalObjs: func() []client.Object {
				ports := servicePortToPnpPorts(validInternalServiceWithHCPort.Spec.Ports)
				pnp := createExpectedPNP(lbName, lbNamespace, clusterName, string(shootUUID), lbNamespace, ports)
				return []client.Object{
					nil,
					intGlobalObjsWithHCPort[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleInternal),
					intGlobalObjsWithHCPort[backendServiceIndex].(*globalnetworkingv1.BackendService),
					intGlobalObjsWithHCPort[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
					intGlobalObjsWithHCPort[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
					pnp,
					nil,
				}
			}(),
			wantZonalObjs: []client.Object{
				intZonalObjsWithHCPort[backendIndex].(*networkingv1.Backend),
			},
		},
		{
			name: "Internal Service with allowed projects annotation",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      shootName,
					Namespace: shootNamespace,
					UID:       shootUUID,
					Annotations: map[string]string{
						lbType:                               lbInternal,
						internalLBAllowProjectsAnnotationKey: "proj1,proj2",
					},
				},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeLoadBalancer,
					Ports: []v1.ServicePort{
						{
							Name:       "port-0",
							Port:       8080,
							TargetPort: intstr.FromInt(80),
							Protocol:   v1.ProtocolTCP,
							NodePort:   30000,
						},
					},
				},
			},
			globalClient: initFakeClient(globalScheme),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme),
			},
			wantErr: false,
			wantGlobalObjs: func() []client.Object {
				service := &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						UID: shootUUID,
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Name:       "port-0",
								Port:       8080,
								TargetPort: intstr.FromInt(80),
								Protocol:   v1.ProtocolTCP,
								NodePort:   30000,
							},
						},
					},
				}
				serviceUID := string(service.UID)
				objs, _ := initFakeInternalLBObjs(lbName, lbNamespace, clusterName, service)
				ports := servicePortToPnpPorts(service.Spec.Ports)
				createPNP := func(project string) *globalnetworkingv1.ProjectNetworkPolicy {
					pnpName, _ := getProjectNetworkPolicyName(serviceUID, project)
					return &globalnetworkingv1.ProjectNetworkPolicy{
						ObjectMeta: metav1.ObjectMeta{
							Name:      pnpName,
							Namespace: lbNamespace,
							Labels: map[string]string{
								pnpLabelServiceUID:        serviceUID,
								ilbPnpLabelAllowedProject: project,
							},
						},
						Spec: networkingv1.ProjectNetworkPolicySpec{
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
									Ports: ports,
									From: []networkingv1.ProjectNetworkPolicyPeer{
										{
											Projects: &networkingv1.PolicyProjects{
												MatchNames: []string{project},
											},
										},
									},
								},
							},
						},
					}
				}

				pnp1 := createPNP("test-project")
				pnp2 := createPNP("proj1")
				pnp3 := createPNP("proj2")

				return []client.Object{
					nil,
					objs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleInternal),
					objs[backendServiceIndex].(*globalnetworkingv1.BackendService),
					objs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
					objs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
					pnp1,
					pnp2,
					pnp3,
					nil,
				}
			}(),
			wantZonalObjs: func() []client.Object {
				_, objs := initFakeInternalLBObjs(lbName, lbNamespace, clusterName, &v1.Service{
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Name:       "port-0",
								Port:       8080,
								TargetPort: intstr.FromInt(80),
								Protocol:   v1.ProtocolTCP,
								NodePort:   30000,
							},
						},
					},
				})
				return objs
			}(),
		},
		{
			name: "Internal Service with wildcard allowed projects annotation",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      shootName,
					Namespace: shootNamespace,
					UID:       shootUUID,
					Annotations: map[string]string{
						lbType:                               lbInternal,
						internalLBAllowProjectsAnnotationKey: "*",
					},
				},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeLoadBalancer,
					Ports: []v1.ServicePort{
						{
							Name:       "port-0",
							Port:       8080,
							TargetPort: intstr.FromInt(80),
							Protocol:   v1.ProtocolTCP,
							NodePort:   30000,
						},
					},
				},
			},
			globalClient: initFakeClient(globalScheme),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme),
			},
			wantErr: false,
			wantGlobalObjs: func() []client.Object {
				service := &v1.Service{
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Name:       "port-0",
								Port:       8080,
								TargetPort: intstr.FromInt(80),
								Protocol:   v1.ProtocolTCP,
								NodePort:   30000,
							},
						},
					},
				}
				objs, _ := initFakeInternalLBObjs(lbName, lbNamespace, clusterName, service)
				ports := servicePortToPnpPorts(service.Spec.Ports)
				pnp := createExpectedPNP(lbName, lbNamespace, clusterName, string(shootUUID), "*", ports)

				return []client.Object{
					nil,
					objs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleInternal),
					objs[backendServiceIndex].(*globalnetworkingv1.BackendService),
					objs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
					objs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
					pnp,
					nil,
				}
			}(),
			wantZonalObjs: func() []client.Object {
				_, objs := initFakeInternalLBObjs(lbName, lbNamespace, clusterName, &v1.Service{
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Name:       "port-0",
								Port:       8080,
								TargetPort: intstr.FromInt(80),
								Protocol:   v1.ProtocolTCP,
								NodePort:   30000,
							},
						},
					},
				})
				return objs
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gdc.lb.(*lb).globalMPClient = tt.globalClient
			gdc.lb.(*lb).zonalClients = tt.zonalClients
			_, err := gdc.lb.EnsureLoadBalancer(ctx, clusterName, tt.service, nil)

			// ignore as fwdRule Status cant be populated for tests
			if err != nil && strings.Contains(err.Error(), "extractVIPFromCIDR error") {
				err = nil
			}
			if (tt.wantErr && err == nil) || (!tt.wantErr && err != nil) {
				t.Fatalf("wantErr %v, gotErr %v", tt.wantErr, err)
			}
			if err != nil {
				return
			}
			if err := fetchAndCompareLBObjs(tt.wantGlobalObjs, tt.wantZonalObjs, tt.globalClient, tt.zonalClients); err != nil {
				t.Errorf("EnsureLoadBalancer(%q) returned error %v", tt.name, err)
			}
		})
	}
}

func TestUpdateLoadBalancer(t *testing.T) {
	gdc, globalScheme, zonalScheme, err := initFakeGDC()
	if err != nil {
		t.Fatalf("initFakeGDC throws error - %s", err.Error())
	}

	ctx := context.Background()

	// external objects
	validExternalService := initFakeExternalService()

	updatedExternalService := validExternalService.DeepCopy()
	updatedExternalService.Spec.Ports = []v1.ServicePort{
		{
			Name:       "port-5",
			Port:       8086,
			TargetPort: intstr.FromInt(86),
			Protocol:   v1.ProtocolTCP,
			NodePort:   30006,
		},
	}

	extGlobalObjs, extZonalObjs := initFakeExternalLBObjs(lbName, lbNamespace, clusterName, validExternalService)
	updatedExtGlobalObjs, updatedExtZonalObjs := initFakeExternalLBObjs(lbName, lbNamespace, clusterName, updatedExternalService)

	// internal objects
	validInternalService := initFakeInternalService()

	updatedInternalService := validInternalService.DeepCopy()
	updatedInternalService.Spec.Ports = []v1.ServicePort{
		{
			Name:       "port-5",
			Port:       8085,
			TargetPort: intstr.FromInt(85),
			Protocol:   v1.ProtocolTCP,
			NodePort:   30005,
		},
	}

	intGlobalObjs, intZonalObjs := initFakeInternalLBObjs(lbName, lbNamespace, clusterName, validInternalService)
	updatedIntGlobalObjs, updatedIntZonalObjs := initFakeInternalLBObjs(lbName, lbNamespace, clusterName, updatedInternalService)

	// invalid objects
	invalidServiceAnnotation := validExternalService.DeepCopy()
	invalidServiceAnnotation.Annotations[lbType] = "invalid"

	tests := []struct {
		name           string
		service        *v1.Service
		globalClient   client.Client
		zonalClients   map[string]client.Client
		wantErr        error
		wantGlobalObjs []client.Object
		wantZonalObjs  []client.Object
	}{
		{
			name:         "Invalid Service - External FR exists",
			service:      invalidServiceAnnotation,
			globalClient: initFakeClient(globalScheme, extGlobalObjs...),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme, extZonalObjs...),
			},
			wantGlobalObjs: []client.Object{
				extGlobalObjs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleExternal),
				nil,
				extGlobalObjs[backendServiceIndex].(*globalnetworkingv1.BackendService),
				extGlobalObjs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
				extGlobalObjs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
				extGlobalObjs[pnpIndex].(*globalnetworkingv1.ProjectNetworkPolicy),
			},
			wantZonalObjs: []client.Object{
				extZonalObjs[backendIndex].(*networkingv1.Backend),
			},
			wantErr: fmt.Errorf("LBType %q not supported. Use values %s or %s for service %q/%q", invalidServiceAnnotation.Annotations[lbType], lbInternal, lbExternal, invalidServiceAnnotation.Namespace, invalidServiceAnnotation.Name),
		},
		{
			name:         "Internal Service - External FR exists - no updates",
			service:      validInternalService,
			globalClient: initFakeClient(globalScheme, extGlobalObjs...),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme, extZonalObjs...),
			},
			wantGlobalObjs: []client.Object{
				extGlobalObjs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleExternal),
				nil,
				extGlobalObjs[backendServiceIndex].(*globalnetworkingv1.BackendService),
				extGlobalObjs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
				extGlobalObjs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
				extGlobalObjs[pnpIndex].(*globalnetworkingv1.ProjectNetworkPolicy),
			},
			wantZonalObjs: []client.Object{
				extZonalObjs[backendIndex].(*networkingv1.Backend),
			},
			wantErr: nil,
		},
		{
			name:         "Internal Service - Internal FR exists - no updates",
			service:      validInternalService,
			globalClient: initFakeClient(globalScheme, intGlobalObjs...),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme, intZonalObjs...),
			},
			wantGlobalObjs: []client.Object{
				nil,
				intGlobalObjs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleInternal),
				intGlobalObjs[backendServiceIndex].(*globalnetworkingv1.BackendService),
				intGlobalObjs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
				intGlobalObjs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
				intGlobalObjs[pnpIndex].(*globalnetworkingv1.ProjectNetworkPolicy),
			},
			wantZonalObjs: []client.Object{
				intZonalObjs[backendIndex].(*networkingv1.Backend),
			},
			wantErr: nil,
		},
		{
			name:         "Internal Service - Internal FR exists - port updates",
			service:      updatedInternalService,
			globalClient: initFakeClient(globalScheme, intGlobalObjs...),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme, intZonalObjs...),
			},
			wantGlobalObjs: []client.Object{
				nil,
				updatedIntGlobalObjs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleInternal),
				updatedIntGlobalObjs[backendServiceIndex].(*globalnetworkingv1.BackendService),
				updatedIntGlobalObjs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
				updatedIntGlobalObjs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
				updatedIntGlobalObjs[pnpIndex].(*globalnetworkingv1.ProjectNetworkPolicy),
			},
			wantZonalObjs: []client.Object{
				updatedIntZonalObjs[backendIndex].(*networkingv1.Backend),
			},
			wantErr: nil,
		},
		{
			name:         "External Service - Internal FR exists - no updates",
			service:      validExternalService,
			globalClient: initFakeClient(globalScheme, intGlobalObjs...),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme, intZonalObjs...),
			},
			wantGlobalObjs: []client.Object{
				nil,
				intGlobalObjs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleInternal),
				intGlobalObjs[backendServiceIndex].(*globalnetworkingv1.BackendService),
				intGlobalObjs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
				intGlobalObjs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
				intGlobalObjs[pnpIndex].(*globalnetworkingv1.ProjectNetworkPolicy),
			},
			wantZonalObjs: []client.Object{
				intZonalObjs[backendIndex].(*networkingv1.Backend),
			},
			wantErr: nil,
		},
		{
			name:         "External Service - External FR exists - no updates",
			service:      validExternalService,
			globalClient: initFakeClient(globalScheme, extGlobalObjs...),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme, extZonalObjs...),
			},
			wantGlobalObjs: []client.Object{
				extGlobalObjs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleExternal),
				nil,
				extGlobalObjs[backendServiceIndex].(*globalnetworkingv1.BackendService),
				extGlobalObjs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
				extGlobalObjs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
				extGlobalObjs[pnpIndex].(*globalnetworkingv1.ProjectNetworkPolicy),
			},
			wantZonalObjs: []client.Object{
				extZonalObjs[backendIndex].(*networkingv1.Backend),
			},
			wantErr: nil,
		},
		{
			name:         "External Service - External FR exists - port updates",
			service:      updatedExternalService,
			globalClient: initFakeClient(globalScheme, extGlobalObjs...),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme, extZonalObjs...),
			},
			wantGlobalObjs: []client.Object{
				updatedExtGlobalObjs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleExternal),
				nil,
				updatedExtGlobalObjs[backendServiceIndex].(*globalnetworkingv1.BackendService),
				updatedExtGlobalObjs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
				updatedExtGlobalObjs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
				updatedExtGlobalObjs[pnpIndex].(*globalnetworkingv1.ProjectNetworkPolicy),
			},
			wantZonalObjs: []client.Object{
				updatedExtZonalObjs[backendIndex].(*networkingv1.Backend),
			},
			wantErr: nil,
		},
		{
			name: "Internal Service - CIDR Change",
			service: func() *v1.Service {
				svc := validInternalService.DeepCopy()
				svc.Annotations[internalLBSubnetAnnotationKey] = "new-subnet"
				return svc
			}(),
			globalClient: initFakeClient(globalScheme, append(intGlobalObjs, &ipamglobalv1.Subnet{
				ObjectMeta: metav1.ObjectMeta{Name: "new-subnet", Namespace: lbNamespace},
				Spec:       ipamglobalv1.SubnetSpec{Type: ipamv1.Leaf},
			})...),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme, intZonalObjs...),
			},
			wantErr: fmt.Errorf("createOrUpdateIntFwdRule retryable error: recreating ForwardingRuleInternal due to CIDRRef change"),
		},
		{
			name: "External Service - CIDR Change",
			service: func() *v1.Service {
				svc := validExternalService.DeepCopy()
				svc.Annotations[externalLBIPAddressesAnnotationKey] = "new-subnet"
				return svc
			}(),
			globalClient: initFakeClient(globalScheme, append(extGlobalObjs, &ipamglobalv1.Subnet{
				ObjectMeta: metav1.ObjectMeta{Name: "new-subnet", Namespace: lbNamespace},
				Spec:       ipamglobalv1.SubnetSpec{Type: ipamv1.Leaf},
			})...),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme, extZonalObjs...),
			},
			wantErr: fmt.Errorf("createOrUpdateExtFwdRule retryable error: recreating ForwardingRuleExternal due to CIDRRef change"),
		},
		{
			name: "Internal Service - Change from specific project to wildcard PNP",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      shootName,
					Namespace: shootNamespace,
					UID:       shootUUID,
					Annotations: map[string]string{
						lbType:                               lbInternal,
						internalLBAllowProjectsAnnotationKey: "*",
					},
				},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeLoadBalancer,
					Ports: []v1.ServicePort{
						{
							Name:       "port-0",
							Port:       8080,
							TargetPort: intstr.FromInt(80),
							Protocol:   v1.ProtocolTCP,
							NodePort:   30000,
						},
					},
				},
			},
			globalClient: func() client.Client {
				objs, _ := initFakeInternalLBObjs(lbName, lbNamespace, clusterName, validInternalService)
				return initFakeClient(globalScheme, objs...)
			}(),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme, intZonalObjs...),
			},
			wantGlobalObjs: func() []client.Object {
				service := &v1.Service{
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Name:       "port-0",
								Port:       8080,
								TargetPort: intstr.FromInt(80),
								Protocol:   v1.ProtocolTCP,
								NodePort:   30000,
							},
						},
					},
				}
				objs, _ := initFakeInternalLBObjs(lbName, lbNamespace, clusterName, service)
				ports := servicePortToPnpPorts(service.Spec.Ports)
				pnpWildcard := createExpectedPNP(lbName, lbNamespace, clusterName, string(shootUUID), "*", ports)

				return []client.Object{
					nil,
					objs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleInternal),
					objs[backendServiceIndex].(*globalnetworkingv1.BackendService),
					objs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
					objs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
					pnpWildcard,
				}
			}(),
			wantZonalObjs: intZonalObjs,
			wantErr:       nil,
		},
		{
			name: "Internal Service - Change from wildcard to specific project PNP",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      shootName,
					Namespace: shootNamespace,
					UID:       shootUUID,
					Annotations: map[string]string{
						lbType:                               lbInternal,
						internalLBAllowProjectsAnnotationKey: "proj1",
					},
				},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeLoadBalancer,
					Ports: []v1.ServicePort{
						{
							Name:       "port-0",
							Port:       8080,
							TargetPort: intstr.FromInt(80),
							Protocol:   v1.ProtocolTCP,
							NodePort:   30000,
						},
					},
				},
			},
			globalClient: func() client.Client {
				service := initFakeInternalService()
				service.Annotations[internalLBAllowProjectsAnnotationKey] = "*"
				objs, _ := initFakeInternalLBObjs(lbName, lbNamespace, clusterName, service)
				ports := servicePortToPnpPorts(service.Spec.Ports)
				pnpWildcard := createExpectedPNP(lbName, lbNamespace, clusterName, string(shootUUID), "*", ports)
				// Replace project-specific PNP with wildcard PNP
				objs[pnpIndex] = pnpWildcard
				return initFakeClient(globalScheme, objs...)
			}(),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme, intZonalObjs...),
			},
			wantGlobalObjs: func() []client.Object {
				service := &v1.Service{
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Name:       "port-0",
								Port:       8080,
								TargetPort: intstr.FromInt(80),
								Protocol:   v1.ProtocolTCP,
								NodePort:   30000,
							},
						},
					},
				}
				objs, _ := initFakeInternalLBObjs(lbName, lbNamespace, clusterName, service)
				ports := servicePortToPnpPorts(service.Spec.Ports)
				pnpSelf := createExpectedPNP(lbName, lbNamespace, clusterName, string(shootUUID), lbNamespace, ports)
				pnpProj1 := createExpectedPNP(lbName, lbNamespace, clusterName, string(shootUUID), "proj1", ports)

				return []client.Object{
					nil,
					objs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleInternal),
					objs[backendServiceIndex].(*globalnetworkingv1.BackendService),
					objs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
					objs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
					pnpSelf,
					pnpProj1,
				}
			}(),
			wantZonalObjs: intZonalObjs,
			wantErr:       nil,
		},
		{
			name: "Internal Service - Update project specific list",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      shootName,
					Namespace: shootNamespace,
					UID:       shootUUID,
					Annotations: map[string]string{
						lbType:                               lbInternal,
						internalLBAllowProjectsAnnotationKey: "proj2",
					},
				},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeLoadBalancer,
					Ports: []v1.ServicePort{
						{
							Name:       "port-0",
							Port:       8080,
							TargetPort: intstr.FromInt(80),
							Protocol:   v1.ProtocolTCP,
							NodePort:   30000,
						},
					},
				},
			},
			globalClient: func() client.Client {
				service := initFakeInternalService()
				service.Annotations[internalLBAllowProjectsAnnotationKey] = "proj1"
				objs, _ := initFakeInternalLBObjs(lbName, lbNamespace, clusterName, service)
				ports := servicePortToPnpPorts(service.Spec.Ports)
				pnpSelf := createExpectedPNP(lbName, lbNamespace, clusterName, string(shootUUID), lbNamespace, ports)
				pnpProj1 := createExpectedPNP(lbName, lbNamespace, clusterName, string(shootUUID), "proj1", ports)
				// Initial state: self and proj1
				return initFakeClient(globalScheme, objs[0], objs[1], objs[2], objs[3], pnpSelf, pnpProj1)
			}(),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme, intZonalObjs...),
			},
			wantGlobalObjs: func() []client.Object {
				service := &v1.Service{
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Name:       "port-0",
								Port:       8080,
								TargetPort: intstr.FromInt(80),
								Protocol:   v1.ProtocolTCP,
								NodePort:   30000,
							},
						},
					},
				}
				objs, _ := initFakeInternalLBObjs(lbName, lbNamespace, clusterName, service)
				ports := servicePortToPnpPorts(service.Spec.Ports)
				pnpSelf := createExpectedPNP(lbName, lbNamespace, clusterName, string(shootUUID), lbNamespace, ports)
				pnpProj2 := createExpectedPNP(lbName, lbNamespace, clusterName, string(shootUUID), "proj2", ports)

				return []client.Object{
					nil,
					objs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleInternal),
					objs[backendServiceIndex].(*globalnetworkingv1.BackendService),
					objs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
					objs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
					pnpSelf,
					pnpProj2,
				}
			}(),
			wantZonalObjs: intZonalObjs,
			wantErr:       nil,
		},
		{
			name:    "Internal Service - Delete legacy single PNP on update",
			service: validInternalService,
			globalClient: func() client.Client {
				objs, _ := initFakeInternalLBObjs(lbName, lbNamespace, clusterName, validInternalService)
				// Add a legacy PNP with the same name as lbName
				legacyPNP := &globalnetworkingv1.ProjectNetworkPolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name:      lbName,
						Namespace: lbNamespace,
					},
				}
				return initFakeClient(globalScheme, append(objs, legacyPNP)...)
			}(),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme, intZonalObjs...),
			},
			wantGlobalObjs: func() []client.Object {
				objs, _ := initFakeInternalLBObjs(lbName, lbNamespace, clusterName, validInternalService)
				ports := servicePortToPnpPorts(validInternalService.Spec.Ports)
				pnpSelf := createExpectedPNP(lbName, lbNamespace, clusterName, string(shootUUID), lbNamespace, ports)

				return []client.Object{
					nil,
					objs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleInternal),
					objs[backendServiceIndex].(*globalnetworkingv1.BackendService),
					objs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
					objs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
					pnpSelf,
				}
			}(),
			wantZonalObjs: intZonalObjs,
			wantErr:       nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gdc.lb.(*lb).globalMPClient = tt.globalClient
			gdc.lb.(*lb).zonalClients = tt.zonalClients
			err := gdc.lb.UpdateLoadBalancer(ctx, clusterName, tt.service, nil)

			// ignore as fwdRule Status cant be populated for tests
			if err != nil && strings.Contains(err.Error(), "extractVIPFromCIDR error") {
				err = nil
			}
			if (tt.wantErr != nil && err == nil) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("wantErr %v, gotErr %v", tt.wantErr, err)
			}
			if err != nil && !strings.Contains(err.Error(), tt.wantErr.Error()) {
				t.Fatalf("wantErr %v, gotErr %v", tt.wantErr, err)
			}
			if err != nil {
				return
			}
			if err := fetchAndCompareLBObjs(tt.wantGlobalObjs, tt.wantZonalObjs, tt.globalClient, tt.zonalClients); err != nil {
				t.Errorf("UpdateLoadBalancer(%q) returned error %v", tt.name, err)
			}
		})
	}
}

func TestEnsureLoadBalancerDeleted(t *testing.T) {
	gdc, globalScheme, zonalScheme, err := initFakeGDC()
	if err != nil {
		t.Fatalf("initFakeGDC throws error - %s", err.Error())
	}

	ctx := context.Background()

	// useful test objects
	validExternalService := initFakeExternalService()
	validInternalService := initFakeInternalService()

	invalidServiceAnnotation := validExternalService.DeepCopy()
	invalidServiceAnnotation.Annotations[lbType] = "invalid"

	extGlobalObjs, extZonalObjs := initFakeExternalLBObjs(lbName, lbNamespace, clusterName, validExternalService)
	intGlobalObjs, intZonalObjs := initFakeInternalLBObjs(lbName, lbNamespace, clusterName, validInternalService)

	tests := []struct {
		name           string
		service        *v1.Service
		globalClient   client.Client
		zonalClients   map[string]client.Client
		wantErr        error
		wantGlobalObjs []client.Object
		wantZonalObjs  []client.Object
	}{
		{
			name:         "Invalid Service - External FR exists - no deletion",
			service:      invalidServiceAnnotation,
			globalClient: initFakeClient(globalScheme, extGlobalObjs...),
			zonalClients: map[string]client.Client{zone: initFakeClient(zonalScheme, extZonalObjs...)},
			wantGlobalObjs: []client.Object{
				extGlobalObjs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleExternal),
				nil,
				extGlobalObjs[backendServiceIndex].(*globalnetworkingv1.BackendService),
				extGlobalObjs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
				extGlobalObjs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
				extGlobalObjs[pnpIndex].(*globalnetworkingv1.ProjectNetworkPolicy),
			},
			wantZonalObjs: []client.Object{
				extZonalObjs[backendIndex].(*networkingv1.Backend),
			},
			wantErr: fmt.Errorf("LBType %q not supported. Use values %s or %s for service %q/%q", invalidServiceAnnotation.Annotations[lbType], lbInternal, lbExternal, invalidServiceAnnotation.Namespace, invalidServiceAnnotation.Name),
		},
		{
			name:         "Internal Service - External FR exists - no deletion",
			service:      validInternalService,
			globalClient: initFakeClient(globalScheme, extGlobalObjs...),
			zonalClients: map[string]client.Client{zone: initFakeClient(zonalScheme, extZonalObjs...)},
			wantGlobalObjs: []client.Object{
				extGlobalObjs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleExternal),
				nil,
				extGlobalObjs[backendServiceIndex].(*globalnetworkingv1.BackendService),
				extGlobalObjs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
				extGlobalObjs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
				extGlobalObjs[pnpIndex].(*globalnetworkingv1.ProjectNetworkPolicy),
			},
			wantZonalObjs: []client.Object{
				extZonalObjs[backendIndex].(*networkingv1.Backend),
			},
			wantErr: nil,
		},
		{
			name:         "External Service - Internal FR exists - no deletion",
			service:      validExternalService,
			globalClient: initFakeClient(globalScheme, intGlobalObjs...),
			zonalClients: map[string]client.Client{zone: initFakeClient(zonalScheme, intZonalObjs...)},
			wantGlobalObjs: []client.Object{
				nil,
				intGlobalObjs[forwardingRuleIndex].(*globalnetworkingv1.ForwardingRuleInternal),
				intGlobalObjs[backendServiceIndex].(*globalnetworkingv1.BackendService),
				intGlobalObjs[backendServicePolicyIndex].(*globalnetworkingv1.BackendServicePolicy),
				intGlobalObjs[healthCheckIndex].(*globalnetworkingv1.HealthCheck),
				intGlobalObjs[pnpIndex].(*globalnetworkingv1.ProjectNetworkPolicy),
			},
			wantZonalObjs: []client.Object{
				intZonalObjs[backendIndex].(*networkingv1.Backend),
			},
			wantErr: nil,
		},
		{
			name:         "External Service - External FR exists - delete external LB objs",
			service:      validExternalService,
			globalClient: initFakeClient(globalScheme, extGlobalObjs...),
			zonalClients: map[string]client.Client{zone: initFakeClient(zonalScheme, extZonalObjs...)},
			wantGlobalObjs: []client.Object{
				nil, nil, nil, nil, nil, nil,
			},
			wantZonalObjs: []client.Object{
				nil,
			},
			wantErr: nil,
		},
		{
			name:         "Internal Service - Internal FR exists - delete internal LB objs",
			service:      validInternalService,
			globalClient: initFakeClient(globalScheme, intGlobalObjs...),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme, intZonalObjs...),
			},
			wantGlobalObjs: []client.Object{
				nil, nil, nil, nil, nil, nil,
			},
			wantZonalObjs: []client.Object{
				nil,
			},
			wantErr: nil,
		},
		{
			name: "Internal Service with Subnet notation - Internal FR exists - delete internal LB objs",
			service: func() *v1.Service {
				svc := validInternalService.DeepCopy()
				if svc.Annotations == nil {
					svc.Annotations = make(map[string]string)
				}
				svc.Annotations[internalLBSubnetAnnotationKey] = lbBranchSubnetName
				return svc
			}(),
			globalClient: initFakeClient(globalScheme, intGlobalObjs...),
			zonalClients: map[string]client.Client{
				zone: initFakeClient(zonalScheme, intZonalObjs...),
			},
			wantGlobalObjs: []client.Object{
				nil, nil, nil, nil, nil, nil, nil,
			},
			wantZonalObjs: []client.Object{
				nil,
			},
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gdc.lb.(*lb).globalMPClient = tt.globalClient
			gdc.lb.(*lb).zonalClients = tt.zonalClients
			err := gdc.lb.EnsureLoadBalancerDeleted(ctx, clusterName, tt.service)

			// ignore as fwdRule Status cant be populated for tests
			if err != nil && strings.Contains(err.Error(), "extractVIPFromCIDR error") {
				t.Logf("Tests dont have IP for forwarding rule. Got error %v but ignoring due to test", err.Error())
				err = nil
			}
			if (tt.wantErr != nil && err == nil) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("wantErr %v, gotErr %v", tt.wantErr, err)
			}
			if err != nil && !strings.Contains(err.Error(), tt.wantErr.Error()) {
				t.Fatalf("wantErr %v, gotErr %v", tt.wantErr, err)
			}

			if err := fetchAndCompareLBObjs(tt.wantGlobalObjs, tt.wantZonalObjs, tt.globalClient, tt.zonalClients); err != nil {
				t.Errorf("EnsureLoadBalancerDeleted(%q) returned error %v", tt.name, err)
			}
		})
	}
}

func TestConfigureFRPorts(t *testing.T) {
	externalLBService := initFakeExternalService()
	svcPorts := externalLBService.Spec.Ports
	frPorts := configureFRPorts(svcPorts)

	if len(frPorts) != len(svcPorts) {
		t.Fatalf("Incorrect ports len got = %v, want = %v", len(frPorts), len(svcPorts))
	}
	for index := range frPorts {
		if frPorts[index].Port != svcPorts[index].Port {
			t.Errorf("Incorrect Port for index %d got = %d, want = %d", index, svcPorts[index].Port, frPorts[index].Port)
		}
		if *(frPorts[index].Protocol) != svcPorts[index].Protocol {
			t.Errorf("Incorrect Port Protocol for index %d got = %v, want = %v", index, ptr.To(svcPorts[index].Protocol), frPorts[index].Protocol)
		}
	}
}

func TestConfigureBSTargetPorts(t *testing.T) {
	externalLBService := initFakeExternalService()
	svcPorts := externalLBService.Spec.Ports
	frTargetPorts := configureBSTargetPorts(svcPorts)
	if len(frTargetPorts) != len(svcPorts) {
		t.Fatalf("Incorrect ports len got = %v, want = %v", len(frTargetPorts), len(svcPorts))
	}
	for index := range frTargetPorts {
		if frTargetPorts[index].Port.Port != svcPorts[index].Port {
			t.Errorf("Incorrect Port for index %d got = %d, want = %d", index, svcPorts[index].Port, frTargetPorts[index].Port.Port)
		}
		if *(frTargetPorts[index].Protocol) != svcPorts[index].Protocol {
			t.Errorf("Incorrect Port Protocol for index %d got = %v, want = %v", index, svcPorts[index].Protocol, frTargetPorts[index].Protocol)
		}
		if frTargetPorts[index].TargetPort != svcPorts[index].NodePort {
			t.Errorf("Incorrect TargetPort for index %d got = %v, want = %v", index, svcPorts[index].NodePort, frTargetPorts[index].TargetPort)
		}
	}
}

func TestCreateOrUpdateBackendService(t *testing.T) {
	ctx := context.Background()
	mpScheme, err := globalManagementPlaneScheme()
	if err != nil {
		t.Fatalf("managementPlaneScheme throws error - %s", err.Error())
	}

	ignoreResourceField := cmpopts.IgnoreFields(metav1.ObjectMeta{}, "ResourceVersion")

	externalSvc := initFakeExternalService()
	backendService := globalnetworkingv1.BackendService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lbName + backendServiceRefSuffix,
			Namespace: lbNamespace,
			Labels: map[string]string{
				besSelectorLabel: lbName + backendServiceRefSuffix,
			},
		},
		Spec: globalnetworkingv1.BackendServiceSpec{
			TargetPorts: configureBSTargetPorts(externalSvc.Spec.Ports),
			BackendRefs: []globalnetworkingv1.BackendRef{
				{
					Zone: "zone1",
					Name: lbName + backendSuffix,
				},
			},
			HealthCheckName: ptr.To(lbName + healthCheckSuffix),
		},
	}

	updatedSvc := externalSvc.DeepCopy()
	updatedSvc.Spec.Ports = []v1.ServicePort{
		{
			Name:       "port-2",
			Port:       8082,
			TargetPort: intstr.FromInt(82),
			Protocol:   v1.ProtocolTCP,
			NodePort:   30002,
		},
	}
	updatedBackendService := backendService.DeepCopy()
	updatedBackendService.Spec.TargetPorts = configureBSTargetPorts(updatedSvc.Spec.Ports)
	updatedBackendService.Spec.BackendRefs = []globalnetworkingv1.BackendRef{
		{
			Zone: "zone2",
			Name: lbName + backendSuffix,
		},
	}

	tests := []struct {
		name           string
		globalMPClient client.Client
		zonalClients   map[string]client.Client
		service        v1.Service
		want           globalnetworkingv1.BackendService
	}{
		{
			name:           "BackendService does not exist",
			globalMPClient: fake.NewClientBuilder().WithScheme(mpScheme).Build(),
			zonalClients: map[string]client.Client{
				"zone1": fake.NewClientBuilder().WithScheme(mpScheme).Build(),
			},
			service: *externalSvc,
			want:    backendService,
		},
		{
			name:           "BackendService exists - no changes",
			globalMPClient: fake.NewClientBuilder().WithScheme(mpScheme).WithObjects([]client.Object{&backendService}...).Build(),
			zonalClients: map[string]client.Client{
				"zone1": fake.NewClientBuilder().WithScheme(mpScheme).Build(),
			},
			service: *externalSvc,
			want:    backendService,
		},
		{
			name:           "BackendService exists - updated ports",
			globalMPClient: fake.NewClientBuilder().WithScheme(mpScheme).WithObjects([]client.Object{&backendService}...).Build(),
			zonalClients: map[string]client.Client{
				"zone2": fake.NewClientBuilder().WithScheme(mpScheme).Build(),
			},
			service: *updatedSvc,
			want:    *updatedBackendService,
		},
	}

	gdc, _, _, _ := initFakeGDC()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gdcLb := gdc.lb.(*lb)
			gdcLb.globalMPClient = tt.globalMPClient
			gdcLb.zonalClients = tt.zonalClients
			err := gdcLb.createOrUpdateBackendService(ctx, lbName, lbNamespace, &tt.service)
			if err != nil {
				t.Fatalf("createOrUpdateBackendService(%q) returned error %v", tt.name, err)
			}
			got := &globalnetworkingv1.BackendService{}
			if err := tt.globalMPClient.Get(ctx, types.NamespacedName{
				Name:      tt.want.Name,
				Namespace: tt.want.Namespace,
			}, got); err != nil {
				t.Fatalf("Error fetching from client %v", err)
			}
			if diff := cmp.Diff(tt.want, *got, ignoreResourceField); diff != "" {
				t.Errorf("returned unexpected diff (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCreateOrUpdateHealthCheck(t *testing.T) {
	gdc, globalScheme, _, err := initFakeGDC()
	if err != nil {
		t.Fatalf("initFakeGDC throws error - %s", err.Error())
	}
	ctx := context.Background()
	ignoreResourceField := cmpopts.IgnoreFields(metav1.ObjectMeta{}, "ResourceVersion")

	// Service that will result in HTTP health check
	externalSvc := initFakeExternalService()

	// Existing health check with TCP prober
	tcpHealthCheck := &globalnetworkingv1.HealthCheck{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lbName + healthCheckSuffix,
			Namespace: lbNamespace,
		},
		Spec: networkingv1.HealthCheckSpec{
			ProbeHandler: networkingv1.ProbeHandler{
				TCPHealthCheck: &networkingv1.TCPHealthCheck{
					Port: 8080,
				},
			},
		},
	}

	// Desired health check with HTTP prober
	httpHealthCheck := &globalnetworkingv1.HealthCheck{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lbName + healthCheckSuffix,
			Namespace: lbNamespace,
		},
		Spec: networkingv1.HealthCheckSpec{
			ProbeHandler: networkingv1.ProbeHandler{
				HTTPHealthCheck: &networkingv1.HTTPHealthCheck{
					Port:        defaultHealthzPort,
					RequestPath: "/healthz",
				},
			},
		},
	}

	tests := []struct {
		name         string
		service      *v1.Service
		existingHC   *globalnetworkingv1.HealthCheck
		globalClient client.Client
		wantErr      bool
		wantErrMsg   string
		wantHC       *globalnetworkingv1.HealthCheck
	}{
		{
			name:         "HealthCheck does not exist",
			service:      externalSvc,
			globalClient: initFakeClient(globalScheme),
			wantErr:      false,
			wantHC:       httpHealthCheck,
		},
		{
			name:         "HealthCheck exists, no change",
			service:      externalSvc,
			existingHC:   httpHealthCheck,
			globalClient: initFakeClient(globalScheme, httpHealthCheck),
			wantErr:      false,
			wantHC:       httpHealthCheck,
		},
		{
			name:         "HealthCheck exists, prober type changed from TCP to HTTP",
			service:      externalSvc,
			existingHC:   tcpHealthCheck,
			globalClient: initFakeClient(globalScheme, tcpHealthCheck),
			wantErr:      true,
			wantErrMsg:   "createOrUpdateHealthCheck: recreating HealthCheck due to ProbeHandler type change",
			wantHC:       nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gdc.lb.(*lb).globalMPClient = tt.globalClient
			err := gdc.lb.(*lb).createOrUpdateHealthCheck(ctx, lbName, lbNamespace, tt.service)

			if (tt.wantErr && err == nil) || (!tt.wantErr && err != nil) {
				t.Fatalf("createOrUpdateHealthCheck() error = %v, wantErr %v", err, tt.wantErr)
			}

			if tt.wantErr && err != nil && tt.wantErrMsg != "" && !strings.Contains(err.Error(), tt.wantErrMsg) {
				t.Fatalf("createOrUpdateHealthCheck() error = %v, wantErrMsg %q", err, tt.wantErrMsg)
			}

			gotHC := &globalnetworkingv1.HealthCheck{}
			getErr := tt.globalClient.Get(ctx, types.NamespacedName{Name: lbName + healthCheckSuffix, Namespace: lbNamespace}, gotHC)

			if tt.wantHC == nil {
				if !k8serrors.IsNotFound(getErr) {
					t.Errorf("Expected HealthCheck to be deleted, but it was found.")
				}
			} else {
				if getErr != nil {
					t.Fatalf("Error fetching HealthCheck from client: %v", getErr)
				}
				if diff := cmp.Diff(tt.wantHC.Spec, gotHC.Spec, ignoreResourceField); diff != "" {
					t.Errorf("HealthCheck returned unexpected diff (-want +got):\n%s", diff)
				}
			}
		})
	}
}

func TestEnsureLeafSubnetForService(t *testing.T) {
	testCases := []struct {
		name               string
		service            *v1.Service
		existingObjs       []client.Object
		expectedSubnetName string
		expectErr          bool
		validate           func(*testing.T, client.Client)
	}{
		{
			name: "error when both internal and external annotations are present",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "test-ns",
					Annotations: map[string]string{
						internalLBSubnetAnnotationKey:      "internal-subnet",
						externalLBIPAddressesAnnotationKey: "external-subnet",
					},
				},
			},
			expectErr: true,
		},
		{
			name: "no annotations present",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-service",
					Namespace:   "test-ns",
					Annotations: map[string]string{},
				},
			},
			expectedSubnetName: "",
		},
		{
			name: "only internal annotation, parent is leaf",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "test-ns",
					Annotations: map[string]string{
						internalLBSubnetAnnotationKey: "parent-subnet",
					},
				},
			},
			existingObjs: []client.Object{
				&ipamglobalv1.Subnet{
					ObjectMeta: metav1.ObjectMeta{Name: "parent-subnet", Namespace: "test-ns"},
					Spec:       ipamglobalv1.SubnetSpec{Type: ipamv1.Leaf},
				},
			},
			expectedSubnetName: "parent-subnet",
		},
		{
			name: "only external annotation, create new leaf subnet",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "test-ns",
					Annotations: map[string]string{
						externalLBIPAddressesAnnotationKey: "parent-subnet",
					},
				},
			},
			existingObjs: []client.Object{
				&ipamglobalv1.Subnet{
					ObjectMeta: metav1.ObjectMeta{Name: "parent-subnet", Namespace: "test-ns"},
					Spec:       ipamglobalv1.SubnetSpec{Type: ipamv1.Branch},
				},
			},
			expectedSubnetName: "test-service" + subnetSuffix,
			validate: func(t *testing.T, c client.Client) {
				subnet := &ipamglobalv1.Subnet{}
				err := c.Get(context.Background(), types.NamespacedName{Name: "test-service" + subnetSuffix, Namespace: "test-ns"}, subnet)
				if err != nil {
					t.Errorf("expected subnet to be created, but got error: %v", err)
				}
				if subnet.Spec.ParentReference.Name != "parent-subnet" {
					t.Errorf("expected parent reference to be 'parent-subnet', got %q", subnet.Spec.ParentReference.Name)
				}
			},
		},
		{
			name: "parent subnet changes, delete and recreate",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "test-ns",
					Annotations: map[string]string{
						internalLBSubnetAnnotationKey: "new-parent-subnet",
					},
				},
			},
			existingObjs: []client.Object{
				&ipamglobalv1.Subnet{
					ObjectMeta: metav1.ObjectMeta{Name: "new-parent-subnet", Namespace: "test-ns"},
					Spec:       ipamglobalv1.SubnetSpec{Type: ipamv1.Branch},
				},
				&ipamglobalv1.Subnet{
					ObjectMeta: metav1.ObjectMeta{Name: "test-service" + subnetSuffix, Namespace: "test-ns"},
					Spec: ipamglobalv1.SubnetSpec{
						Type: ipamv1.Leaf,
						ParentReference: &ipamv1.SubnetReference{
							Name:      "old-parent-subnet",
							Namespace: ptr.To("test-ns"),
						},
					},
				},
			},
			expectErr: true,
			validate: func(t *testing.T, c client.Client) {
				subnet := &ipamglobalv1.Subnet{}
				err := c.Get(context.Background(), types.NamespacedName{Name: "test-service" + subnetSuffix, Namespace: "test-ns"}, subnet)
				if !k8serrors.IsNotFound(err) {
					t.Errorf("expected subnet to be deleted, but it still exists")
				}
			},
		},
		{
			name: "parent subnet does not change",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "test-ns",
					Annotations: map[string]string{
						internalLBSubnetAnnotationKey: "parent-subnet",
					},
				},
			},
			existingObjs: []client.Object{
				&ipamglobalv1.Subnet{
					ObjectMeta: metav1.ObjectMeta{Name: "parent-subnet", Namespace: "test-ns"},
					Spec:       ipamglobalv1.SubnetSpec{Type: ipamv1.Branch},
				},
				&ipamglobalv1.Subnet{
					ObjectMeta: metav1.ObjectMeta{Name: "test-service" + subnetSuffix, Namespace: "test-ns"},
					Spec: ipamglobalv1.SubnetSpec{
						Type: ipamv1.Leaf,
						ParentReference: &ipamv1.SubnetReference{
							Name:      "parent-subnet",
							Namespace: ptr.To("test-ns"),
						},
					},
				},
			},
			expectedSubnetName: "test-service" + subnetSuffix,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := ipamglobalv1.AddToScheme(scheme); err != nil {
				t.Fatalf("failed to add ipamglobalv1 to scheme: %v", err)
			}
			if err := v1.AddToScheme(scheme); err != nil {
				t.Fatalf("failed to add v1 to scheme: %v", err)
			}

			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.existingObjs...).Build()
			g := &lb{
				globalMPClient: fakeClient,
			}

			subnetName, err := g.ensureLeafSubnetForService(context.Background(), tc.service.Name, tc.service.Namespace, tc.service)

			if (err != nil) != tc.expectErr {
				t.Errorf("ensureLeafSubnetForService() error = %v, expectErr %v", err, tc.expectErr)
				return
			}

			if diff := cmp.Diff(tc.expectedSubnetName, subnetName); diff != "" {
				t.Errorf("ensureLeafSubnetForService() returned diff (-want +got):\n%s", diff)
			}

			if tc.validate != nil {
				tc.validate(t, g.globalMPClient)
			}
		})
	}
}

func TestExtractServiceType(t *testing.T) {
	testCases := []struct {
		name             string
		service          *v1.Service
		expectedSvc      string
		expectErrMessage string
	}{
		{
			name: "External LB - Default (no annotation)",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-svc-1",
					Namespace:   "default",
					Annotations: map[string]string{},
				},
			},
			expectedSvc:      lbExternal,
			expectErrMessage: "",
		},
		{
			name: "External LB - Explicit annotation",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-svc-2",
					Namespace: "default",
					Annotations: map[string]string{
						lbType: lbExternal,
					},
				},
			},
			expectedSvc:      lbExternal,
			expectErrMessage: "",
		},
		{
			name: "Internal LB - Explicit annotation",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-svc-3",
					Namespace: "default",
					Annotations: map[string]string{
						lbType: lbInternal,
					},
				},
			},
			expectedSvc:      lbInternal,
			expectErrMessage: "",
		},
		{
			name: "External LB - Empty string annotation",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-svc-4",
					Namespace: "default",
					Annotations: map[string]string{
						lbType: "",
					},
				},
			},
			expectedSvc:      lbExternal,
			expectErrMessage: "",
		},
		{
			name: "Error - Unsupported LBType",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-svc-5",
					Namespace: "default",
					Annotations: map[string]string{
						lbType: "unsupported-type",
					},
				},
			},
			expectedSvc:      "",
			expectErrMessage: "LBType \"unsupported-type\" not supported. Use values internal or external for service \"default\"/\"test-svc-5\"",
		},
		{
			name: "Error - External LB with internal subnet annotation",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-svc-6",
					Namespace: "default",
					Annotations: map[string]string{
						lbType:                        lbExternal,
						internalLBSubnetAnnotationKey: "my-subnet",
					},
				},
			},
			expectedSvc:      "",
			expectErrMessage: "external LB service \"default\"/\"test-svc-6\" must not have internal subnet annotation \"networking.gke.io/load-balancer-subnet\"",
		},
		{
			name: "Error - Default External LB (empty string) with internal subnet annotation",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-svc-7",
					Namespace: "default",
					Annotations: map[string]string{
						lbType:                        "",
						internalLBSubnetAnnotationKey: "my-subnet",
					},
				},
			},
			expectedSvc:      "",
			expectErrMessage: "external LB service \"default\"/\"test-svc-7\" must not have internal subnet annotation \"networking.gke.io/load-balancer-subnet\"",
		},
		{
			name: "Error - Internal LB with external IP annotation",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-svc-8",
					Namespace: "default",
					Annotations: map[string]string{
						lbType:                             lbInternal,
						externalLBIPAddressesAnnotationKey: "my-subnet",
					},
				},
			},
			expectedSvc:      "",
			expectErrMessage: "internal LB service \"default\"/\"test-svc-8\" must not have external IP annotation \"networking.gke.io/load-balancer-ip-addresses\"",
		},
		{
			name: "External LB - Default (nil annotations)",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-svc-9",
					Namespace:   "default",
					Annotations: nil,
				},
			},
			expectedSvc:      lbExternal,
			expectErrMessage: "",
		},
		{
			name: "Internal LB - Valid with internal subnet annotation",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-svc-10",
					Namespace: "default",
					Annotations: map[string]string{
						lbType:                        lbInternal,
						internalLBSubnetAnnotationKey: "my-subnet",
					},
				},
			},
			expectedSvc:      lbInternal,
			expectErrMessage: "",
		},
		{
			name: "External LB - Valid with external IP annotation",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-svc-11",
					Namespace: "default",
					Annotations: map[string]string{
						lbType:                             lbExternal,
						externalLBIPAddressesAnnotationKey: "my-subnet",
					},
				},
			},
			expectedSvc:      lbExternal,
			expectErrMessage: "",
		},
		{
			name: "External LB - Default (empty string) with external IP annotation",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-svc-12",
					Namespace: "default",
					Annotations: map[string]string{
						lbType:                             "",
						externalLBIPAddressesAnnotationKey: "my-subnet",
					},
				},
			},
			expectedSvc:      lbExternal,
			expectErrMessage: "",
		},
		{
			name: "External LB - Default with other annotations",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-svc-13",
					Namespace: "default",
					Annotations: map[string]string{
						"some.other.annotation/key": "value",
					},
				},
			},
			expectedSvc:      lbExternal,
			expectErrMessage: "",
		},
		{
			name: "External LB - external IP annotation with actual IP address",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-svc",
					Namespace: "default",
					Annotations: map[string]string{
						lbType:                             "",
						externalLBIPAddressesAnnotationKey: "1.2.3.4",
					},
				},
			},
			expectedSvc:      "",
			expectErrMessage: "external LB service \"default\"/\"test-svc\" annotation \"networking.gke.io/load-balancer-ip-addresses\" value must be a subnet name, not an IP address: got \"1.2.3.4\"",
		},
		{
			name: "Internal LB - Internal IP annotation with actual IP address",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-svc",
					Namespace: "default",
					Annotations: map[string]string{
						lbType:                        lbInternal,
						internalLBSubnetAnnotationKey: "1.2.3.4",
					},
				},
			},
			expectedSvc:      "",
			expectErrMessage: "internal LB service \"default\"/\"test-svc\" annotation \"networking.gke.io/load-balancer-subnet\" value must be a subnet name, not an IP address: got \"1.2.3.4\"",
		},
	}

	// Run test cases
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			svcType, err := extractServiceType(tc.service)

			// Check for error
			if tc.expectErrMessage != "" {
				if err == nil {
					t.Errorf("expected an error, but got nil")
				} else if !strings.Contains(err.Error(), tc.expectErrMessage) {
					t.Errorf("expected error message to contain %q, but got %q", tc.expectErrMessage, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("did not expect an error, but got: %v", err)
				}
			}

			if svcType != tc.expectedSvc {
				t.Errorf("expected service type %q, but got %q", tc.expectedSvc, svcType)
			}
		})
	}
}

func initFakeGDC() (*cloud, *runtime.Scheme, *runtime.Scheme, error) {
	globalScheme, err := globalManagementPlaneScheme()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("globalManagementPlaneScheme throws error - %s", err.Error())
	}
	zonalScheme, err := zonalManagementPlaneScheme()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("zonalManagementPlaneScheme throws error - %s", err.Error())
	}
	gdc := &cloud{
		config: &configFile{
			Project: lbNamespace,
			CaData:  "base64-string-ca",
		},
	}
	lb := &lb{
		project:        lbNamespace,
		globalMPClient: fake.NewClientBuilder().WithScheme(globalScheme).Build(),
		zonalClients: map[string]client.Client{
			zone: fake.NewClientBuilder().WithScheme(zonalScheme).Build(),
		},
	}
	gdc.lb = lb
	return gdc, globalScheme, zonalScheme, nil
}

func fetchAndCompareLBObjs(wantGlobalObjs, wantZonalObjs []client.Object, globalMPClient client.Client, zonalClients map[string]client.Client) error {
	globalKeys := []types.NamespacedName{
		{Name: lbName + fwdRuleSuffix, Namespace: lbNamespace},              // for external FR
		{Name: lbName + fwdRuleSuffix, Namespace: lbNamespace},              // for internal FR
		{Name: lbName + backendServiceRefSuffix, Namespace: lbNamespace},    // for Backend Service
		{Name: lbName + backendServicePolicySuffix, Namespace: lbNamespace}, // for Backend Service Policy
		{Name: lbName + healthCheckSuffix, Namespace: lbNamespace},          // for HC
		{Name: lbName + subnetSuffix, Namespace: lbNamespace},               // for Subnet
	}

	gotGlobalObjs := []client.Object{
		&globalnetworkingv1.ForwardingRuleExternal{},
		&globalnetworkingv1.ForwardingRuleInternal{},
		&globalnetworkingv1.BackendService{},
		&globalnetworkingv1.BackendServicePolicy{},
		&globalnetworkingv1.HealthCheck{},
		&ipamglobalv1.Subnet{},
	}

	var filteredWantGlobalObjs []client.Object
	var wantPNPs []*globalnetworkingv1.ProjectNetworkPolicy

	// Basic objects 0-4
	for i := 0; i < 5; i++ {
		if i < len(wantGlobalObjs) {
			filteredWantGlobalObjs = append(filteredWantGlobalObjs, wantGlobalObjs[i])
		} else {
			filteredWantGlobalObjs = append(filteredWantGlobalObjs, nil)
		}
	}

	// Identify PNPs and Subnet from the rest
	for i := 5; i < len(wantGlobalObjs); i++ {
		obj := wantGlobalObjs[i]
		if obj != nil {
			if pnp, ok := obj.(*globalnetworkingv1.ProjectNetworkPolicy); ok {
				wantPNPs = append(wantPNPs, pnp)
				continue
			}
		}

		// If it's the last element and not a PNP, it's the Subnet
		if i == len(wantGlobalObjs)-1 {
			filteredWantGlobalObjs = append(filteredWantGlobalObjs, obj)
		}
	}

	gotZonalObjs := []client.Object{
		&networkingv1.Backend{},
	}
	zonalKeys := []types.NamespacedName{
		{Name: lbName + backendSuffix, Namespace: lbNamespace},
	}

	combinedErrors := compareClientObjs(globalMPClient, globalKeys, gotGlobalObjs, filteredWantGlobalObjs)
	for _, zonalClient := range zonalClients {
		err := compareClientObjs(zonalClient, zonalKeys, gotZonalObjs, wantZonalObjs)
		if err != nil {
			combinedErrors = multierror.Append(combinedErrors, err)
		}
	}

	existingPNPs := &globalnetworkingv1.ProjectNetworkPolicyList{}
	if err := globalMPClient.List(context.Background(), existingPNPs, client.InNamespace(lbNamespace)); err != nil {
		return multierror.Append(combinedErrors, fmt.Errorf("list existing PNPs: %w", err))
	}

	if len(existingPNPs.Items) != len(wantPNPs) {
		combinedErrors = multierror.Append(combinedErrors, fmt.Errorf("PNP count mismatch: got %d, want %d", len(existingPNPs.Items), len(wantPNPs)))
	} else {
		for _, wantPnp := range wantPNPs {
			found := false
			for _, gotPnp := range existingPNPs.Items {
				if gotPnp.Name == wantPnp.Name {
					found = true
					ignoreResourceField := cmpopts.IgnoreFields(metav1.ObjectMeta{}, "ResourceVersion")
					if diff := cmp.Diff(wantPnp, &gotPnp, ignoreResourceField); diff != "" {
						combinedErrors = multierror.Append(combinedErrors, fmt.Errorf("comparePNP(%q) returned unexpected diff (-want +got):\n%s", wantPnp.Name, diff))
					}
					break
				}
			}
			if !found {
				combinedErrors = multierror.Append(combinedErrors, fmt.Errorf("expected PNP %s not found", wantPnp.Name))
			}
		}
	}

	return combinedErrors
}

func compareClientObjs(client client.Client, clientKeys []types.NamespacedName, gotObjs, wantObjs []client.Object) error {
	ignoreResourceField := cmpopts.IgnoreFields(metav1.ObjectMeta{}, "ResourceVersion")
	ctx := context.Background()

	var combinedErrors error

	for i := range wantObjs {
		err := client.Get(ctx, clientKeys[i], gotObjs[i])

		// want nil, but the object was found. THIS IS A FAILURE.
		if wantObjs[i] == nil && err == nil {
			combinedErrors = multierror.Append(combinedErrors, fmt.Errorf("%v: expected object to be deleted, but it was found", clientKeys[i]))
			continue
		}

		// want nil, and it was correctly not found. THIS IS A SUCCESS.
		if wantObjs[i] == nil && k8serrors.IsNotFound(err) {
			continue // Correct, move to the next item.
		}

		// want an object, but it wasn't found. THIS IS A FAILURE.
		if wantObjs[i] != nil && k8serrors.IsNotFound(err) {
			combinedErrors = multierror.Append(combinedErrors, fmt.Errorf("%v: wanted object but it was not found", clientKeys[i]))
			continue
		}

		// unexpected errors from the client.
		if err != nil && !k8serrors.IsNotFound(err) {
			combinedErrors = multierror.Append(combinedErrors, fmt.Errorf("%v: got unexpected client error: %w", clientKeys[i], err))
			continue
		}

		if diff := cmp.Diff(wantObjs[i], gotObjs[i], ignoreResourceField); diff != "" {
			combinedErrors = multierror.Append(combinedErrors, fmt.Errorf("compareResource(%q) returned unexpected diff (-want +got):\n%s", clientKeys[i], diff))
		}
	}
	return combinedErrors
}

func initFakeInternalService() *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      shootName,
			Namespace: shootNamespace,
			UID:       shootUUID,
			Annotations: map[string]string{
				lbType: lbInternal,
			},
		},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeLoadBalancer,
			Ports: []v1.ServicePort{
				{
					Name:       "port-0",
					Port:       8080,
					TargetPort: intstr.FromInt(80),
					Protocol:   v1.ProtocolTCP,
					NodePort:   30000,
				},
				{
					Name:       "port-1",
					Port:       8081,
					TargetPort: intstr.FromInt(81),
					Protocol:   v1.ProtocolTCP,
					NodePort:   30001,
				},
			},
		},
	}
}

func initFakeExternalService() *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      shootName,
			Namespace: shootNamespace,
			UID:       shootUUID,
			Annotations: map[string]string{
				lbType: lbExternal,
			},
		},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeLoadBalancer,
			Ports: []v1.ServicePort{
				{
					Name:       "port-2",
					Port:       8082,
					TargetPort: intstr.FromInt(82),
					Protocol:   v1.ProtocolTCP,
					NodePort:   30002,
				},
				{
					Name:       "port-3",
					Port:       8083,
					TargetPort: intstr.FromInt(83),
					Protocol:   v1.ProtocolTCP,
					NodePort:   30003,
				},
			},
		},
	}
}

func initFakeExternalLBObjs(name, namespace, clusterName string, service *v1.Service) ([]client.Object, []client.Object) {
	healthCheckName := name + healthCheckSuffix
	healthCheckPort := defaultHealthzPort
	if service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
		healthCheckPort = service.Spec.HealthCheckNodePort
	}

	globalObjs := []client.Object{
		&globalnetworkingv1.ForwardingRuleExternal{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + fwdRuleSuffix,
				Namespace: namespace,
			},
			Spec: networkingv1.ForwardingRuleExternalSpec{
				ForwardingRuleSpecCommon: networkingv1.ForwardingRuleSpecCommon{
					BackendServiceRef: &networkingv1.BackendServiceRef{
						Name: name + backendServiceRefSuffix,
					},
					Ports: configureFRPorts(service.Spec.Ports),
				},
			},
		},
		&globalnetworkingv1.BackendService{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + backendServiceRefSuffix,
				Namespace: namespace,
				Labels: map[string]string{
					besSelectorLabel: name + backendServiceRefSuffix,
				},
			},
			Spec: globalnetworkingv1.BackendServiceSpec{
				TargetPorts: configureBSTargetPorts(service.Spec.Ports),
				BackendRefs: []globalnetworkingv1.BackendRef{
					{
						Zone: zone,
						Name: name + backendSuffix,
					},
				},
				HealthCheckName: &(healthCheckName),
			},
		},
		&globalnetworkingv1.BackendServicePolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + backendServicePolicySuffix,
				Namespace: namespace,
			},
			Spec: networkingv1.BackendServicePolicySpec{
				SessionAffinity: networkingv1.SessionAffinityClientIpDstPortProto,
				Selectors: metav1.LabelSelector{
					MatchLabels: map[string]string{
						besSelectorLabel: name + backendServiceRefSuffix,
					},
				},
			},
		},
		&globalnetworkingv1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name:      healthCheckName,
				Namespace: namespace,
			},
			Spec: networkingv1.HealthCheckSpec{
				ProbeHandler: networkingv1.ProbeHandler{
					HTTPHealthCheck: &networkingv1.HTTPHealthCheck{
						Port:        healthCheckPort,
						RequestPath: "/healthz",
					},
				},
			},
		},
		&globalnetworkingv1.ProjectNetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels: map[string]string{
					pnpLabelServiceUID: string(service.UID),
				},
			},
			Spec: networkingv1.ProjectNetworkPolicySpec{
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
						Ports: servicePortToPnpPorts(service.Spec.Ports),
					},
				},
			},
		},
	}
	zonalObjs := []client.Object{
		&networkingv1.Backend{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + backendSuffix,
				Namespace: namespace,
			},
			Spec: networkingv1.BackendSpec{
				EndpointsLabels: metav1.LabelSelector{
					MatchLabels: map[string]string{
						lbSelectorLabel: clusterName,
					},
				},
			},
		},
	}
	return globalObjs, zonalObjs
}

func initFakeInternalLBObjs(name, namespace, clusterName string, service *v1.Service) ([]client.Object, []client.Object) {
	healthCheckPort := defaultHealthzPort
	if service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
		healthCheckPort = service.Spec.HealthCheckNodePort
	}

	globalObjs := []client.Object{
		&globalnetworkingv1.ForwardingRuleInternal{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + fwdRuleSuffix,
				Namespace: namespace,
			},
			Spec: networkingv1.ForwardingRuleInternalSpec{
				ForwardingRuleSpecCommon: networkingv1.ForwardingRuleSpecCommon{
					BackendServiceRef: &networkingv1.BackendServiceRef{
						Name: name + backendServiceRefSuffix,
					},
					Ports: configureFRPorts(service.Spec.Ports),
				},
			},
		},
		&globalnetworkingv1.BackendService{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + backendServiceRefSuffix,
				Namespace: namespace,
				Labels: map[string]string{
					besSelectorLabel: name + backendServiceRefSuffix,
				},
			},
			Spec: globalnetworkingv1.BackendServiceSpec{
				TargetPorts: configureBSTargetPorts(service.Spec.Ports),
				BackendRefs: []globalnetworkingv1.BackendRef{
					{
						Zone: zone,
						Name: name + backendSuffix,
					},
				},
				HealthCheckName: ptr.To(name + healthCheckSuffix),
			},
		},
		&globalnetworkingv1.BackendServicePolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + backendServicePolicySuffix,
				Namespace: namespace,
			},
			Spec: networkingv1.BackendServicePolicySpec{
				SessionAffinity: networkingv1.SessionAffinityClientIpDstPortProto,
				Selectors: metav1.LabelSelector{
					MatchLabels: map[string]string{
						besSelectorLabel: name + backendServiceRefSuffix,
					},
				},
			},
		},
		&globalnetworkingv1.HealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + healthCheckSuffix,
				Namespace: namespace,
			},
			Spec: networkingv1.HealthCheckSpec{
				ProbeHandler: networkingv1.ProbeHandler{
					HTTPHealthCheck: &networkingv1.HTTPHealthCheck{
						Port:        healthCheckPort,
						RequestPath: "/healthz",
					},
				},
			},
		},
		&globalnetworkingv1.ProjectNetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      func() string { name, _ := getProjectNetworkPolicyName(string(service.UID), namespace); return name }(),
				Namespace: namespace,
				Labels: map[string]string{
					pnpLabelServiceUID:        string(service.UID),
					ilbPnpLabelAllowedProject: namespace,
				},
			},
			Spec: networkingv1.ProjectNetworkPolicySpec{
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
						Ports: servicePortToPnpPorts(service.Spec.Ports),
						From: []networkingv1.ProjectNetworkPolicyPeer{
							{
								Projects: &networkingv1.PolicyProjects{
									MatchNames: []string{namespace},
								},
							},
						},
					},
				},
			},
		},
		&ipamglobalv1.Subnet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + subnetSuffix,
				Namespace: namespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "networking.global.gdc.goog/v1",
						Kind:       "ForwardingRuleInternal",
						Name:       name + fwdRuleSuffix,
					},
				},
			},
			Spec: ipamglobalv1.SubnetSpec{
				Type: ipamv1.Leaf,
				IPv4Request: &ipamv1.SubnetRequest{
					PrefixLength: ptr.To(int32(32)),
				},
				ParentReference: &ipamv1.SubnetReference{
					Name:      lbBranchSubnetName,
					Namespace: ptr.To(namespace),
				},
			},
		},
		&ipamglobalv1.Subnet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      lbBranchSubnetName,
				Namespace: namespace,
			},
			Spec: ipamglobalv1.SubnetSpec{
				Type: ipamv1.Branch,
			},
		},
		&ipamglobalv1.Subnet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      lbLeafSubnetName,
				Namespace: namespace,
			},
			Spec: ipamglobalv1.SubnetSpec{
				Type: ipamv1.Leaf,
			},
		},
	}
	zonalObjs := []client.Object{
		&networkingv1.Backend{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + backendSuffix,
				Namespace: namespace,
			},
			Spec: networkingv1.BackendSpec{
				EndpointsLabels: metav1.LabelSelector{
					MatchLabels: map[string]string{
						lbSelectorLabel: clusterName,
					},
				},
			},
		},
	}
	return globalObjs, zonalObjs
}

func initFakeClient(scheme *runtime.Scheme, objects ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func createExpectedPNP(baseName, namespace, clusterName, serviceUID, project string, ports []networkingv1.ProjectNetworkPolicyPort) *globalnetworkingv1.ProjectNetworkPolicy {
	pnpName, _ := getProjectNetworkPolicyName(serviceUID, project)

	pnp := &globalnetworkingv1.ProjectNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pnpName,
			Namespace: namespace,
			Labels: map[string]string{
				pnpLabelServiceUID: serviceUID,
			},
		},
		Spec: networkingv1.ProjectNetworkPolicySpec{
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
					Ports: ports,
				},
			},
		},
	}

	if project != "*" {
		pnp.Labels[ilbPnpLabelAllowedProject] = project
		pnp.Spec.Ingress[0].From = []networkingv1.ProjectNetworkPolicyPeer{
			{
				Projects: &networkingv1.PolicyProjects{
					MatchNames: []string{project},
				},
			},
		}
	}
	return pnp
}
