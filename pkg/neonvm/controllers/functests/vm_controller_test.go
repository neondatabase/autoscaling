/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package functests

import (
	"context"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/neonvm/controllers"
)

var _ = Describe("VirtualMachine controller", func() {
	Context("VirtualMachine controller test", func() {
		const VirtualMachineName = "test-virtualmachine"

		ctx := context.Background()

		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:      VirtualMachineName,
				Namespace: VirtualMachineName,
			},
		}

		typeNamespaceName := types.NamespacedName{Name: VirtualMachineName, Namespace: VirtualMachineName}

		BeforeEach(func() {
			By("Creating the Namespace to perform the tests")
			err := k8sClient.Create(ctx, namespace)
			Expect(err).To(Not(HaveOccurred()))

			By("Setting the Image ENV VAR which stores the Operand image")
			err = os.Setenv("VM_RUNNER_IMAGE", "runner:test")
			Expect(err).To(Not(HaveOccurred()))
		})

		AfterEach(func() {
			By("Deleting the Namespace to perform the tests")
			_ = k8sClient.Delete(ctx, namespace)

			By("Removing the Image ENV VAR which stores the Operand image")
			_ = os.Unsetenv("VM_RUNNER_IMAGE")
		})

		It("should successfully reconcile a custom resource for VirtualMachine", func() {
			By("Creating the custom resource for the Kind VirtualMachine")
			virtualmachine := &vmv1.VirtualMachine{}
			err := k8sClient.Get(ctx, typeNamespaceName, virtualmachine)
			if err != nil && errors.IsNotFound(err) {
				// Let's mock our custom resource at the same way that we would
				// apply on the cluster the manifest under config/samples
				virtualmachine := &vmv1.VirtualMachine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      VirtualMachineName,
						Namespace: namespace.Name,
					},
					Spec: vmv1.VirtualMachineSpec{
						QMP:           1,
						RestartPolicy: "Never",
						RunnerPort:    1,
						Guest: vmv1.Guest{ //nolint:exhaustruct // other stuff will get defaulted
							CPUs:        vmv1.CPUs{Min: 1, Use: 1, Max: 1},
							MemorySlots: vmv1.MemorySlots{Min: 1, Use: 1, Max: 1},
						},
					},
				}

				err = k8sClient.Create(ctx, virtualmachine)
				Expect(err).To(Not(HaveOccurred()))
			}

			By("Checking if the custom resource was successfully created")
			Eventually(func() error {
				found := &vmv1.VirtualMachine{}
				return k8sClient.Get(ctx, typeNamespaceName, found)
			}, time.Minute, time.Second).Should(Succeed())

			By("Reconciling the custom resource created")
			virtualmachineReconciler := &controllers.VMReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: nil,
				Config: &controllers.ReconcilerConfig{
					DisableRunnerCgroup:     false,
					MaxConcurrentReconciles: 1,
					SkipUpdateValidationFor: nil,
					QEMUDiskCacheSettings:   "cache=none",
					DefaultMemoryProvider:   vmv1.MemoryProviderDIMMSlots,
					MemhpAutoMovableRatio:   "301",
					FailurePendingPeriod:    1 * time.Minute,
					FailingRefreshInterval:  1 * time.Minute,
					AtMostOnePod:            false,
					DefaultCPUScalingMode:   vmv1.CpuScalingModeQMP,
				},
			}

			_, err = virtualmachineReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespaceName,
			})
			Expect(err).To(Not(HaveOccurred()))
			/*
				By("Checking if Deployment was successfully created in the reconciliation")
				Eventually(func() error {
					found := &appsv1.Deployment{}
					return k8sClient.Get(ctx, typeNamespaceName, found)
				}, time.Minute, time.Second).Should(Succeed())
			*/
			/*
				By("Checking the latest Status Condition added to the VirtualMachine instance")
				Eventually(func() error {
					if virtualmachine.Status.Conditions != nil && len(virtualmachine.Status.Conditions) != 0 {
						latestStatusCondition := virtualmachine.Status.Conditions[len(virtualmachine.Status.Conditions)-1]
						expectedLatestStatusCondition := metav1.Condition{Type: typeAvailableVirtualMachine,
							Status: metav1.ConditionTrue, Reason: "Reconciling",
							Message: fmt.Sprintf("Deployment for custom resource (%s) with %d replicas created successfully", virtualmachine.Name, virtualmachine.Spec.Size)}
						if latestStatusCondition != expectedLatestStatusCondition {
							return errors.New("The latest status condition added to the virtualmachine instance is not as expected")
						}
					}
					return nil
				}, time.Minute, time.Second).Should(Succeed())
			*/
		})
	})
})
