package controllers

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitalocean/go-qemu/qmp"

	"k8s.io/apimachinery/pkg/api/resource"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

type QmpCpus struct {
	Return []struct {
		Props struct {
			CoreId   int32 `json:"core-id"`
			ThreadId int32 `json:"thread-id"`
			SocketId int32 `json:"socket-id"`
		} `json:"props"`
		VcpusCount int32   `json:"vcpus-count"`
		QomPath    *string `json:"qom-path"`
		Type       string  `json:"type"`
	} `json:"return"`
}

type QmpMemorySize struct {
	Return struct {
		BaseMemory    int64 `json:"base-memory"`
		PluggedMemory int64 `json:"plugged-memory"`
	} `json:"return"`
}

type QmpCpuSlot struct {
	Core int32  `json:"core"`
	QOM  string `json:"qom"`
	Type string `json:"type"`
}

type QmpMemoryDevices struct {
	Return []QmpMemoryDevice `json:"return"`
}

type QmpMemoryDevice struct {
	Type string `json:"type"`
	Data struct {
		Memdev       string `json:"memdev"`
		Hotplugged   bool   `json:"hotplugged"`
		Addr         int64  `json:"addr"`
		Hotplugguble bool   `json:"hotpluggable"`
		Size         int64  `json:"size"`
		Slot         int64  `json:"slot"`
		Node         int64  `json:"node"`
		Id           string `json:"id"`
	} `json:"data"`
}

type QmpObjects struct {
	Return []QmpObject `json:"return"`
}

type QmpObject struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type QmpMigrationInfo struct {
	Return MigrationInfo `json:"return"`
}

type MigrationInfo struct {
	Status      string `json:"status"`
	TotalTimeMs int64  `json:"total-time"`
	SetupTimeMs int64  `json:"setup-time"`
	DowntimeMs  int64  `json:"downtime"`
	Ram         struct {
		Transferred    int64 `json:"transferred"`
		Remaining      int64 `json:"remaining"`
		Total          int64 `json:"total"`
		Duplicate      int64 `json:"duplicate"`
		Normal         int64 `json:"normal"`
		NormalBytes    int64 `json:"normal-bytes"`
		DirtySyncCount int64 `json:"dirty-sync-count"`
	} `json:"ram"`
	Compression struct {
		CompressedSize  int64   `json:"compressed-size"`
		CompressionRate float64 `json:"compression-rate"`
	} `json:"compression"`
}

func QmpAddr(vm *vmv1.VirtualMachine) (ip string, port int32) {
	return vm.Status.PodIP, vm.Spec.QMP
}

func QmpConnect(ip string, port int32) (*qmp.SocketMonitor, error) {
	mon, err := qmp.NewSocketMonitor("tcp", fmt.Sprintf("%s:%d", ip, port), 2*time.Second)
	if err != nil {
		return nil, err
	}
	if err := mon.Connect(); err != nil {
		return nil, err
	}

	return mon, nil
}

func QmpGetCpus(ip string, port int32) ([]QmpCpuSlot, []QmpCpuSlot, error) {
	mon, err := QmpConnect(ip, port)
	if err != nil {
		return nil, nil, err
	}
	defer mon.Disconnect() //nolint:errcheck // nothing to do with error when deferred. TODO: log it?

	qmpcmd := []byte(`{"execute": "query-hotpluggable-cpus"}`)
	raw, err := mon.Run(qmpcmd)
	if err != nil {
		return nil, nil, err
	}

	var result QmpCpus
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, nil, fmt.Errorf("error unmarshaling json: %w", err)
	}

	plugged := []QmpCpuSlot{}
	empty := []QmpCpuSlot{}
	for _, entry := range result.Return {
		if entry.QomPath != nil {
			plugged = append(plugged, QmpCpuSlot{Core: entry.Props.CoreId, QOM: *entry.QomPath, Type: entry.Type})
		} else {
			empty = append(empty, QmpCpuSlot{Core: entry.Props.CoreId, QOM: "", Type: entry.Type})
		}
	}

	return plugged, empty, nil
}

func QmpPlugCpu(ip string, port int32) error {
	_, empty, err := QmpGetCpus(ip, port)
	if err != nil {
		return err
	}
	if len(empty) == 0 {
		return errors.New("no empty slots for CPU hotplug")
	}

	mon, err := QmpConnect(ip, port)
	if err != nil {
		return err
	}
	defer mon.Disconnect() //nolint:errcheck // nothing to do with error when deferred. TODO: log it?

	// empty list reversed, first cpu slot in the end of list and last cpu slot in the beginning
	slot := empty[len(empty)-1]
	qmpcmd := []byte(fmt.Sprintf(`{
		"execute": "device_add",
		"arguments": {
			"id": "cpu%d",
			"driver": %q,
			"core-id": %d,
			"socket-id": 0,
			"thread-id": 0
		}
	}`, slot.Core, slot.Type, slot.Core))

	_, err = mon.Run(qmpcmd)
	if err != nil {
		return err
	}

	return nil
}

func QmpUnplugCpu(ip string, port int32) error {
	plugged, _, err := QmpGetCpus(ip, port)
	if err != nil {
		return err
	}

	slot := -1
	found := false
	for i, s := range plugged {
		if strings.Contains(s.QOM, "machine/peripheral/cpu") {
			found = true
			slot = i
			break
		}
	}
	if !found {
		return errors.New("there are no unpluggable CPUs")
	}

	mon, err := QmpConnect(ip, port)
	if err != nil {
		return err
	}
	defer mon.Disconnect() //nolint:errcheck // nothing to do with error when deferred. TODO: log it?

	cmd := []byte(fmt.Sprintf(`{"execute": "device_del", "arguments": {"id": %q}}`, plugged[slot].QOM))
	_, err = mon.Run(cmd)
	if err != nil {
		return err
	}
	// small pause to let hypervisor do unplug
	time.Sleep(500 * time.Millisecond)

	return nil
}

func QmpSyncCpuToTarget(vm *vmv1.VirtualMachine, migration *vmv1.VirtualMachineMigration) error {
	plugged, _, err := QmpGetCpus(QmpAddr(vm))
	if err != nil {
		return err
	}
	pluggedInTarget, _, err := QmpGetCpus(migration.Status.TargetPodIP, vm.Spec.QMP)
	if err != nil {
		return err
	}
	if len(plugged) == len(pluggedInTarget) {
		// no need plug anything
		return nil
	}

	target, err := QmpConnect(migration.Status.TargetPodIP, vm.Spec.QMP)
	if err != nil {
		return err
	}
	defer target.Disconnect() //nolint:errcheck // nothing to do with error when deferred. TODO: log it?

searchForEmpty:
	for _, slot := range plugged {
		// firsly check if slot occupied already
		// run over Target CPUs and compare with source
		for _, tslot := range pluggedInTarget {
			if slot == tslot {
				// that mean such CPU already present in Target, skip it
				continue searchForEmpty
			}
		}
		qmpcmd := []byte(fmt.Sprintf(`{
			"execute": "device_add",
			"arguments": {
				"id": "cpu%d",
				"driver": %q,
				"core-id": %d,
				"socket-id": 0,
				"thread-id": 0
			}
		}`, slot.Core, slot.Type, slot.Core))
		_, err = target.Run(qmpcmd)
		if err != nil {
			return err
		}
	}

	return nil
}

func QmpQueryMemoryDevices(ip string, port int32) ([]QmpMemoryDevice, error) {
	mon, err := QmpConnect(ip, port)
	if err != nil {
		return nil, err
	}
	defer mon.Disconnect() //nolint:errcheck // nothing to do with error when deferred. TODO: log it?

	var result QmpMemoryDevices
	cmd := []byte(`{"execute": "query-memory-devices"}`)
	raw, err := mon.Run(cmd)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("error unmarshaling json: %w", err)
	}
	return result.Return, nil
}

// MemslotIdxFromName takes "/objects/memslot3" and returns 3
func MemslotIdxFromName(name string) (int, error) {
	idxStr := strings.TrimPrefix(name, "/objects/memslot")
	idx, err := strconv.Atoi(idxStr)
	if err != nil {
		// doesn't reference `err`, because we don't know the actual issue
		return 0, fmt.Errorf("failed to parse memory device id: %q", name)
	}
	return idx, nil
}

func QmpQueryMemoryBackendIds(mon *qmp.SocketMonitor) (map[int]struct{}, error) {
	cmd := []byte(`{"execute": "qom-list", "arguments": {"path": "/objects"}}`)
	raw, err := mon.Run(cmd)
	if err != nil {
		return nil, err
	}

	var result QmpObjects
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("error unmarshaling json: %w", err)
	}
	backends := map[int]struct{}{}
	for _, o := range result.Return {
		if o.Name == "pc.ram" { // Non-hotplugged memory
			continue
		}
		if o.Type != "child<memory-backend-ram>" {
			continue
		}

		idx, err := MemslotIdxFromName(o.Name)
		if err != nil {
			return nil, err
		}
		backends[idx] = struct{}{}
	}
	return backends, nil
}

// QmpAddMemoryBackend adds a single memory slot to the VM with the given size.
//
// The memory slot does nothing until a corresponding "device" is added to the VM for the same memory slot.
// See QmpAddMemoryDevice for more.
// When unplugging, QmpDelMemoryDevice must be called before QmpDelMemoryBackend.
func QmpAddMemoryBackend(mon *qmp.SocketMonitor, idx int, sizeBytes int64) error {
	cmd := []byte(fmt.Sprintf(
		`{"execute": "object-add",
		  "arguments": {"id": "memslot%d",
						"size": %d,
						"qom-type": "memory-backend-ram"}}`, idx, sizeBytes,
	))
	_, err := mon.Run(cmd)
	return err
}

func QmpDelMemoryBackend(mon *qmp.SocketMonitor, idx int) error {
	cmd := []byte(fmt.Sprintf(
		`{"execute": "object-del",
		  "arguments": {"id": "memslot%d"}}`, idx,
	))
	_, err := mon.Run(cmd)
	return err
}

func QmpAddMemoryDevice(mon *qmp.SocketMonitor, idx int) error {
	cmd := []byte(fmt.Sprintf(
		`{"execute": "device_add",
		  "arguments": {"id": "dimm%d",
						"driver": "pc-dimm",
						"memdev": "memslot%d"}}`, idx, idx,
	))
	_, err := mon.Run(cmd)
	return err
}

func QmpDelMemoryDevice(mon *qmp.SocketMonitor, idx int) error {
	cmd := []byte(fmt.Sprintf(
		`{"execute": "device_del",
		  "arguments": {"id": "dimm%d"}}`, idx,
	))
	_, err := mon.Run(cmd)
	return err
}

func QmpPlugMemory(virtualmachine *vmv1.VirtualMachine) error {
	// slots - number of pluggable memory slots (Max - Min)
	slots := *virtualmachine.Spec.Guest.MemorySlots.Max - *virtualmachine.Spec.Guest.MemorySlots.Min

	memoryDevices, err := QmpQueryMemoryDevices(QmpAddr(virtualmachine))
	if err != nil {
		return err
	}

	// check if available mmory slots present
	plugged := int32(len(memoryDevices))
	if plugged >= slots {
		return errors.New("no empty slots for Memory hotplug")
	}

	mon, err := QmpConnect(QmpAddr(virtualmachine))
	if err != nil {
		return err
	}
	defer mon.Disconnect() //nolint:errcheck // nothing to do with error when deferred. TODO: log it?

	// try to find empty slot
	backends, err := QmpQueryMemoryBackendIds(mon)
	if err != nil {
		return err
	}
	var slot int
	for slot = 1; slot <= int(slots); slot++ {
		_, ok := backends[slot]
		if !ok {
			// that mean such object wasn't found, or by other words slot is empty
			break
		}
	}

	err = QmpAddMemoryBackend(mon, slot, virtualmachine.Spec.Guest.MemorySlotSize.Value())
	if err != nil {
		return err
	}
	// now add pc-dimm device
	err = QmpAddMemoryDevice(mon, slot)
	if err != nil {
		// device_add command failed... so try remove object that we just created
		_ = QmpDelMemoryBackend(mon, slot)
		return err
	}

	return nil
}

func QmpSyncMemoryToTarget(vm *vmv1.VirtualMachine, migration *vmv1.VirtualMachineMigration) error {
	memoryDevices, err := QmpQueryMemoryDevices(QmpAddr(vm))
	if err != nil {
		return err
	}
	memoryDevicesInTarget, err := QmpQueryMemoryDevices(migration.Status.TargetPodIP, vm.Spec.QMP)
	if err != nil {
		return err
	}

	target, err := QmpConnect(migration.Status.TargetPodIP, vm.Spec.QMP)
	if err != nil {
		return err
	}
	defer target.Disconnect() //nolint:errcheck // nothing to do with error when deferred. TODO: log it?

	for _, m := range memoryDevices {
		// firsly check if slot occupied already
		// run over Target memory and compare device id
		found := false
		for _, tm := range memoryDevicesInTarget {
			if DeepEqual(m, tm) {
				found = true
			}
		}
		if found {
			// that mean such memory device 'm' already present in Target, skip it
			continue
		}
		// add memdev object
		memdevIdx, err := MemslotIdxFromName(m.Data.Memdev)
		if err != nil {
			return err
		}
		err = QmpAddMemoryBackend(target, memdevIdx, m.Data.Size)
		if err != nil {
			return err
		}
		// now add pc-dimm device
		err = QmpAddMemoryDevice(target, memdevIdx)
		if err != nil {
			// device_add command failed... so try remove object that we just created
			_ = QmpDelMemoryBackend(target, memdevIdx)
			return err
		}
	}

	return nil
}

func QmpUnplugMemory(ip string, port int32) error {
	memoryDevices, err := QmpQueryMemoryDevices(ip, port)
	if err != nil {
		return err
	}
	plugged := len(memoryDevices)
	if plugged == 0 {
		return errors.New("there are no unpluggable Memory slots")
	}

	mon, err := QmpConnect(ip, port)
	if err != nil {
		return err
	}
	defer mon.Disconnect() //nolint:errcheck // nothing to do with error when deferred. TODO: log it?

	// run from last to first
	var i int
	var merr error
	for i = plugged - 1; i >= 0; i-- {
		// remove pc-dimm device
		idx, err := MemslotIdxFromName(memoryDevices[i].Data.Memdev)
		if err != nil {
			merr = errors.Join(merr, err)
			continue
		}
		err = QmpDelMemoryDevice(mon, idx)
		if err != nil {
			merr = errors.Join(merr, err)
			continue
		}
		// wait a bit to allow guest kernel remove memory block
		time.Sleep(time.Second)

		// remove corresponding memdev object
		err = QmpDelMemoryBackend(mon, idx)
		if err != nil {
			merr = errors.Join(merr, err)
			continue
		}
		// successfully deleted memory device
		break
	}
	if i >= 0 {
		// some memory device was removed
		return nil
	}

	return merr
}

func QmpGetMemorySize(ip string, port int32) (*resource.Quantity, error) {
	mon, err := QmpConnect(ip, port)
	if err != nil {
		return nil, err
	}
	defer mon.Disconnect() //nolint:errcheck // nothing to do with error when deferred. TODO: log it?

	qmpcmd := []byte(`{"execute": "query-memory-size-summary"}`)
	raw, err := mon.Run(qmpcmd)
	if err != nil {
		return nil, err
	}

	var result QmpMemorySize
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("error unmarshaling json: %w", err)
	}

	return resource.NewQuantity(result.Return.BaseMemory+result.Return.PluggedMemory, resource.BinarySI), nil
}

func QmpStartMigration(virtualmachine *vmv1.VirtualMachine, virtualmachinemigration *vmv1.VirtualMachineMigration) error {

	// QMP port
	port := virtualmachine.Spec.QMP

	// connect to source runner QMP
	s_ip := virtualmachinemigration.Status.SourcePodIP
	smon, err := qmp.NewSocketMonitor("tcp", fmt.Sprintf("%s:%d", s_ip, port), 2*time.Second)
	if err != nil {
		return err
	}
	if err := smon.Connect(); err != nil {
		return err
	}
	defer smon.Disconnect() //nolint:errcheck // nothing to do with error when deferred. TODO: log it?

	// connect to target runner QMP
	t_ip := virtualmachinemigration.Status.TargetPodIP
	tmon, err := qmp.NewSocketMonitor("tcp", fmt.Sprintf("%s:%d", t_ip, port), 2*time.Second)
	if err != nil {
		return err
	}
	if err := tmon.Connect(); err != nil {
		return err
	}
	defer tmon.Disconnect() //nolint:errcheck // nothing to do with error when deferred. TODO: log it?

	cache := resource.MustParse("256Mi")
	var qmpcmd []byte
	// setup migration on source runner
	qmpcmd = []byte(fmt.Sprintf(`{
		"execute": "migrate-set-capabilities",
		"arguments":
		    {
			"capabilities": [
			    {"capability": "postcopy-ram",  "state": %t},
			    {"capability": "xbzrle",        "state": true},
			    {"capability": "compress",      "state": true},
			    {"capability": "auto-converge", "state": %t},
			    {"capability": "zero-blocks",   "state": true}
			]
		    }
		}`, virtualmachinemigration.Spec.AllowPostCopy, virtualmachinemigration.Spec.AutoConverge))
	_, err = smon.Run(qmpcmd)
	if err != nil {
		return err
	}
	qmpcmd = []byte(fmt.Sprintf(`{
		"execute": "migrate-set-parameters",
		"arguments":
		    {
			"xbzrle-cache-size":   %d,
			"max-bandwidth":       %d,
			"multifd-compression": "zstd"
		    }
		}`, cache.Value(), virtualmachinemigration.Spec.MaxBandwidth.Value()))
	_, err = smon.Run(qmpcmd)
	if err != nil {
		return err
	}

	// setup migration on target runner
	qmpcmd = []byte(fmt.Sprintf(`{
		"execute": "migrate-set-capabilities",
		"arguments":
		    {
			"capabilities": [
			    {"capability": "postcopy-ram",  "state": %t},
			    {"capability": "xbzrle",        "state": true},
			    {"capability": "compress",      "state": true},
			    {"capability": "auto-converge", "state": %t},
			    {"capability": "zero-blocks",   "state": true}
			]
		    }
		}`, virtualmachinemigration.Spec.AllowPostCopy, virtualmachinemigration.Spec.AutoConverge))
	_, err = tmon.Run(qmpcmd)
	if err != nil {
		return err
	}
	qmpcmd = []byte(fmt.Sprintf(`{
		"execute": "migrate-set-parameters",
		"arguments":
		    {
			"xbzrle-cache-size":   %d,
			"max-bandwidth":       %d,
			"multifd-compression": "zstd"
		    }
		}`, cache.Value(), virtualmachinemigration.Spec.MaxBandwidth.Value()))
	_, err = tmon.Run(qmpcmd)
	if err != nil {
		return err
	}

	// trigger migration
	qmpcmd = []byte(fmt.Sprintf(`{
		"execute": "migrate",
		"arguments":
		    {
			"uri": "tcp:%s:%d",
			"inc": %t,
			"blk": %t
		    }
		}`, t_ip, vmv1.MigrationPort, virtualmachinemigration.Spec.Incremental, !virtualmachinemigration.Spec.Incremental))
	_, err = smon.Run(qmpcmd)
	if err != nil {
		return err
	}
	if virtualmachinemigration.Spec.AllowPostCopy {
		qmpcmd = []byte(`{"execute": "migrate-start-postcopy"}`)
		_, err = smon.Run(qmpcmd)
		if err != nil {
			return err
		}
	}

	return nil
}

func QmpGetMigrationInfo(ip string, port int32) (*MigrationInfo, error) {
	mon, err := QmpConnect(ip, port)
	if err != nil {
		return nil, err
	}
	defer mon.Disconnect() //nolint:errcheck // nothing to do with error when deferred. TODO: log it?

	qmpcmd := []byte(`{"execute": "query-migrate"}`)
	raw, err := mon.Run(qmpcmd)
	if err != nil {
		return nil, err
	}

	var result QmpMigrationInfo
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("error unmarshaling json: %w", err)
	}

	return &result.Return, nil
}

func QmpCancelMigration(ip string, port int32) error {
	mon, err := QmpConnect(ip, port)
	if err != nil {
		return err
	}
	defer mon.Disconnect() //nolint:errcheck // nothing to do with error when deferred. TODO: log it?

	qmpcmd := []byte(`{"execute": "migrate_cancel"}`)
	_, err = mon.Run(qmpcmd)
	if err != nil {
		return err
	}

	return nil
}

func QmpQuit(ip string, port int32) error {
	mon, err := QmpConnect(ip, port)
	if err != nil {
		return err
	}
	defer mon.Disconnect() //nolint:errcheck // nothing to do with error when deferred. TODO: log it?

	qmpcmd := []byte(`{"execute": "quit"}`)
	_, err = mon.Run(qmpcmd)
	if err != nil {
		return err
	}

	return nil
}
