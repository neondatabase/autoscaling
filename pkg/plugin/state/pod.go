package state

import (
	"errors"
	"time"

	"github.com/samber/lo"
	"go.uber.org/zap/zapcore"
	"golang.org/x/exp/constraints"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/api"
	"github.com/neondatabase/autoscaling/pkg/util"
)

type Pod struct {
	// NOTE: It's important that Pod objects contain no references, otherwise speculative changes on
	// a node could accidentally leak through after choosing not to commit them.

	util.NamespacedName
	UID       types.UID
	CreatedAt time.Time

	// VirtualMachine, if not empty, gives the name of the VirtualMachine object that owns this Pod.
	VirtualMachine util.NamespacedName

	// Migratable is true if this Pod is owned by a VirtualMachine and it has the appropriate label
	// to enable automatic live migration.
	Migratable bool

	// AlwaysMigrate is true if this Pod is owned by a VirtualMachine and it has the (TESTING ONLY)
	// label to mark that this pod should be continuously migrated.
	AlwaysMigrate bool

	// Migrating is true iff there is a VirtualMachineMigration with this pod as the source.
	Migrating bool

	CPU PodResources[vmv1.MilliCPU]
	Mem PodResources[api.Bytes]
}

// MarshalLogObject implements zapcore.ObjectMarshaler so that Pod can be used with zap.Object.
func (p Pod) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("Namespace", p.Namespace)
	enc.AddString("Name", p.Name)
	enc.AddString("UID", string(p.UID))
	enc.AddTime("CreatedAt", p.CreatedAt)
	if !lo.IsEmpty(p.VirtualMachine) {
		if err := enc.AddObject("VirtualMachine", p.VirtualMachine); err != nil {
			return err
		}
		enc.AddBool("Migratable", p.Migratable)
		enc.AddBool("AlwaysMigrate", p.AlwaysMigrate)
		enc.AddBool("Migrating", p.Migrating)
	}
	if err := enc.AddReflected("CPU", p.CPU); err != nil {
		return err
	}
	if err := enc.AddReflected("Mem", p.Mem); err != nil {
		return err
	}
	return nil
}

// PodResources is the extracted resources reserved and desired by the pod
type PodResources[T constraints.Unsigned] struct {
	// Reserved is the amount of T that has been set aside for usage by this Pod.
	//
	// For a regular pod, this is simply the sum of the resource requests for its containers, but
	// for a VM, it is equal to the last resources that the scheduler has approved for the
	// autoscaler-agent OR the CPU/Memory '.use' field, if that has not yet happened.
	Reserved T

	// Requested is the amount of T that the Pod would like to have available to it.
	//
	// For a regular Pod, this is exactly equal to Reserved.
	//
	// For a VM, this is equal to the last amount that the autoscaler-agent requested -- or if that
	// hasn't happened yet, simply Reserved.
	//
	// If Requested is ever less than Reserved, the scheduler is expected to immediately reduce
	// Reserved -- in effect, it's been given back resources that it previously set aside.
	Requested T

	// Factor is the smallest incremental change in T that can be allocated to the pod.
	//
	// For pods that aren't VMs, this should be set to zero, as it has no impact.
	Factor T
}

func PodStateFromK8sObj(pod *corev1.Pod) (Pod, error) {
	if vmRef, ok := vmv1.VirtualMachineOwnerForPod(pod); ok {
		return podStateForVMRunner(pod, vmRef)
	} else {
		return podStateForNormalPod(pod), nil
	}
}

func podStateForNormalPod(pod *corev1.Pod) Pod {
	// this pod is *not* a VM runner pod -- we should use the standard kubernetes resources.

	var cpu vmv1.MilliCPU
	var mem api.Bytes
	for _, container := range pod.Spec.Containers {
		// For each resource, add the requests, if they're provided.
		//
		// NB: .Cpu()/.Memory() return a pointer to a value equal to zero if the resource is not
		// present. So we can just add it either way.
		cpu += vmv1.MilliCPUFromResourceQuantity(*container.Resources.Requests.Cpu())
		mem += api.BytesFromResourceQuantity(*container.Resources.Requests.Memory())
	}

	return Pod{
		NamespacedName: util.GetNamespacedName(pod),
		UID:            pod.UID,
		CreatedAt:      pod.CreationTimestamp.Time,

		VirtualMachine: lo.Empty[util.NamespacedName](),
		Migratable:     false,
		AlwaysMigrate:  false,
		Migrating:      false,

		CPU: PodResources[vmv1.MilliCPU]{
			Reserved:  cpu,
			Requested: cpu,
			Factor:    0,
		},
		Mem: PodResources[api.Bytes]{
			Reserved:  mem,
			Requested: mem,
			Factor:    0,
		},
	}
}

func podStateForVMRunner(pod *corev1.Pod, vmRef metav1.OwnerReference) (Pod, error) {
	// this pod is a VM runner pod
	vm := util.NamespacedName{Namespace: pod.Namespace, Name: vmRef.Name}

	_, migrationRole, ownedByMigration := vmv1.MigrationOwnerForPod(pod)

	alwaysMigrate := api.HasAlwaysMigrateLabel(pod)
	autoMigrate := api.HasAutoMigrationEnabled(pod)

	migrating := ownedByMigration && migrationRole == vmv1.MigrationRoleSource
	// allow ongoing migrations to continue. Don't allow migrations of current migration
	// targets. New migrations can be started when auto migrations are enabled, or if the
	// testing-only "always migrate" flag is enabled.
	migratable := migrating || (migrationRole != vmv1.MigrationRoleTarget && (autoMigrate || alwaysMigrate))

	autoscalable := api.HasAutoscalingEnabled(pod)

	res, err := vmv1.VirtualMachineResourcesFromPod(pod)
	if err != nil {
		return lo.Empty[Pod](), err
	}

	actualResources := &api.Resources{
		VCPU: res.CPUs.Use,
		Mem:  api.BytesFromResourceQuantity(res.MemorySlotSize) * api.Bytes(res.MemorySlots.Use),
	}

	var scalingUnit, requested, approved *api.Resources

	if !autoscalable {
		approved = actualResources
		requested = actualResources
	} else {
		scalingUnit, err = api.ExtractScalingUnit(pod)
		if err != nil {
			return lo.Empty[Pod](), err
		}

		requested, err = api.ExtractRequestedScaling(pod)
		if err != nil {
			return lo.Empty[Pod](), err
		} else if requested == nil {
			requested = actualResources
		} else {
			// We cannot have requested scaling but no scaling unit -- disallow that here.
			if scalingUnit == nil {
				return lo.Empty[Pod](), errors.New("Pod has requested scaling but no scaling unit annotation")
			}
		}

		approved, err = api.ExtractApprovedScaling(pod)
		if err != nil {
			return lo.Empty[Pod](), err
		} else if approved == nil {
			approved = actualResources
		}
	}

	if scalingUnit == nil {
		// default the scaling unit to zero; if we got here, it's not needed.
		scalingUnit = &api.Resources{
			VCPU: 0,
			Mem:  0,
		}
	}

	return Pod{
		NamespacedName: util.GetNamespacedName(pod),
		UID:            pod.UID,
		CreatedAt:      pod.CreationTimestamp.Time,

		VirtualMachine: vm,
		Migratable:     migratable,
		AlwaysMigrate:  alwaysMigrate,
		Migrating:      migrating,

		CPU: PodResources[vmv1.MilliCPU]{
			Reserved:  approved.VCPU,
			Requested: requested.VCPU,
			Factor:    scalingUnit.VCPU,
		},
		Mem: PodResources[api.Bytes]{
			Reserved:  approved.Mem,
			Requested: requested.Mem,
			Factor:    scalingUnit.Mem,
		},
	}, nil
}

// BetterMigrationTargetThan returns true iff the pod is a better migration target than the 'other'
// pod.
func (p Pod) BetterMigrationTargetThan(other Pod) bool {
	// For now, just prioritize migration for older pods, so that we naturally avoid continuously
	// re-migrating the same VMs.
	return p.CreatedAt.Before(other.CreatedAt)
}
