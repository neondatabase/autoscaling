package state_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/plugin/state"
	"github.com/neondatabase/autoscaling/pkg/util"
)

func TestPodStateExtraction(t *testing.T) {
	type resources struct {
		cpu vmv1.MilliCPU
		mem api.Bytes
	}

	type podObj struct {
		labels      map[string]string
		annotations map[string]string
		ownerRefs   []metav1.OwnerReference
		containers  []resources
	}

	type flags struct {
		migratable    bool
		alwaysMigrate bool
		migrating     bool
	}

	type extractedPod struct {
		vm    *util.NamespacedName
		flags *flags

		reserved  resources
		requested *resources
		factor    *resources
	}

	mib := 1024 * 1024

	cases := []struct {
		name string

		obj podObj

		extracted extractedPod
	}{
		{
			name: "normal-pod",
			obj: podObj{
				labels:      nil,
				annotations: nil,
				ownerRefs:   nil,
				containers: []resources{
					{
						cpu: vmv1.MilliCPU(250),
						mem: api.Bytes(256 * mib),
					},
					{
						cpu: vmv1.MilliCPU(500),
						mem: api.Bytes(1024 * mib),
					},
				},
			},
			extracted: extractedPod{
				vm:    nil,
				flags: nil,
				reserved: resources{
					cpu: vmv1.MilliCPU(750),
					mem: api.Bytes(1280 * mib),
				},
				requested: nil,
				factor:    nil,
			},
		},
		{
			name: "external-vm",
			obj: podObj{
				labels: nil,
				annotations: map[string]string{
					"vm.neon.tech/resources": `{
						"cpus": { "min": "500m", "use": "1000m", "max": "1500m" },
						"memorySlots": { "min": 1, "use": 2, "max": 3 },
						"memorySlotSize": "1Gi"
					}`,
				},
				ownerRefs: []metav1.OwnerReference{{
					APIVersion:         "vm.neon.tech/v1",
					Kind:               "VirtualMachine",
					Name:               "vm-name",
					UID:                "vm-uid",
					Controller:         lo.ToPtr(true),
					BlockOwnerDeletion: nil,
				}},
				containers: nil,
			},
			extracted: extractedPod{
				vm: &util.NamespacedName{
					Name:      "vm-name",
					Namespace: "test-namespace",
				},
				flags: nil,
				reserved: resources{
					cpu: vmv1.MilliCPU(1000),
					mem: api.Bytes(2048 * mib),
				},
				requested: nil,
				factor:    nil,
			},
		},
		{
			name: "autoscaling-requested-no-scheduler-approval",
			obj: podObj{
				labels: map[string]string{
					"autoscaling.neon.tech/enabled": "true",
				},
				annotations: map[string]string{
					"vm.neon.tech/resources": `{
						"cpus": { "min": "500m", "use": "1000m", "max": "1500m" },
						"memorySlots": { "min": 1, "use": 2, "max": 3 },
						"memorySlotSize": "1Gi"
					}`,
					"autoscaling.neon.tech/scaling-unit": `{
						"vCPUs": "500m",
						"mem": "1Gi"
					}`,
					"internal.autoscaling.neon.tech/resources-requested": `{
						"vCPUs": "1500m",
						"mem": "3Gi"
					}`,
				},
				ownerRefs: []metav1.OwnerReference{{
					APIVersion:         "vm.neon.tech/v1",
					Kind:               "VirtualMachine",
					Name:               "vm-name",
					UID:                "vm-uid",
					Controller:         lo.ToPtr(true),
					BlockOwnerDeletion: nil,
				}},
				containers: nil,
			},
			extracted: extractedPod{
				vm: &util.NamespacedName{
					Name:      "vm-name",
					Namespace: "test-namespace",
				},
				flags: nil,
				reserved: resources{
					cpu: vmv1.MilliCPU(1000),
					mem: api.Bytes(2048 * mib),
				},
				requested: &resources{
					cpu: vmv1.MilliCPU(1500),
					mem: api.Bytes(3072 * mib),
				},
				factor: &resources{
					cpu: vmv1.MilliCPU(500),
					mem: api.Bytes(1024 * mib),
				},
			},
		},
		{
			name: "autoscaling-full",
			obj: podObj{
				labels: map[string]string{
					"autoscaling.neon.tech/enabled": "true",
				},
				annotations: map[string]string{
					"vm.neon.tech/resources": `{
						"cpus": { "min": "1000m", "use": "1500m", "max": "2000m" },
						"memorySlots": { "min": 2, "use": 3, "max": 4 },
						"memorySlotSize": "1Gi"
					}`,
					"autoscaling.neon.tech/scaling-unit": `{
						"vCPUs": "500m",
						"mem": "1Gi"
					}`,
					"internal.autoscaling.neon.tech/resources-requested": `{
						"vCPUs": "1000m",
						"mem": "2Gi"
					}`,
					"internal.autoscaling.neon.tech/resources-approved": `{
						"vCPUs": "2000m",
						"mem": "4Gi"
					}`,
				},
				ownerRefs: []metav1.OwnerReference{{
					APIVersion:         "vm.neon.tech/v1",
					Kind:               "VirtualMachine",
					Name:               "vm-name",
					UID:                "vm-uid",
					Controller:         lo.ToPtr(true),
					BlockOwnerDeletion: nil,
				}},
				containers: nil,
			},
			extracted: extractedPod{
				vm: &util.NamespacedName{
					Name:      "vm-name",
					Namespace: "test-namespace",
				},
				flags: nil,
				reserved: resources{
					cpu: vmv1.MilliCPU(2000),
					mem: api.Bytes(4096 * mib),
				},
				requested: &resources{
					cpu: vmv1.MilliCPU(1000),
					mem: api.Bytes(2048 * mib),
				},
				factor: &resources{
					cpu: vmv1.MilliCPU(500),
					mem: api.Bytes(1024 * mib),
				},
			},
		},
		{
			name: "autoscaling-full-migratable",
			obj: podObj{
				labels: map[string]string{
					"autoscaling.neon.tech/auto-migration-enabled": "true",
					"autoscaling.neon.tech/enabled":                "true",
				},
				annotations: map[string]string{
					"vm.neon.tech/resources": `{
						"cpus": { "min": "1000m", "use": "1500m", "max": "2000m" },
						"memorySlots": { "min": 2, "use": 3, "max": 4 },
						"memorySlotSize": "1Gi"
					}`,
					"autoscaling.neon.tech/scaling-unit": `{
						"vCPUs": "500m",
						"mem": "1Gi"
					}`,
					"internal.autoscaling.neon.tech/resources-requested": `{
						"vCPUs": "1000m",
						"mem": "2Gi"
					}`,
					"internal.autoscaling.neon.tech/resources-approved": `{
						"vCPUs": "2000m",
						"mem": "4Gi"
					}`,
				},
				ownerRefs: []metav1.OwnerReference{{
					APIVersion:         "vm.neon.tech/v1",
					Kind:               "VirtualMachine",
					Name:               "vm-name",
					UID:                "vm-uid",
					Controller:         lo.ToPtr(true),
					BlockOwnerDeletion: nil,
				}},
				containers: nil,
			},
			extracted: extractedPod{
				vm: &util.NamespacedName{
					Name:      "vm-name",
					Namespace: "test-namespace",
				},
				flags: &flags{
					migratable:    true,
					alwaysMigrate: false,
					migrating:     false,
				},
				reserved: resources{
					cpu: vmv1.MilliCPU(2000),
					mem: api.Bytes(4096 * mib),
				},
				requested: &resources{
					cpu: vmv1.MilliCPU(1000),
					mem: api.Bytes(2048 * mib),
				},
				factor: &resources{
					cpu: vmv1.MilliCPU(500),
					mem: api.Bytes(1024 * mib),
				},
			},
		},
		{
			name: "autoscaling-full-ongoing-migration-source",
			obj: podObj{
				labels: map[string]string{
					"autoscaling.neon.tech/auto-migration-enabled": "true",
					"autoscaling.neon.tech/enabled":                "true",
				},
				annotations: map[string]string{
					"vm.neon.tech/resources": `{
						"cpus": { "min": "1000m", "use": "1500m", "max": "2000m" },
						"memorySlots": { "min": 2, "use": 3, "max": 4 },
						"memorySlotSize": "1Gi"
					}`,
					"autoscaling.neon.tech/scaling-unit": `{
						"vCPUs": "500m",
						"mem": "1Gi"
					}`,
					"internal.autoscaling.neon.tech/resources-requested": `{
						"vCPUs": "1000m",
						"mem": "2Gi"
					}`,
					"internal.autoscaling.neon.tech/resources-approved": `{
						"vCPUs": "2000m",
						"mem": "4Gi"
					}`,
				},
				ownerRefs: []metav1.OwnerReference{
					// Migration source pods are still "controlled" by the VirtualMachine object,
					// but the VirtualMachineMigration is a secondary owner.
					{
						APIVersion:         "vm.neon.tech/v1",
						Kind:               "VirtualMachine",
						Name:               "vm-name",
						UID:                "vm-uid",
						Controller:         lo.ToPtr(true),
						BlockOwnerDeletion: nil,
					},
					{
						APIVersion:         "vm.neon.tech/v1",
						Kind:               "VirtualMachineMigration",
						Name:               "vmm-name",
						UID:                "vmm-uid",
						Controller:         lo.ToPtr(false),
						BlockOwnerDeletion: nil,
					},
				},
				containers: nil,
			},
			extracted: extractedPod{
				vm: &util.NamespacedName{
					Name:      "vm-name",
					Namespace: "test-namespace",
				},
				flags: &flags{
					migratable:    true,
					alwaysMigrate: false,
					migrating:     true,
				},
				reserved: resources{
					cpu: vmv1.MilliCPU(2000),
					mem: api.Bytes(4096 * mib),
				},
				requested: &resources{
					cpu: vmv1.MilliCPU(1000),
					mem: api.Bytes(2048 * mib),
				},
				factor: &resources{
					cpu: vmv1.MilliCPU(500),
					mem: api.Bytes(1024 * mib),
				},
			},
		},
		{
			name: "autoscaling-full-ongoing-migration-target",
			obj: podObj{
				labels: map[string]string{
					"autoscaling.neon.tech/auto-migration-enabled": "true",
					"autoscaling.neon.tech/enabled":                "true",
				},
				annotations: map[string]string{
					"vm.neon.tech/resources": `{
						"cpus": { "min": "1000m", "use": "1500m", "max": "2000m" },
						"memorySlots": { "min": 2, "use": 3, "max": 4 },
						"memorySlotSize": "1Gi"
					}`,
					"autoscaling.neon.tech/scaling-unit": `{
						"vCPUs": "500m",
						"mem": "1Gi"
					}`,
					"internal.autoscaling.neon.tech/resources-requested": `{
						"vCPUs": "1000m",
						"mem": "2Gi"
					}`,
					"internal.autoscaling.neon.tech/resources-approved": `{
						"vCPUs": "2000m",
						"mem": "4Gi"
					}`,
				},
				ownerRefs: []metav1.OwnerReference{
					// Migration target pods are "controlled" by the VirtualMachineMigration object
					// and have the VirutalMachine object as a secondary owner.
					{
						APIVersion:         "vm.neon.tech/v1",
						Kind:               "VirtualMachineMigration",
						Name:               "vmm-name",
						UID:                "vmm-uid",
						Controller:         lo.ToPtr(true),
						BlockOwnerDeletion: nil,
					},
					{
						APIVersion:         "vm.neon.tech/v1",
						Kind:               "VirtualMachine",
						Name:               "vm-name",
						UID:                "vm-uid",
						Controller:         lo.ToPtr(false),
						BlockOwnerDeletion: nil,
					},
				},
				containers: nil,
			},
			extracted: extractedPod{
				vm: &util.NamespacedName{
					Name:      "vm-name",
					Namespace: "test-namespace",
				},
				flags: &flags{
					// migration target pods are not migratable, because they're currently receiving
					// a migration.
					migratable:    false,
					alwaysMigrate: false,
					// The migration *target* isn't migrating away; it's where the resources will
					// end up.
					migrating: false,
				},
				reserved: resources{
					cpu: vmv1.MilliCPU(2000),
					mem: api.Bytes(4096 * mib),
				},
				requested: &resources{
					cpu: vmv1.MilliCPU(1000),
					mem: api.Bytes(2048 * mib),
				},
				factor: &resources{
					cpu: vmv1.MilliCPU(500),
					mem: api.Bytes(1024 * mib),
				},
			},
		},
		{
			name: "autoscaling-full-finished-migration-source",
			obj: podObj{
				labels: map[string]string{
					"autoscaling.neon.tech/auto-migration-enabled": "true",
					"autoscaling.neon.tech/enabled":                "true",
				},
				annotations: map[string]string{
					"vm.neon.tech/resources": `{
						"cpus": { "min": "1000m", "use": "1500m", "max": "2000m" },
						"memorySlots": { "min": 2, "use": 3, "max": 4 },
						"memorySlotSize": "1Gi"
					}`,
					"autoscaling.neon.tech/scaling-unit": `{
						"vCPUs": "500m",
						"mem": "1Gi"
					}`,
					"internal.autoscaling.neon.tech/resources-requested": `{
						"vCPUs": "1000m",
						"mem": "2Gi"
					}`,
					"internal.autoscaling.neon.tech/resources-approved": `{
						"vCPUs": "2000m",
						"mem": "4Gi"
					}`,
				},
				ownerRefs: []metav1.OwnerReference{
					// Once the migration is complete, the source pod is left with the
					// VirtualMachine and VirtualMachineMigration objects as owners but no
					// controlling owner reference.
					{
						APIVersion:         "vm.neon.tech/v1",
						Kind:               "VirtualMachineMigration",
						Name:               "vmm-name",
						UID:                "vmm-uid",
						Controller:         lo.ToPtr(false),
						BlockOwnerDeletion: nil,
					},
					{
						APIVersion:         "vm.neon.tech/v1",
						Kind:               "VirtualMachine",
						Name:               "vm-name",
						UID:                "vm-uid",
						Controller:         lo.ToPtr(false),
						BlockOwnerDeletion: nil,
					},
				},
				containers: nil,
			},
			extracted: extractedPod{
				vm: &util.NamespacedName{
					Name:      "vm-name",
					Namespace: "test-namespace",
				},
				flags: &flags{
					migratable:    true,
					alwaysMigrate: false,
					// We'll consider this as still kind of migrating, because it'll soon disappear
					// due to the migration being complete.
					migrating: true,
				},
				reserved: resources{
					cpu: vmv1.MilliCPU(2000),
					mem: api.Bytes(4096 * mib),
				},
				requested: &resources{
					cpu: vmv1.MilliCPU(1000),
					mem: api.Bytes(2048 * mib),
				},
				factor: &resources{
					cpu: vmv1.MilliCPU(500),
					mem: api.Bytes(1024 * mib),
				},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Jan 1 2000 at 00:00:00 UTC
			createdAt := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

			obj := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "pod-name",
					Namespace:         "test-namespace",
					UID:               "pod-uid",
					CreationTimestamp: metav1.NewTime(createdAt),
					Labels:            c.obj.labels,
					Annotations:       c.obj.annotations,
					OwnerReferences:   c.obj.ownerRefs,
				},
				Spec: corev1.PodSpec{
					Containers: lo.Map(c.obj.containers, func(r resources, idx int) corev1.Container {
						return corev1.Container{
							Name: fmt.Sprintf("container-%d", idx),
							Resources: corev1.ResourceRequirements{
								Limits: nil,
								Requests: map[corev1.ResourceName]resource.Quantity{
									corev1.ResourceCPU:    *r.cpu.ToResourceQuantity(),
									corev1.ResourceMemory: *r.mem.ToResourceQuantity(),
								},
								Claims: nil,
							},
						}
					}),
				},
			}

			expectedPod := state.Pod{
				NamespacedName: util.NamespacedName{
					Name:      "pod-name",
					Namespace: "test-namespace",
				},
				UID:            "pod-uid",
				CreatedAt:      createdAt,
				VirtualMachine: lo.FromPtr(c.extracted.vm),
				Migratable:     lo.FromPtr(c.extracted.flags).migratable,
				AlwaysMigrate:  lo.FromPtr(c.extracted.flags).alwaysMigrate,
				Migrating:      lo.FromPtr(c.extracted.flags).migrating,
				CPU: state.PodResources[vmv1.MilliCPU]{
					Reserved:  c.extracted.reserved.cpu,
					Requested: lo.FromPtrOr(c.extracted.requested, c.extracted.reserved).cpu,
					Factor:    lo.FromPtr(c.extracted.factor).cpu,
				},
				Mem: state.PodResources[api.Bytes]{
					Reserved:  c.extracted.reserved.mem,
					Requested: lo.FromPtrOr(c.extracted.requested, c.extracted.reserved).mem,
					Factor:    lo.FromPtr(c.extracted.factor).mem,
				},
			}

			pod, err := state.PodStateFromK8sObj(obj)
			if err != nil {
				t.Error("failed to extract pod state: ", err.Error())
			}

			assert.Equal(t, expectedPod, pod)
		})
	}
}
