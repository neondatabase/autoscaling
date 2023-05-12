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

func QmpConnect(virtualmachine *vmv1.VirtualMachine) (*qmp.SocketMonitor, error) {
	ip := virtualmachine.Status.PodIP
	port := virtualmachine.Spec.QMP

	mon, err := qmp.NewSocketMonitor("tcp", fmt.Sprintf("%s:%d", ip, port), 2*time.Second)
	if err != nil {
		return nil, err
	}
	if err := mon.Connect(); err != nil {
		return nil, err
	}

	return mon, nil
}

func QmpConnectByIP(ip string, port int32) (*qmp.SocketMonitor, error) {
	mon, err := qmp.NewSocketMonitor("tcp", fmt.Sprintf("%s:%d", ip, port), 2*time.Second)
	if err != nil {
		return nil, err
	}
	if err := mon.Connect(); err != nil {
		return nil, err
	}

	return mon, nil
}

func QmpGetCpus(virtualmachine *vmv1.VirtualMachine) ([]QmpCpuSlot, []QmpCpuSlot, error) {
	mon, err := QmpConnect(virtualmachine)
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

func QmpGetCpusFromRunner(ip string, port int32) ([]QmpCpuSlot, []QmpCpuSlot, error) {
	mon, err := QmpConnectByIP(ip, port)
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

func QmpPlugCpu(virtualmachine *vmv1.VirtualMachine) error {
	_, empty, err := QmpGetCpus(virtualmachine)
	if err != nil {
		return err
	}
	if len(empty) == 0 {
		return errors.New("no empty slots for CPU hotplug")
	}

	mon, err := QmpConnect(virtualmachine)
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

func QmpPlugCpuToRunner(ip string, port int32) error {
	_, empty, err := QmpGetCpusFromRunner(ip, port)
	if err != nil {
		return err
	}
	if len(empty) == 0 {
		return errors.New("no empty slots for CPU hotplug")
	}

	mon, err := QmpConnectByIP(ip, port)
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

func QmpUnplugCpu(virtualmachine *vmv1.VirtualMachine) error {
	plugged, _, err := QmpGetCpus(virtualmachine)
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

	mon, err := QmpConnect(virtualmachine)
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

func QmpQueryMemoryDevices(virtualmachine *vmv1.VirtualMachine) ([]QmpMemoryDevice, error) {
	mon, err := QmpConnect(virtualmachine)
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

func QmpQueryMemoryDevicesFromRunner(ip string, port int32) ([]QmpMemoryDevice, error) {
	mon, err := QmpConnectByIP(ip, port)
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

	memoryDevices, err := QmpQueryMemoryDevices(virtualmachine)
	if err != nil {
		return err
	}

	// check if available mmory slots present
	plugged := int32(len(memoryDevices))
	if plugged >= slots {
		return errors.New("no empty slots for Memory hotplug")
	}

	mon, err := QmpConnect(virtualmachine)
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
		cmd = []byte(fmt.Sprintf(`{"execute": "object-add", "arguments": {"id": "memslot%d", "size": %d, "qom-type": "memory-backend-ram"}}`, plugged+1, virtualmachine.Spec.Guest.MemorySlotSize.Value()))
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

func QmpPlugMemoryToRunner(ip string, port int32, size int64) error {
	memoryDevices, err := QmpQueryMemoryDevicesFromRunner(ip, port)
	if err != nil {
		return err
	}
	plugged := int32(len(memoryDevices))

	mon, err := QmpConnectByIP(ip, port)
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

func QmpUnplugMemory(virtualmachine *vmv1.VirtualMachine) error {
	memoryDevices, err := QmpQueryMemoryDevices(virtualmachine)
	if err != nil {
		return err
	}
	plugged := len(memoryDevices)
	if plugged == 0 {
		return errors.New("there are no unpluggable Memory slots")
	}

	mon, err := QmpConnect(virtualmachine)
	if err != nil {
		return err
	}
	defer mon.Disconnect()

	// remove latest pc-dimm device
	cmd := []byte(fmt.Sprintf(`{"execute": "device_del", "arguments": {"id": "%s"}}`, memoryDevices[plugged-1].Data.Id))
	_, err = mon.Run(cmd)
	if err != nil {
		return err
	}
	// wait a bit to allow guest kernel remove memory block
	time.Sleep(time.Second)

	// remove corresponding (latest) memdev object
	cmd = []byte(fmt.Sprintf(`{"execute": "object-del", "arguments": {"id": "%s"}}`, strings.ReplaceAll(memoryDevices[plugged-1].Data.Memdev, "/objects/", "")))
	_, err = mon.Run(cmd)
	if err != nil {
		return err
	}

	return nil
}

func QmpGetMemorySize(virtualmachine *vmv1.VirtualMachine) (*resource.Quantity, error) {
	mon, err := QmpConnect(virtualmachine)
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

func QmpGetMigrationInfo(virtualmachine *vmv1.VirtualMachine) (MigrationInfo, error) {
	empty := MigrationInfo{}
	mon, err := QmpConnect(virtualmachine)
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

func QmpQuit(ip string, port int32) error {
	mon, err := QmpConnectByIP(ip, port)
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
