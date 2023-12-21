package controllers

import (
	"encoding/json"
	"errors"
	"fmt"
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
	defer mon.Disconnect()

	qmpcmd := []byte(`{"execute": "query-hotpluggable-cpus"}`)
	raw, err := mon.Run(qmpcmd)
	if err != nil {
		return nil, nil, err
	}

	var result QmpCpus
	json.Unmarshal(raw, &result)

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
	defer mon.Disconnect()

	// empty list reversed, first cpu slot in the end of list and last cpu slot in the beginning
	slot := empty[len(empty)-1]
	qmpcmd := []byte(fmt.Sprintf(`{"execute": "device_add", "arguments": {"id": "cpu%d", "driver": "%s", "core-id": %d, "socket-id": 0,  "thread-id": 0}}`, slot.Core, slot.Type, slot.Core))

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
	defer mon.Disconnect()

	cmd := []byte(fmt.Sprintf(`{"execute": "device_del", "arguments": {"id": "%s"}}`, plugged[slot].QOM))
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
	defer target.Disconnect()

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
		qmpcmd := []byte(fmt.Sprintf(`{"execute": "device_add", "arguments": {"id": "cpu%d", "driver": "%s", "core-id": %d, "socket-id": 0,  "thread-id": 0}}`, slot.Core, slot.Type, slot.Core))
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
	defer mon.Disconnect()

	var result QmpMemoryDevices
	cmd := []byte(`{"execute": "query-memory-devices"}`)
	raw, err := mon.Run(cmd)
	if err != nil {
		return nil, err
	}
	json.Unmarshal(raw, &result)
	return result.Return, nil
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
	defer mon.Disconnect()

	// try to find empty slot
	var slot int32
	for slot = 1; slot <= slots; slot++ {
		cmd := []byte(fmt.Sprintf(`{"execute": "qom-list", "arguments": {"path": "/objects/memslot%d"}}`, slot))
		_, err = mon.Run(cmd)
		if err != nil {
			// that mean such object wasn't found, or by other words slot is empty
			break
		}
	}

	// add memdev object
	cmd := []byte(fmt.Sprintf(`{"execute": "object-add", "arguments": {"id": "memslot%d", "size": %d, "qom-type": "memory-backend-ram"}}`, slot, virtualmachine.Spec.Guest.MemorySlotSize.Value()))
	_, err = mon.Run(cmd)
	if err != nil {
		return err
	}
	// now add pc-dimm device
	cmd = []byte(fmt.Sprintf(`{"execute": "device_add", "arguments": {"id": "dimm%d", "driver": "pc-dimm", "memdev": "memslot%d"}}`, slot, slot))
	_, err = mon.Run(cmd)
	if err != nil {
		// device_add command failed... so try remove object that we just created
		cmd = []byte(fmt.Sprintf(`{"execute": "object-del", "arguments": {"id": "memslot%d"}}`, slot))
		mon.Run(cmd)
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
	defer target.Disconnect()

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
		memdevId := strings.ReplaceAll(m.Data.Memdev, "/objects/", "")
		cmd := []byte(fmt.Sprintf(`{"execute": "object-add", "arguments": {"id": "%s", "size": %d, "qom-type": "memory-backend-ram"}}`, memdevId, m.Data.Size))
		_, err = target.Run(cmd)
		if err != nil {
			return err
		}
		// now add pc-dimm device
		cmd = []byte(fmt.Sprintf(`{"execute": "device_add", "arguments": {"id": "%s", "driver": "pc-dimm", "memdev": "%s"}}`, m.Data.Id, memdevId))
		_, err = target.Run(cmd)
		if err != nil {
			// device_add command failed... so try remove object that we just created
			cmd = []byte(fmt.Sprintf(`{"execute": "object-del", "arguments": {"id": "%s"}}`, m.Data.Memdev))
			target.Run(cmd)
			return err
		}
	}

	return nil
}

func QmpPlugMemoryToRunner(ip string, port int32, size int64) error {
	memoryDevices, err := QmpQueryMemoryDevices(ip, port)
	if err != nil {
		return err
	}
	plugged := int32(len(memoryDevices))

	mon, err := QmpConnect(ip, port)
	if err != nil {
		return err
	}
	defer mon.Disconnect()

	// add memdev object for next slot
	// firstly check if such object already present to avoid repeats
	cmd := []byte(fmt.Sprintf(`{"execute": "qom-list", "arguments": {"path": "/objects/memslot%d"}}`, plugged+1))
	_, err = mon.Run(cmd)
	if err != nil {
		// that mean such object wasn't found, let's add it
		cmd = []byte(fmt.Sprintf(`{"execute": "object-add", "arguments": {"id": "memslot%d", "size": %d, "qom-type": "memory-backend-ram"}}`, plugged+1, size))
		_, err = mon.Run(cmd)
		if err != nil {
			return err
		}
	}
	// now add pc-dimm device
	cmd = []byte(fmt.Sprintf(`{"execute": "device_add", "arguments": {"id": "dimm%d", "driver": "pc-dimm", "memdev": "memslot%d"}}`, plugged+1, plugged+1))
	_, err = mon.Run(cmd)
	if err != nil {
		// device_add command failed... so try remove object that we just created
		cmd = []byte(fmt.Sprintf(`{"execute": "object-del", "arguments": {"id": "memslot%d"}}`, plugged+1))
		mon.Run(cmd)
		return err
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
	defer mon.Disconnect()

	// run from last to first
	var i int
	var merr error
	for i = plugged - 1; i >= 0; i-- {
		// remove pc-dimm device
		cmd := []byte(fmt.Sprintf(`{"execute": "device_del", "arguments": {"id": "%s"}}`, memoryDevices[i].Data.Id))
		_, err = mon.Run(cmd)
		if err != nil {
			merr = errors.Join(merr, err)
			continue
		}
		// wait a bit to allow guest kernel remove memory block
		time.Sleep(time.Second)

		// remove corresponding memdev object
		cmd = []byte(fmt.Sprintf(`{"execute": "object-del", "arguments": {"id": "%s"}}`, strings.ReplaceAll(memoryDevices[i].Data.Memdev, "/objects/", "")))
		_, err = mon.Run(cmd)
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
	defer mon.Disconnect()

	qmpcmd := []byte(`{"execute": "query-memory-size-summary"}`)
	raw, err := mon.Run(qmpcmd)
	if err != nil {
		return nil, err
	}

	var result QmpMemorySize
	json.Unmarshal(raw, &result)

	return resource.NewQuantity(result.Return.BaseMemory+result.Return.PluggedMemory, resource.BinarySI), nil
}

func QmpResizeVirtioMem(virtualmachine *vmv1.VirtualMachine) error {
	// calculate real requested size
	virtioMemSize := virtualmachine.Spec.Guest.Memory.Use.Value() - virtualmachine.Spec.Guest.Memory.Min.Value()

	mon, err := QmpConnect(QmpAddr(virtualmachine))
	if err != nil {
		return err
	}
	defer mon.Disconnect()

	// resize virtio-mem
	cmd := []byte(fmt.Sprintf(`{"execute": "qom-set", "arguments": {"path": "vm0", "property": "requested-size", "value": %d}}`, virtioMemSize))
	_, err = mon.Run(cmd)
	if err != nil {
		return err
	}

	return nil
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
	defer smon.Disconnect()

	// connect to target runner QMP
	t_ip := virtualmachinemigration.Status.TargetPodIP
	tmon, err := qmp.NewSocketMonitor("tcp", fmt.Sprintf("%s:%d", t_ip, port), 2*time.Second)
	if err != nil {
		return err
	}
	if err := tmon.Connect(); err != nil {
		return err
	}
	defer tmon.Disconnect()

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

func QmpGetMigrationInfo(ip string, port int32) (MigrationInfo, error) {
	empty := MigrationInfo{}
	mon, err := QmpConnect(ip, port)
	if err != nil {
		return empty, err
	}
	defer mon.Disconnect()

	qmpcmd := []byte(`{"execute": "query-migrate"}`)
	raw, err := mon.Run(qmpcmd)
	if err != nil {
		return empty, err
	}

	var result QmpMigrationInfo
	json.Unmarshal(raw, &result)

	return result.Return, nil
}

func QmpCancelMigration(ip string, port int32) error {
	mon, err := QmpConnect(ip, port)
	if err != nil {
		return err
	}
	defer mon.Disconnect()

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
	defer mon.Disconnect()

	qmpcmd := []byte(`{"execute": "quit"}`)
	_, err = mon.Run(qmpcmd)
	if err != nil {
		return err
	}

	return nil
}
