package controllers

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitalocean/go-qemu/qmp"
	"k8s.io/apimachinery/pkg/api/resource"

	vmv1 "github.com/neondatabase/neonvm/apis/neonvm/v1"
)

type QmpCpus struct {
	Return []struct {
		Props struct {
			CoreId   int32 `json:"core-id"`
			ThreadId int32 `json:"thread-id"`
			SockerId int32 `json:"socket-id"`
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

func QmpGetCpus(virtualmachine *vmv1.VirtualMachine) ([]QmpCpuSlot, []QmpCpuSlot, error) {
	mon, err := QmpConnect(virtualmachine)
	if err != nil {
		return nil, nil, err
	}
	defer mon.Disconnect()

	qmpcmd := []byte(`{ "execute": "query-hotpluggable-cpus"}`)
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
		return errors.New("No empy slots for CPU hotplug")
	}

	mon, err := QmpConnect(virtualmachine)
	if err != nil {
		return err
	}
	defer mon.Disconnect()

	// empty list reversed, first cpu slot in the end of list and last cpu slot in the begining
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
		return errors.New("There are no unpluggable CPUs")
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
	cmd := []byte(`{ "execute": "query-memory-devices"}`)
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
		return errors.New("No empy slots for Memory hotplug")
	}

	mon, err := QmpConnect(virtualmachine)
	if err != nil {
		return err
	}
	defer mon.Disconnect()

	// add memdev object for next slot
	// firstly check if such obect already present to avoid repeats
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
		// device_add comand failed... so try remove object that we just created
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
		return errors.New("There are no unpluggable Memory slots")
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

	qmpcmd := []byte(`{ "execute": "query-memory-size-summary"}`)
	raw, err := mon.Run(qmpcmd)
	if err != nil {
		return nil, err
	}

	var result QmpMemorySize
	json.Unmarshal(raw, &result)

	return resource.NewQuantity(result.Return.BaseMemory+result.Return.PluggedMemory, resource.BinarySI), nil
}
