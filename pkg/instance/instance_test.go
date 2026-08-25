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
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	vmv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/virtualmachine/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	providerName     = "fake-gdc"
	projectNamespace = "test-project"
	instanceName     = "vm-1234"
	zone             = "fake-region-zone"
	vmType           = "fake-type"
	providerID       = providerName + "://" + projectNamespace + "/" + zone + "/" + instanceName
)

func TestInstanceID(t *testing.T) {
	tests := map[string]struct {
		instanceID string
		nodeName   types.NodeName
	}{
		"Successfully get instance ID": {
			instanceID: "test-project/fake-region-zone/node1",
			nodeName:   types.NodeName("node1"),
		},
	}

	for id, tc := range tests {
		t.Run(id, func(t *testing.T) {
			vm := &vmv1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "node1",
					Namespace: projectNamespace,
				},
			}
			i, err := newFakeInstance(zone, vm)
			if err != nil {
				t.Fatalf("create fake instance: %v", err)
			}

			if id, err := i.InstanceID(context.Background(), tc.nodeName); err != nil {
				t.Errorf("unexpected error: %v", err)
			} else if tc.instanceID != id {
				t.Errorf("unexpected instance ID: want %q but got %q", tc.instanceID, id)
			}
		})
	}
}

func TestInstanceIDError(t *testing.T) {
	tests := map[string]struct {
		newInstance func(*testing.T) cloudprovider.Instances
		errMsg      string
		nodeName    types.NodeName
	}{
		"InstanceID for non-existing VM": {
			newInstance: func(*testing.T) cloudprovider.Instances {
				i, err := newFakeInstance(zone)
				if err != nil {
					t.Fatalf("create fake instance: %v", err)
				}
				return i
			},
			errMsg:   "instance not found",
			nodeName: types.NodeName("node1"),
		},
		"server error when getting the VM": {
			newInstance: func(*testing.T) cloudprovider.Instances {
				// use a client without registered scheme to trigger an error
				zc := map[string]client.Client{
					zone: fake.NewClientBuilder().Build(),
				}
				i, err := New(&Config{
					ZonalClients: zc,
					Project:      projectNamespace,
					ProviderName: providerName,
				})
				if err != nil {
					t.Errorf("create instance: %v", err)
				}
				return i
			},
			errMsg:   "get vm.VirtualMachine \"test-project\"/\"node1\"",
			nodeName: types.NodeName("node1"),
		},
		"zonal client missing": {
			newInstance: func(*testing.T) cloudprovider.Instances {
				i, err := newFakeInstance("incorrect-zone")
				if err != nil {
					t.Fatalf("create fake instance: %v", err)
				}
				return i
			},
			errMsg:   "instance not found",
			nodeName: types.NodeName("node1"),
		},
	}

	for id, tc := range tests {
		t.Run(id, func(t *testing.T) {
			i := tc.newInstance(t)
			if _, err := i.InstanceID(context.Background(), tc.nodeName); err == nil {
				t.Error("expecting an error but got nil")
			} else if !strings.Contains(err.Error(), tc.errMsg) {
				t.Errorf("unexpected error: want %v but got %v", tc.errMsg, err)
			}
		})
	}
}

func TestNodeAddressesByProviderIDError(t *testing.T) {
	ctx := context.Background()

	tests := map[string]struct {
		initialize func(t *testing.T) cloudprovider.Instances
		wantErr    error
		providerID string
	}{
		"VM missing status": {
			initialize: func(t *testing.T) cloudprovider.Instances {
				vmMissingStatus := fakeVM()
				vmMissingStatus.Status = vmv1.VirtualMachineStatus{}
				vmMissingStatusInstance, err := newFakeInstance(zone, vmMissingStatus)
				if err != nil {
					t.Fatalf("vmMissingStatusGDC throws error %v", err)
				}
				return vmMissingStatusInstance
			},
			providerID: providerID,
			wantErr: fmt.Errorf("IP cant be determined from vm.Status.Network.Interfaces - %v",
				[]vmv1.NetworkInterfaceStatus{}),
		},
		"missing VM ipAddress": {
			initialize: func(t *testing.T) cloudprovider.Instances {
				vmMissingIP := fakeVM()
				vmMissingIP.Status.Network.Interfaces = []vmv1.NetworkInterfaceStatus{}
				vmMissingIPInstance, err := newFakeInstance(zone, vmMissingIP)
				if err != nil {
					t.Fatalf("vmMissingIPGDC throws error %v", err)
				}
				return vmMissingIPInstance
			},
			providerID: providerID,
			wantErr: fmt.Errorf("IP cant be determined from vm.Status.Network.Interfaces - %v",
				[]vmv1.NetworkInterfaceStatus{}),
		},
		"incorrect VM ipAddress": {
			initialize: func(t *testing.T) cloudprovider.Instances {
				vmIncorrectIP := fakeVM()
				vmIncorrectIP.Status.Network.Interfaces[0].IpAddresses[0] = "abcd"
				vmIncorrectIPInstance, err := newFakeInstance(zone, vmIncorrectIP)
				if err != nil {
					t.Fatalf("vmIncorrectIPGDC throws error %v", err)
				}
				return vmIncorrectIPInstance
			},
			providerID: providerID,
			wantErr: fmt.Errorf("len(nodeAddresses)=0, vm.Status.Network.Interfaces=%v",
				[]vmv1.NetworkInterfaceStatus{
					{
						Name:        "eth0",
						IpAddresses: []string{"abcd"},
					},
					{
						Name:        "eth1",
						IpAddresses: []string{"2.3.4.5/32"},
					},
				}),
		},
		"missing zonal client": {
			initialize: func(t *testing.T) cloudprovider.Instances {
				vm := fakeVM()
				zonalClientMissingInstance, err := newFakeInstance("incorrect-zone", vm)
				if err != nil {
					t.Fatalf("zonalClientMissingInstance throws error %v", err)
				}
				return zonalClientMissingInstance
			},
			providerID: providerID,
			wantErr:    fmt.Errorf("zonal client for zone %q not found", zone),
		},
		"incorrect ProviderID": {
			initialize: func(t *testing.T) cloudprovider.Instances {
				vm := fakeVM()
				incorrectProviderIDInstance, err := newFakeInstance(zone, vm)
				if err != nil {
					t.Fatalf("incorrectProviderIDInstance throws error %v", err)
				}
				return incorrectProviderIDInstance
			},
			providerID: providerName + "://" + projectNamespace + "/" + instanceName,
			wantErr:    fmt.Errorf("misformatted providerID: %q", providerName+"://"+projectNamespace+"/"+instanceName),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			i := tt.initialize(t)
			if _, err := i.NodeAddressesByProviderID(ctx, tt.providerID); err == nil {
				t.Error("expecting an error but got nil")
			} else if !strings.Contains(err.Error(), tt.wantErr.Error()) {
				t.Errorf("instance.NodeAddressesByProviderID() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNodeAddressesByProviderID(t *testing.T) {
	ctx := context.Background()

	tests := map[string]struct {
		initialize        func(t *testing.T) cloudprovider.Instances
		wantNodeAddresses []v1.NodeAddress
	}{
		"correct VM status": {
			initialize: func(t *testing.T) cloudprovider.Instances {
				// initializing useful test objects
				instance, err := newFakeInstance(zone, fakeVM())
				if err != nil {
					t.Fatalf("newFakeInstance throws error %v", err)
				}
				return instance
			},
			wantNodeAddresses: []v1.NodeAddress{
				{
					Type:    v1.NodeInternalIP,
					Address: "1.2.3.4",
				},
			},
		},
		"missing subnet range ipAddress": {
			initialize: func(t *testing.T) cloudprovider.Instances {
				vmMissingCIDR := fakeVM()
				vmMissingCIDR.Status.Network.Interfaces[0].IpAddresses[0] = "1.2.3.4"
				vmMissingCIDRInstance, err := newFakeInstance(zone, vmMissingCIDR)
				if err != nil {
					t.Fatalf("vmMissingCIDRGDC throws error %v", err)
				}
				return vmMissingCIDRInstance
			},
			wantNodeAddresses: []v1.NodeAddress{
				{
					Type:    v1.NodeInternalIP,
					Address: "1.2.3.4",
				},
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			i := tt.initialize(t)
			gotNodeAddress, err := i.NodeAddressesByProviderID(ctx, providerID)

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if diff := cmp.Diff(gotNodeAddress, tt.wantNodeAddresses); diff != "" {
				t.Errorf("instance.NodeAddressesByProviderID() mismatch (-got +want):\n%s", diff)
			}
		})
	}
}

func TestInstanceExistsByProviderID(t *testing.T) {
	testCases := map[string]struct {
		expected   bool
		initialize func(t *testing.T) cloudprovider.Instances
	}{
		"Instance exists": {
			expected: true,
			initialize: func(t *testing.T) cloudprovider.Instances {
				vms := []client.Object{
					&vmv1.VirtualMachine{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "vm-1234",
							Namespace: "test-project",
						},
					},
				}
				i, err := newFakeInstance(zone, vms...)
				if err != nil {
					t.Fatalf("create fake instance: %v", err)
				}
				return i
			},
		},
		"Instance does not exist": {
			expected: false,
			initialize: func(t *testing.T) cloudprovider.Instances {
				vms := []client.Object{
					&vmv1.VirtualMachine{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "vm-2345",
							Namespace: "test-project",
						},
					},
					&vmv1.VirtualMachine{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "vm-1234",
							Namespace: "foo-project",
						},
					},
				}
				i, err := newFakeInstance(zone, vms...)
				if err != nil {
					t.Fatalf("create fake instance: %v", err)
				}
				return i
			},
		},
		"Instance exists in another zone": {
			expected: false,
			initialize: func(t *testing.T) cloudprovider.Instances {
				vms := []client.Object{
					&vmv1.VirtualMachine{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "vm-1234",
							Namespace: "test-project",
						},
					},
				}
				s := runtime.NewScheme()
				if err := vmv1.AddToScheme(s); err != nil {
					t.Fatalf("vmv1.AddToScheme(): %v", err)
				}
				zc := map[string]client.Client{
					"different-zone": fake.NewClientBuilder().WithScheme(s).WithObjects(vms...).Build(),
					zone:             fake.NewClientBuilder().WithScheme(s).Build(),
				}
				i, err := New(&Config{
					ZonalClients: zc,
					Project:      projectNamespace,
					ProviderName: providerName,
				})
				if err != nil {
					t.Fatalf("create fake instance: %v", err)
				}
				return i
			},
		},
	}

	for id, tc := range testCases {
		t.Run(id, func(t *testing.T) {
			i := tc.initialize(t)
			if got, err := i.InstanceExistsByProviderID(context.Background(), providerID); err != nil {
				t.Errorf("unexpected error: %v", err)
			} else if tc.expected != got {
				t.Errorf("unexpected value returned: want %t but got %t", tc.expected, got)
			}
		})
	}
}

func TestInstanceExistsByProviderIDError(t *testing.T) {
	tests := map[string]struct {
		initialize func(t *testing.T) cloudprovider.Instances
		wantErr    error
	}{
		"zone scheme error": {
			initialize: func(t *testing.T) cloudprovider.Instances {
				c := fake.NewClientBuilder().Build()
				i, err := New(&Config{
					ZonalClients: map[string]client.Client{
						zone: c,
					},
					Project:      projectNamespace,
					ProviderName: providerName,
				})
				if err != nil {
					t.Fatalf("create fake instance: %v", err)
				}
				return i
			},
		},
		"zone key error": {
			initialize: func(t *testing.T) cloudprovider.Instances {
				c := fake.NewClientBuilder().Build()
				i, err := New(&Config{
					ZonalClients: map[string]client.Client{
						"incorrect-zone": c,
					},
					Project:      projectNamespace,
					ProviderName: providerName,
				})
				if err != nil {
					t.Fatalf("create fake instance: %v", err)
				}
				return i
			},
		},
		"zone client missing": {
			initialize: func(t *testing.T) cloudprovider.Instances {
				i, err := New(&Config{
					ZonalClients: map[string]client.Client{
						zone: nil,
					},
					Project:      projectNamespace,
					ProviderName: providerName,
				})
				if err != nil {
					t.Fatalf("create fake instance: %v", err)
				}
				return i
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			i := tt.initialize(t)
			got, err := i.InstanceExistsByProviderID(context.Background(), providerID)
			if err == nil {
				t.Error("expect an error but got nil")
			}
			if got {
				t.Error("expect InstanceExistsByProviderID to return false but got true")
			}
		})
	}
}

func TestInstanceTypeByProviderID(t *testing.T) {
	testCases := map[string]struct {
		instanceType string
		vms          []client.Object
	}{
		"successfully get instance type": {
			instanceType: "type1",
			vms: []client.Object{
				&vmv1.VirtualMachine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      instanceName,
						Namespace: projectNamespace,
					},
					Spec: vmv1.VirtualMachineSpec{
						Compute: vmv1.Compute{
							VirtualMachineType: "type1",
						},
					},
				},
				&vmv1.VirtualMachine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      instanceName,
						Namespace: "incorrect-project",
					},
					Spec: vmv1.VirtualMachineSpec{
						Compute: vmv1.Compute{
							VirtualMachineType: "type2",
						},
					},
				},
			},
		},
		"instance type missing in VM spec": {
			instanceType: "",
			vms: []client.Object{
				&vmv1.VirtualMachine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      instanceName,
						Namespace: projectNamespace,
					},
					Spec: vmv1.VirtualMachineSpec{},
				},
			},
		},
	}

	for id, tc := range testCases {
		t.Run(id, func(t *testing.T) {
			i, err := newFakeInstance(zone, tc.vms...)
			if err != nil {
				t.Fatalf("create instance: %v", err)
			}

			if got, err := i.InstanceTypeByProviderID(context.Background(), providerID); err != nil {
				t.Errorf("unexpected error: %v", err)
			} else if tc.instanceType != got {
				t.Errorf("unexpected instance type: want %q but got %q", tc.instanceType, got)
			}
		})
	}
}

func TestInstanceTypeByProviderIDError(t *testing.T) {
	testCases := map[string]struct {
		errorMsg   string
		providerID string
	}{
		"wrong provider ID format": {
			errorMsg:   `misformatted providerID: "incorrect:provider:id"`,
			providerID: "incorrect:provider:id",
		},
		"get the type for a non-existing instance": {
			errorMsg:   `virtualmachines.virtualmachine.gdc.goog "non-existing-vm" not found`,
			providerID: "fake-gdc://test-project/test-zone/non-existing-vm",
		},
	}

	for id, tc := range testCases {
		t.Run(id, func(t *testing.T) {
			i, err := newFakeInstance("test-zone")
			if err != nil {
				t.Fatalf("create instance: %v", err)
			}

			if _, err := i.InstanceTypeByProviderID(context.Background(), tc.providerID); err == nil {
				t.Error("expecting an error but got nil")
			} else if err.Error() != tc.errorMsg {
				t.Errorf("unexpected error: want %q but got %q", tc.errorMsg, err.Error())
			}
		})
	}
}

func TestInstanceType(t *testing.T) {
	testCases := map[string]struct {
		instanceType string
		nodeName     string
		vms          []client.Object
	}{
		"successfully get instance type": {
			instanceType: "type1",
			nodeName:     "node1",
			vms: []client.Object{
				&vmv1.VirtualMachine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "node1",
						Namespace: "test-project",
					},
					Spec: vmv1.VirtualMachineSpec{
						Compute: vmv1.Compute{
							VirtualMachineType: "type1",
						},
					},
				},
				&vmv1.VirtualMachine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "node1",
						Namespace: "foo-project",
					},
					Spec: vmv1.VirtualMachineSpec{
						Compute: vmv1.Compute{
							VirtualMachineType: "type2",
						},
					},
				},
			},
		},
	}

	for id, tc := range testCases {
		t.Run(id, func(t *testing.T) {
			i, err := newFakeInstance(zone, tc.vms...)
			if err != nil {
				t.Fatalf("create instance: %v", err)
			}

			if got, err := i.InstanceType(context.Background(), types.NodeName(tc.nodeName)); err != nil {
				t.Errorf("unexpected error: %v", err)
			} else if tc.instanceType != got {
				t.Errorf("unexpected instance type: want %q but got %q", tc.instanceType, got)
			}
		})
	}
}

func TestInstanceTypeError(t *testing.T) {
	testCases := map[string]struct {
		errorMsg string
		nodeName string
	}{
		"get the type for a non-existing instance": {
			errorMsg: "instance not found",
			nodeName: "non-existing-vm",
		},
	}

	for id, tc := range testCases {
		t.Run(id, func(t *testing.T) {
			i, err := newFakeInstance(zone)
			if err != nil {
				t.Fatalf("create instance: %v", err)
			}

			if _, err := i.InstanceType(context.Background(), types.NodeName(tc.nodeName)); err == nil {
				t.Error("expecting an error but got nil")
			} else if err.Error() != tc.errorMsg {
				t.Errorf("unexpected error: want %q but got %q", tc.errorMsg, err.Error())
			}
		})
	}
}

func TestInstanceExists(t *testing.T) {
	testCases := map[string]struct {
		name       string
		node       *v1.Node
		vms        []client.Object
		wantExists bool
	}{
		"Instance exists and node has providerID": {
			name: instanceName,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: instanceName},
				Spec:       v1.NodeSpec{ProviderID: providerID},
			},
			vms:        []client.Object{fakeVM()},
			wantExists: true,
		},
		"Instance exists and node does not have provideID": {
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: instanceName},
			},
			vms:        []client.Object{fakeVM()},
			wantExists: true,
		},
		"Instance does not exist and node has providerID": {
			name: instanceName,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "non-existent-vm"},
				Spec: v1.NodeSpec{
					ProviderID: providerName + "://" + projectNamespace + "/" + zone + "/" + "non-existent-vm",
				},
			},
			vms:        []client.Object{fakeVM()},
			wantExists: false,
		},
		"Instance does not exist and node does not have provideID": {
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "non-existent-vm"},
			},
			vms:        []client.Object{fakeVM()},
			wantExists: false,
		},
	}
	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			i, err := newFakeInstance(zone, tt.vms...)
			if err != nil {
				t.Fatalf("create instance: %v", err)
			}

			gotExists, err := i.InstanceExists(context.Background(), tt.node)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if gotExists != tt.wantExists {
				t.Errorf("instance.InstanceExists() = %v, want %v", gotExists, tt.wantExists)
			}
		})
	}
}

func TestInstanceExistsError(t *testing.T) {
	testCases := map[string]struct {
		node       *v1.Node
		initialize func(t *testing.T) cloudprovider.InstancesV2
		wantErrMsg string
	}{
		"Invalid ProviderID format": {
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: instanceName,
				},
				Spec: v1.NodeSpec{
					ProviderID: "invalid:provider:id",
				},
			},
			initialize: func(t *testing.T) cloudprovider.InstancesV2 {
				i, err := newFakeInstance(zone, fakeVM()) // VM exists but providerID is bad
				if err != nil {
					t.Fatalf("create fake instance: %v", err)
				}
				return i
			},
			wantErrMsg: `misformatted providerID: "invalid:provider:id"`,
		},
		"Missing Zonal Client for ProviderID": {
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: instanceName,
				},
				Spec: v1.NodeSpec{
					ProviderID: providerName + "://" + projectNamespace + "/missing-zone/" + instanceName,
				},
			},
			initialize: func(t *testing.T) cloudprovider.InstancesV2 {
				i, err := newFakeInstance(zone, fakeVM())
				if err != nil {
					t.Fatalf("create fake instance: %v", err)
				}
				return i
			},
			wantErrMsg: `zonal client for zone "missing-zone" not found`,
		},
		"Zonal client scheme missing": {
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: instanceName},
				Spec:       v1.NodeSpec{ProviderID: providerID},
			},
			initialize: func(t *testing.T) cloudprovider.InstancesV2 {
				zc := map[string]client.Client{
					zone: fake.NewClientBuilder().Build(),
				}
				i, err := New(&Config{
					ZonalClients: zc,
					Project:      projectNamespace,
					ProviderName: providerName,
				})
				if err != nil {
					t.Fatalf("create instance: %v", err)
				}
				return i
			},
			wantErrMsg: "no kind is registered for the type v1.VirtualMachine",
		},
		"Zonal client scheme missing without ProviderID": {
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: instanceName},
			},
			initialize: func(t *testing.T) cloudprovider.InstancesV2 {
				zc := map[string]client.Client{
					zone: fake.NewClientBuilder().Build(),
				}
				i, err := New(&Config{
					ZonalClients: zc,
					Project:      projectNamespace,
					ProviderName: providerName,
				})
				if err != nil {
					t.Fatalf("create instance: %v", err)
				}
				return i
			},
			wantErrMsg: "no kind is registered for the type v1.VirtualMachine",
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			i := tc.initialize(t)
			exists, err := i.InstanceExists(context.Background(), tc.node)

			if err == nil {
				t.Errorf("Expected an error, but got nil")
			} else if !strings.Contains(err.Error(), tc.wantErrMsg) {
				t.Errorf("Expected error containing %q, but got: %v", tc.wantErrMsg, err)
			}

			if exists {
				t.Errorf("Expected exists to be false on error, but got true")
			}
		})
	}
}

func TestInstanceShutdown(t *testing.T) {
	testCases := map[string]struct {
		node         *v1.Node
		vms          []client.Object
		wantShutdown bool
	}{
		"VM is stopped": {
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: instanceName},
				Spec:       v1.NodeSpec{ProviderID: providerID},
			},
			vms: []client.Object{
				func() *vmv1.VirtualMachine {
					vm := fakeVM()
					vm.Status.State = vmv1.VirtualMachineStateStopped
					return vm
				}(),
			},
			wantShutdown: true,
		},
		"VM is running": {
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: instanceName},
				Spec:       v1.NodeSpec{ProviderID: providerID},
			},
			vms: []client.Object{
				func() *vmv1.VirtualMachine {
					vm := fakeVM()
					vm.Status.State = vmv1.VirtualMachineStateRunning // Explicitly set running
					return vm
				}(),
			},
			wantShutdown: false,
		},
		"VM state is unknown/empty": {
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: instanceName},
				Spec:       v1.NodeSpec{ProviderID: providerID},
			},
			vms: []client.Object{
				func() *vmv1.VirtualMachine {
					vm := fakeVM()
					vm.Status.State = ""
					return vm
				}(),
			},
			wantShutdown: false,
		},
		"VM is stopped with node missing providerId": {
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: instanceName},
			},
			vms: []client.Object{
				func() *vmv1.VirtualMachine {
					vm := fakeVM()
					vm.Status.State = vmv1.VirtualMachineStateStopped
					return vm
				}(),
			},
			wantShutdown: true,
		},
	}

	ctx := context.Background()
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			i, err := newFakeInstance(zone, tc.vms...)
			if err != nil {
				t.Fatalf("create instance: %v", err)
			}

			gotShutdown, err := i.InstanceShutdown(ctx, tc.node)

			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			if tc.wantShutdown != gotShutdown {
				t.Errorf("instance.InstanceShutdown() = %v, want %v", gotShutdown, tc.wantShutdown)
			}
		})
	}
}

func TestInstanceShutdownError(t *testing.T) {
	testCases := map[string]struct {
		node    *v1.Node
		vms     []client.Object
		wantErr string
	}{
		"VM not found": {
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "non-existent-vm"},
				Spec:       v1.NodeSpec{ProviderID: providerName + "://" + projectNamespace + "/" + zone + "/non-existent-vm"},
			},
			vms:     []client.Object{fakeVM()},
			wantErr: `virtualmachines.virtualmachine.gdc.goog "non-existent-vm" not found`,
		},
		"Invalid provider ID format": {
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: instanceName},
				Spec:       v1.NodeSpec{ProviderID: "invalid:provider:id"},
			},
			vms:     []client.Object{fakeVM()},
			wantErr: `misformatted providerID: "invalid:provider:id"`,
		},
		"Provider ID with missing zone": {
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: instanceName},
				Spec:       v1.NodeSpec{ProviderID: providerName + "://" + projectNamespace + "/" + instanceName},
			},
			vms:     []client.Object{fakeVM()},
			wantErr: fmt.Sprintf(`misformatted providerID: %q`, providerName+"://"+projectNamespace+"/"+instanceName),
		},
		"Zonal client missing for the zone in providerID": {
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: instanceName},
				Spec:       v1.NodeSpec{ProviderID: providerName + "://" + projectNamespace + "/missing-zone/" + instanceName},
			},
			vms:     []client.Object{fakeVM()}, // VM exists, but in a different zone's client
			wantErr: `zonal client for zone "missing-zone" not found`,
		},
	}
	ctx := context.Background()
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			i, err := newFakeInstance(zone, tc.vms...)
			if err != nil {
				t.Fatalf("create instance: %v", err)
			}

			gotShutdown, err := i.InstanceShutdown(ctx, tc.node)
			if err == nil {
				t.Error("expecting an error but got nil")
			}
			if err.Error() != tc.wantErr {
				t.Errorf("unexpected error: want %q but got %q", tc.wantErr, err.Error())
			}
			if gotShutdown != false {
				t.Errorf("unexpected shutdown: want %v but got %v", false, gotShutdown)
			}
		})
	}
}

func TestInstanceMetadata(t *testing.T) {
	testCases := map[string]struct {
		node             *v1.Node
		vms              []client.Object
		expectedMetadata *cloudprovider.InstanceMetadata
	}{
		"successfully get instance metadata with providerID": {
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: instanceName,
				},
				Spec: v1.NodeSpec{
					ProviderID: providerID,
				},
			},
			vms: []client.Object{
				fakeVM(),
			},
			expectedMetadata: &cloudprovider.InstanceMetadata{
				ProviderID:   "fake-gdc://test-project/fake-region-zone/vm-1234",
				Zone:         "fake-region-zone",
				Region:       "fake-region",
				InstanceType: "fake-type",
				NodeAddresses: []v1.NodeAddress{
					{
						Type:    v1.NodeInternalIP,
						Address: "1.2.3.4",
					},
				},
			},
		},
		"successfully get instance metadata without providerID": {
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: instanceName,
				},
			},
			vms: []client.Object{
				fakeVM(),
			},
			expectedMetadata: &cloudprovider.InstanceMetadata{
				ProviderID:   "fake-gdc://test-project/fake-region-zone/vm-1234",
				Zone:         "fake-region-zone",
				Region:       "fake-region",
				InstanceType: "fake-type",
				NodeAddresses: []v1.NodeAddress{
					{
						Type:    v1.NodeInternalIP,
						Address: "1.2.3.4",
					},
				},
			},
		},
		"missing instance type": {
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: instanceName,
				},
				Spec: v1.NodeSpec{
					ProviderID: "fake-gdc://test-project/fake-region-zone/vm-1234",
				},
			},
			vms: []client.Object{
				&vmv1.VirtualMachine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      instanceName,
						Namespace: projectNamespace,
					},
					Status: vmv1.VirtualMachineStatus{
						Network: vmv1.NetworkStatus{
							Interfaces: []vmv1.NetworkInterfaceStatus{
								{
									Name: "eth0",
									IpAddresses: []string{
										"1.2.3.4/32",
									},
								},
							},
						},
					},
				},
			},
			expectedMetadata: &cloudprovider.InstanceMetadata{
				ProviderID:   "fake-gdc://test-project/fake-region-zone/vm-1234",
				Zone:         "fake-region-zone",
				Region:       "fake-region",
				InstanceType: "",
				NodeAddresses: []v1.NodeAddress{
					{
						Type:    v1.NodeInternalIP,
						Address: "1.2.3.4",
					},
				},
			},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			i, err := newFakeInstance(zone, tc.vms...)
			if err != nil {
				t.Fatalf("create instance: %v", err)
			}

			metadata, err := i.InstanceMetadata(context.Background(), tc.node)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if diff := cmp.Diff(metadata, tc.expectedMetadata); diff != "" {
				t.Errorf("instance.InstanceMetadata() mismatch (-got +want):\n%s", diff)
			}
		})
	}
}

func TestInstanceMetadataError(t *testing.T) {
	testCases := map[string]struct {
		node          *v1.Node
		initialize    func(t *testing.T) (*instanceImpl, error)
		expectedError error
	}{
		"instance not found": {
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "non-existing-vm",
				},
				Spec: v1.NodeSpec{
					ProviderID: "fake-gdc://test-project/fake-region-zone/non-existing-vm",
				},
			},
			initialize: func(t *testing.T) (*instanceImpl, error) {
				return newFakeInstance(zone)
			},
			expectedError: fmt.Errorf(`virtualmachines.virtualmachine.gdc.goog "non-existing-vm" not found`),
		},
		"invalid zone format": {
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: instanceName,
				},
				Spec: v1.NodeSpec{
					ProviderID: "fake-gdc://test-project/invalid-zone/vm-1234",
				},
			},
			initialize: func(t *testing.T) (*instanceImpl, error) {
				return newFakeInstance(zone)
			},
			expectedError: fmt.Errorf("invalid zone format: invalid-zone"),
		},
		"incorrect providerID format": {
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: instanceName,
				},
				Spec: v1.NodeSpec{
					ProviderID: "incorrect:provider:id",
				},
			},
			initialize: func(t *testing.T) (*instanceImpl, error) {
				return newFakeInstance(zone)
			},
			expectedError: fmt.Errorf(`misformatted providerID: "incorrect:provider:id"`),
		},
		"missing ip address": {
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: instanceName,
				},
				Spec: v1.NodeSpec{
					ProviderID: "fake-gdc://test-project/fake-region-zone/vm-1234",
				},
			},
			initialize: func(t *testing.T) (*instanceImpl, error) {
				vm := fakeVM()
				vm.Status.Network.Interfaces = []vmv1.NetworkInterfaceStatus{}
				return newFakeInstance(zone, vm)
			},
			expectedError: fmt.Errorf("IP cant be determined from vm.Status.Network.Interfaces - %v",
				[]vmv1.NetworkInterfaceStatus{}),
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			i, err := tc.initialize(t)
			if err != nil {
				t.Fatalf("create instance: %v", err)
			}

			metadata, err := i.InstanceMetadata(context.Background(), tc.node)
			if err == nil || metadata != nil {
				t.Error("expecting an error but got nil")
			}

			if err.Error() != tc.expectedError.Error() {
				t.Errorf("expected error %q, but got %q", tc.expectedError.Error(), err.Error())
			}
		})
	}
}

func newFakeInstance(zoneKey string, vmInstances ...client.Object) (*instanceImpl, error) {
	s := runtime.NewScheme()
	if err := vmv1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("vmv1.AddToScheme(): %v", err)
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(vmInstances...).Build()
	zc := map[string]client.Client{
		zoneKey: c,
	}

	return New(&Config{
		ZonalClients: zc,
		Project:      projectNamespace,
		ProviderName: providerName,
	})
}

func fakeVM() *vmv1.VirtualMachine {
	return &vmv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instanceName,
			Namespace: projectNamespace,
		},
		Spec: vmv1.VirtualMachineSpec{
			Compute: vmv1.Compute{
				VirtualMachineType: vmType,
			},
		},
		Status: vmv1.VirtualMachineStatus{
			Network: vmv1.NetworkStatus{
				Interfaces: []vmv1.NetworkInterfaceStatus{
					{
						Name: "eth0",
						IpAddresses: []string{
							"1.2.3.4/32",
						},
					},
					{
						Name: "eth1",
						IpAddresses: []string{
							"2.3.4.5/32",
						},
					},
				},
			},
		},
	}
}
