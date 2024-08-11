package qmp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/digitalocean/go-qemu/qmp"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/tools/record"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

type CPUs struct {
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

type MemorySize struct {
	Return struct {
		BaseMemory    int64 `json:"base-memory"`
		PluggedMemory int64 `json:"plugged-memory"`
	} `json:"return"`
}

type CPUSlot struct {
	Core int32  `json:"core"`
	QOM  string `json:"qom"`
	Type string `json:"type"`
}

type MemoryDevices struct {
	Return []MemoryDevice `json:"return"`
}

type MemoryDevice struct {
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

type Factory struct{}

var DefaultQMPFactory = &Factory{}

func (f *Factory) ConnectIP(ip string, port int32) (*Monitor, error) {
	mon, err := qmp.NewSocketMonitor("tcp", fmt.Sprintf("%s:%d", ip, port), 2*time.Second)
	if err != nil {
		return nil, err
	}
	if err := mon.Connect(); err != nil {
		return nil, err
	}

	return &Monitor{mon: mon}, nil
}

func (f *Factory) ConnectVM(vm *vmv1.VirtualMachine) (*Monitor, error) {
	return f.ConnectIP(vm.Status.PodIP, vm.Spec.QMP)
}

type Monitor struct {
	mon *qmp.SocketMonitor
}

func (m *Monitor) Close() {
	if m.mon != nil {
		_ = m.mon.Disconnect()
	}
}

func (m *Monitor) CPUs() ([]CPUSlot, []CPUSlot, error) {
	qmpcmd := []byte(`{"execute": "query-hotpluggable-cpus"}`)
	raw, err := m.mon.Run(qmpcmd)
	if err != nil {
		return nil, nil, err
	}

	var result CPUs
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, nil, fmt.Errorf("error unmarshaling json: %w", err)
	}

	plugged := []CPUSlot{}
	empty := []CPUSlot{}
	for _, entry := range result.Return {
		if entry.QomPath != nil {
			plugged = append(plugged, CPUSlot{Core: entry.Props.CoreId, QOM: *entry.QomPath, Type: entry.Type})
		} else {
			empty = append(empty, CPUSlot{Core: entry.Props.CoreId, QOM: "", Type: entry.Type})
		}
	}

	return plugged, empty, nil
}

func (m *Monitor) PlugCPU() error {
	_, empty, err := m.CPUs()
	if err != nil {
		return err
	}

	if len(empty) == 0 {
		return errors.New("no empty slots for CPU hotplug")
	}

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

	_, err = m.mon.Run(qmpcmd)
	if err != nil {
		return err
	}

	return nil
}

func (m *Monitor) UnplugCPU() error {
	plugged, _, err := m.CPUs()
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

	cmd := []byte(fmt.Sprintf(`{"execute": "device_del", "arguments": {"id": %q}}`, plugged[slot].QOM))
	_, err = m.mon.Run(cmd)
	if err != nil {
		return err
	}
	// small pause to let hypervisor do unplug
	time.Sleep(500 * time.Millisecond)

	return nil
}

func SyncCpuToTarget(vm *vmv1.VirtualMachine, migration *vmv1.VirtualMachineMigration) error {
	sourceMon, err := DefaultQMPFactory.ConnectVM(vm)
	if err != nil {
		return err
	}
	defer sourceMon.Close()

	plugged, _, err := sourceMon.CPUs()
	if err != nil {
		return err
	}

	targetMon, err := DefaultQMPFactory.ConnectIP(migration.Status.TargetPodIP, vm.Spec.QMP)
	if err != nil {
		return err
	}
	pluggedInTarget, _, err := targetMon.CPUs()
	if err != nil {
		return err
	}
	if len(plugged) == len(pluggedInTarget) {
		// no need plug anything
		return nil
	}

	defer targetMon.Close()

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
		_, err = targetMon.mon.Run(qmpcmd)
		if err != nil {
			return err
		}
	}

	return nil
}

func (m *Monitor) MemoryDevices() ([]MemoryDevice, error) {
	cmd := []byte(`{"execute": "query-memory-devices"}`)
	raw, err := m.mon.Run(cmd)
	if err != nil {
		return nil, err
	}

	var result MemoryDevices
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("error unmarshaling json: %w", err)
	}
	return result.Return, nil
}

// memslotIdxFromName takes "/objects/memslot3" or "memslot3 and returns 3
func memslotIdxFromName(name string) (int, error) {
	name = strings.TrimPrefix(name, "/objects/")
	idxStr := strings.TrimPrefix(name, "memslot")
	idx, err := strconv.Atoi(idxStr)
	if err != nil {
		// doesn't reference `err`, because we don't know the actual issue
		return 0, fmt.Errorf("failed to parse memory device id: %q", name)
	}
	return idx, nil
}

func (m *Monitor) MemoryBackendIds() (map[int]struct{}, error) {
	cmd := []byte(`{"execute": "qom-list", "arguments": {"path": "/objects"}}`)
	raw, err := m.mon.Run(cmd)
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

		idx, err := memslotIdxFromName(o.Name)
		if err != nil {
			return nil, err
		}
		backends[idx] = struct{}{}
	}
	return backends, nil
}

type QMPRunner interface {
	Run([]byte) ([]byte, error)
}

// QmpSetVirtioMem updates virtio-mem to the new target size, returning the previous target.
//
// If the new target size is equal to the previous one, this function does nothing but query the
// target.
func QmpSetVirtioMem(vm *vmv1.VirtualMachine, targetVirtioMemSize int64) (previous int64, _ error) {
	// Note: The virtio-mem device only exists when max mem != min mem.
	// So if min == max, we should just short-cut, skip the queries, and say it's all good.
	// Refer to the instantiation in neonvm-runner for more.
	if vm.Spec.Guest.MemorySlots.Min == vm.Spec.Guest.MemorySlots.Max {
		// if target size is non-zero even though min == max, something went very wrong
		if targetVirtioMemSize != 0 {
			panic(fmt.Sprintf(
				"VM min mem slots == max mem slots, but target virtio-mem size %d != 0",
				targetVirtioMemSize,
			))
		}
		// Otherwise, we're all good, just pretend like we talked to the VM.
		return 0, nil
	}

	mon, err := DefaultQMPFactory.ConnectVM(vm)
	if err != nil {
		return 0, err
	}
	defer mon.Close()

	// First, fetch current desired virtio-mem size. If it's the same as targetVirtioMemSize, then
	// we can report that it was already the same.
	cmd := []byte(`{"execute": "qom-get", "arguments": {"path": "vm0", "property": "requested-size"}}`)
	raw, err := mon.mon.Run(cmd)
	if err != nil {
		return 0, err
	}
	result := struct {
		Return int64 `json:"return"`
	}{Return: 0}
	if err := json.Unmarshal(raw, &result); err != nil {
		return 0, fmt.Errorf("error unmarshaling json: %w", err)
	}

	previous = result.Return

	if previous == targetVirtioMemSize {
		return previous, nil
	}

	// The current requested size is not equal to the new desired size. Let's change that.
	cmd = []byte(fmt.Sprintf(
		`{"execute": "qom-set", "arguments": {"path": "vm0", "property": "requested-size", "value": %d}}`,
		targetVirtioMemSize,
	))
	_, err = mon.mon.Run(cmd)
	if err != nil {
		return 0, err
	}

	return previous, nil
}

// QmpAddMemoryBackend adds a single memory slot to the VM with the given size.
//
// The memory slot does nothing until a corresponding "device" is added to the VM for the same memory slot.
// See QmpAddMemoryDevice for more.
// When unplugging, QmpDelMemoryDevice must be called before QmpDelMemoryBackend.
func QmpAddMemoryBackend(mon QMPRunner, idx int, sizeBytes int64) error {
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

type QmpMemorySetter struct {
	vm        *vmv1.VirtualMachine
	targetCnt int
	recorder  record.EventRecorder
	log       logr.Logger

	mon         *Monitor
	memBackends map[int]bool // idx -> is active
	maxBackend  int          // stores the max idx that was discovered to added.
	// Is needed to know where to start deletion

	memDevCount int
	errs        []error
}

func (r *QmpMemorySetter) buildState() error {
	memDevs, err := r.mon.MemoryDevices()
	if err != nil {
		return err
	}
	r.memDevCount = len(memDevs)

	for _, m := range memDevs {
		idx, err := memslotIdxFromName(m.Data.Memdev)
		if err == nil {
			r.memBackends[idx] = true
		}
	}

	backends, err := r.mon.MemoryBackendIds()
	if err != nil {
		return err
	}

	for b := range backends {
		if _, ok := r.memBackends[b]; !ok {
			r.memBackends[b] = false
		}
	}
	for idx := range r.memBackends {
		r.maxBackend = max(r.maxBackend, idx)
	}
	r.log.Info("QMP memory state", "backends", r.memBackends, "maxBackend", r.maxBackend)
	return nil
}

func (r *QmpMemorySetter) Disconnect() {
	r.mon.Close()
}

// attemptsCounter limits the total number of operations in each phase.
// In case QMP keeps timeouting, but the operation silently succeeding,
// we don't want to keep doing the QMP actions until we get enough positive
// results.
type attemptsCounter struct {
	target int
	done   int
}

func newAttemptsCounter(target int) *attemptsCounter {
	return &attemptsCounter{
		target: target,
		done:   0,
	}
}

// Registers an attempt and returns true if it allowed to continue
func (t *attemptsCounter) attempt() bool {
	if t.done < t.target {
		t.done++
		return true
	}
	return false
}

func (t *attemptsCounter) didSomething() bool {
	return t.done > 0
}

func (r *QmpMemorySetter) AddBackends() {
	if r.targetCnt <= len(r.memBackends) {
		return
	}

	attempts := newAttemptsCounter(r.targetCnt - len(r.memBackends))
	for idx := 1; idx <= r.targetCnt; idx++ {
		if _, ok := r.memBackends[idx]; ok {
			continue
		}

		if !attempts.attempt() {
			break
		}

		err := QmpAddMemoryBackend(r.mon.mon, idx, r.vm.Spec.Guest.MemorySlotSize.Value())
		if err != nil {
			r.errs = append(r.errs, err)
			r.recorder.Event(r.vm, "Warning", "ScaleUp",
				fmt.Sprintf("Failed to add memslot%d: %s",
					idx, err.Error()))
			continue
		}
		r.recorder.Event(r.vm, "Normal", "ScaleUp",
			fmt.Sprintf("Added memslot%d", idx))
		r.memBackends[idx] = false
		// The one we just added might be the new max
		r.maxBackend = max(r.maxBackend, idx)
	}

	if attempts.didSomething() {
		// might need to wait for QEMU to allocate the memory
		time.Sleep(time.Second)
	}
}

func (r *QmpMemorySetter) AddDevices() {
	if r.targetCnt <= r.memDevCount {
		return
	}

	attempts := newAttemptsCounter(r.targetCnt - r.memDevCount)

	for idx := 1; idx <= r.maxBackend; idx++ {
		active, ok := r.memBackends[idx]
		if !ok || active {
			continue
		}
		// Found unused backend to plug into

		if !attempts.attempt() {
			break
		}

		err := QmpAddMemoryDevice(r.mon.mon, idx)
		if err != nil {
			r.errs = append(r.errs, err)
			r.recorder.Event(r.vm, "Warning", "ScaleUp",
				fmt.Sprintf("Failed to add dimm%d to VM %s: %s",
					idx, r.vm.Name, err.Error()))
			continue
		}
		r.recorder.Event(r.vm, "Normal", "ScaleUp",
			fmt.Sprintf("Added dimm%d", idx))
		r.memBackends[idx] = true
		r.memDevCount++
	}
}

func (r *QmpMemorySetter) RemoveDevices() {
	if r.memDevCount <= r.targetCnt {
		return
	}

	attempts := newAttemptsCounter(r.memDevCount - r.targetCnt)
	// Removing from the end to keep memslot1,memslot2,...
	for idx := r.maxBackend; idx >= 1; idx-- {
		active, ok := r.memBackends[idx]
		if !ok || !active {
			continue
		}
		// Found used backend to remove

		if !attempts.attempt() {
			break
		}

		err := QmpDelMemoryDevice(r.mon.mon, idx)
		if err != nil {
			r.errs = append(r.errs, err)
			r.recorder.Event(r.vm, "Warning", "ScaleDown",
				fmt.Sprintf("Failed to remove dimm%d: %s",
					idx, err.Error()))
			continue
		}
		r.recorder.Event(r.vm, "Normal", "ScaleDown",
			fmt.Sprintf("Removed dimm%d", idx))
		r.memBackends[idx] = false
		r.memDevCount--
	}

	if attempts.didSomething() {
		// wait a bit to allow guest kernel remove memory block
		time.Sleep(time.Second)
	}
}

func (r *QmpMemorySetter) RemoveBackends() {
	if len(r.memBackends) <= r.targetCnt {
		return
	}

	attempts := newAttemptsCounter(len(r.memBackends) - r.targetCnt)
	for idx := r.maxBackend; idx >= 1; idx-- {
		active, ok := r.memBackends[idx]
		if !ok || active {
			continue
		}

		if !attempts.attempt() {
			break
		}

		err := QmpDelMemoryBackend(r.mon.mon, idx)
		if err != nil {
			r.errs = append(r.errs, err)
			r.recorder.Event(r.vm, "Warning", "ScaleDown",
				fmt.Sprintf("Failed to remove memslot%d: %s",
					idx, err.Error()))
			continue
		}
		r.recorder.Event(r.vm, "Normal", "ScaleDown",
			fmt.Sprintf("Removed memslot%d", idx))
		delete(r.memBackends, idx)
	}
}

func (r *QmpMemorySetter) run() (int, error) {
	// Usually, runs first two or last two phases.
	// If there are leftover slots, might run 2 and 4.
	// If there are errors, last two phases serve as cleanup.
	phases := []func(){
		r.AddBackends,
		r.AddDevices,
		r.RemoveDevices,
		r.RemoveBackends,
	}
	for _, phase := range phases {
		phase()
	}

	return r.memDevCount, errors.Join(r.errs...)
}

// QmpSetMemorySlots attempts to plug/unplug memory slots to match targetCnt.
//
// Returns the number of slots, which the function managed to plug.
// Ideally, it matches targetCnt, but can be less or more if there are
// errors.
//
// Returns -1 if failed to father current state of memory, otherwise,
// the return value is valid even if there are errors.
//
// In order for the hotplug to occur, we have to do two things:
// 1. Plug memory backend (memslot<n>) - a QEMU object, which physically
// allocates the memory from host
// 2. Plug DIMM device (dimm<n>) - a device, which exposes the memory to the
// host. dimm<n> is always plugged into memslot<n> with the same n.
//
// In order to do hotunplug, we need to make the same actions in the reversed
// order.
func (f *Factory) QmpSetMemorySlots(
	ctx context.Context,
	vm *vmv1.VirtualMachine,
	targetCnt int,
	recorder record.EventRecorder,
) (int, error) {
	log := log.FromContext(ctx)

	mon, err := f.ConnectVM(vm)
	if err != nil {
		return -1, err
	}

	setter := &QmpMemorySetter{
		vm:        vm,
		targetCnt: targetCnt,
		recorder:  recorder,
		log:       log,

		mon: mon,

		memBackends: map[int]bool{},
		maxBackend:  0,
		memDevCount: 0,
		errs:        []error{},
	}

	defer setter.Disconnect()

	err = setter.buildState()
	if err != nil {
		return -1, err
	}

	return setter.run()
}

func QmpSyncMemoryToTarget(vm *vmv1.VirtualMachine, migration *vmv1.VirtualMachineMigration) error {
	sourceMon, err := DefaultQMPFactory.ConnectVM(vm)
	if err != nil {
		return err
	}
	defer sourceMon.Close()

	memoryDevices, err := sourceMon.MemoryDevices()
	if err != nil {
		return err
	}

	targetMon, err := DefaultQMPFactory.ConnectIP(migration.Status.TargetPodIP, vm.Spec.QMP)
	if err != nil {
		return err
	}
	defer targetMon.Close()

	memoryDevicesInTarget, err := targetMon.MemoryDevices()
	if err != nil {
		return err
	}

	for _, m := range memoryDevices {
		// firsly check if slot occupied already
		// run over Target memory and compare device id
		found := false
		for _, tm := range memoryDevicesInTarget {
			if reflect.DeepEqual(m, tm) {
				found = true
			}
		}
		if found {
			// that mean such memory device 'm' already present in Target, skip it
			continue
		}
		// add memdev object
		memdevIdx, err := memslotIdxFromName(m.Data.Memdev)
		if err != nil {
			return err
		}
		err = QmpAddMemoryBackend(targetMon.mon, memdevIdx, m.Data.Size)
		if err != nil {
			return err
		}
		// now add pc-dimm device
		err = QmpAddMemoryDevice(targetMon.mon, memdevIdx)
		if err != nil {
			// device_add command failed... so try remove object that we just created
			_ = QmpDelMemoryBackend(targetMon.mon, memdevIdx)
			return err
		}
	}

	return nil
}

func (m *Monitor) MemorySize() (*resource.Quantity, error) {
	qmpcmd := []byte(`{"execute": "query-memory-size-summary"}`)
	raw, err := m.mon.Run(qmpcmd)
	if err != nil {
		return nil, err
	}

	var result MemorySize
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

func (m *Monitor) MigrationInfo() (*MigrationInfo, error) {
	qmpcmd := []byte(`{"execute": "query-migrate"}`)
	raw, err := m.mon.Run(qmpcmd)
	if err != nil {
		return nil, err
	}

	var result QmpMigrationInfo
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("error unmarshaling json: %w", err)
	}

	return &result.Return, nil
}

func (m *Monitor) MigrationCancel() error {
	qmpcmd := []byte(`{"execute": "migrate_cancel"}`)
	_, err := m.mon.Run(qmpcmd)
	if err != nil {
		return err
	}

	return nil
}

func (m *Monitor) Quit() error {
	qmpcmd := []byte(`{"execute": "quit"}`)
	_, err := m.mon.Run(qmpcmd)
	if err != nil {
		return err
	}

	return nil
}
