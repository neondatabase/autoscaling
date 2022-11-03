package controllers

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/digitalocean/go-qemu/qmp"
	"k8s.io/apimachinery/pkg/api/resource"

	vmv1 "github.com/neondatabase/neonvm/api/v1"
)

type CpusQueryResult struct {
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

type MemorySizeQueryResult struct {
	Return struct {
		BaseMemory    int64 `json:"base-memory"`
		PluggedMemory int64 `json:"plugged-memory"`
	} `json:"return"`
}

func getCpusPlugged(virtualmachine *vmv1.VirtualMachine) ([]int32, []int32, error) {
	ip := virtualmachine.Status.PodIP
	port := virtualmachine.Spec.QMP

	mon, err := qmp.NewSocketMonitor("tcp", fmt.Sprintf("%s:%d", ip, port), 2*time.Second)
	if err != nil {
		return nil, nil, err
	}
	if err := mon.Connect(); err != nil {
		return nil, nil, err
	}
	defer mon.Disconnect()

	qmpcmd := []byte(`{ "execute": "query-hotpluggable-cpus"}`)
	raw, err := mon.Run(qmpcmd)
	if err != nil {
		return nil, nil, err
	}

	var result CpusQueryResult
	json.Unmarshal(raw, &result)

	plugged := []int32{}
	empty := []int32{}
	for _, entry := range result.Return {
		if entry.QomPath != nil {
			plugged = append(plugged, entry.Props.CoreId)
		} else {
			empty = append(empty, entry.Props.CoreId)
		}
	}
	return plugged, empty, nil
}

func getMemorySize(virtualmachine *vmv1.VirtualMachine) (*resource.Quantity, error) {
	ip := virtualmachine.Status.PodIP
	port := virtualmachine.Spec.QMP

	mon, err := qmp.NewSocketMonitor("tcp", fmt.Sprintf("%s:%d", ip, port), 2*time.Second)
	if err != nil {
		return nil, err
	}
	if err := mon.Connect(); err != nil {
		return nil, err
	}
	defer mon.Disconnect()

	qmpcmd := []byte(`{ "execute": "query-memory-size-summary"}`)
	raw, err := mon.Run(qmpcmd)
	if err != nil {
		return nil, err
	}

	var result MemorySizeQueryResult
	json.Unmarshal(raw, &result)

	return resource.NewQuantity(result.Return.BaseMemory+result.Return.PluggedMemory, resource.BinarySI), nil
}
