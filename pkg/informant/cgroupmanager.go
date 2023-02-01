package informant

// A lightweight wrapper around cgroup2.Manager, with a mix of convenience and extra functionality.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	cgroups "github.com/containerd/cgroups/v3"
	"github.com/containerd/cgroups/v3/cgroup2"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type CgroupManager struct {
	MemoryHighEvent util.CondChannelReceiver
	ErrCh           <-chan error

	name    string
	manager *cgroup2.Manager
}

func NewCgroupManager(groupName string) (*CgroupManager, error) {
	mode := cgroups.Mode()
	if mode != cgroups.Unified && mode != cgroups.Hybrid {
		var modeString string
		switch mode {
		case cgroups.Unavailable:
			modeString = "Unavailable"
		case cgroups.Legacy:
			modeString = "cgroups v1 ONLY"
		default:
			panic(fmt.Errorf("unexpected cgroups mode value %d", mode))
		}

		return nil, fmt.Errorf("cgroups v2 are not enabled, mode = %q", modeString)
	}

	// note: cgroup2.Load expects the cgroup "path" to start with '/', rooted at "/sys/fs/cgroup"
	//
	// The final path of the cgroup will be "/sys/fs/cgroup" + <name>, where <name> is what we give
	// cgroup2.Load().
	manager, err := cgroup2.Load(fmt.Sprint("/", groupName))
	if err != nil {
		return nil, fmt.Errorf("Error loading cgroup: %w", err)
	}

	// FIXME: There's currently no way to stop the goroutine spawned by EventChan, so it doesn't yet
	// make sense to provide a way to cancel the goroutine to handle its events. Eventually, we
	// should either patch containerd/cgroups or write our own implementation here.
	memEvents, eventErrCh := manager.EventChan()
	sendEvent, recvEvent := util.NewCondChannelPair()

	highEventCount := &atomic.Uint64{}
	errCh := make(chan error, 1)

	// Long-running handler task for memory events
	go func() {
		for {
			select {
			case event := <-memEvents:
				highCount := event.High
				oldHighCount := util.AtomicMax(highEventCount, highCount)
				if highCount > oldHighCount {
					sendEvent.Send()
				}
			case err, ok := <-eventErrCh:
				if err == nil && !ok {
					errCh <- errors.New("Memory event channel closed without error")
				} else {
					errCh <- fmt.Errorf("Error while waiting for memory events: %w", err)
				}
				return
			}
		}
	}()

	// Fetch the current "memory high" count
	current, err := parseMemoryEvents(groupName)
	if err != nil {
		return nil, fmt.Errorf("Error getting current memory events: %w", err)
	}

	util.AtomicMax(highEventCount, current.High)
	recvEvent.Consume() // Clear events

	return &CgroupManager{
		MemoryHighEvent: recvEvent,
		ErrCh:           errCh,
		name:            groupName,
		manager:         manager,
	}, nil
}

// TODO: no way to do this with github.com/containerd/cgroups ? Seems like that should be
// exposed to the user... We *can* just parse it directly, but it's a bit annoying.
func parseMemoryEvents(groupName string) (*cgroup2.Event, error) {
	path := cgroupPath(groupName, "memory.events")
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("Error reading file at %q: %w", path, err)
	}

	event := cgroup2.Event{} //nolint:exhaustruct // fields set by the rest of this function
	valueMap := map[string]struct {
		v   *uint64
		set bool
	}{
		"low":      {&event.Low, false},
		"high":     {&event.High, false},
		"max":      {&event.Max, false},
		"oom":      {&event.OOM, false},
		"oom_kill": {&event.OOMKill, false},
	}

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf(
				"Line %d of %q is not expected format: has %d fields", i, path, len(fields),
			)
		}

		name := fields[0]
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf(
				"Error parsing field on line %d of %q as integer: %w", i, path, err,
			)
		}

		pair, ok := valueMap[name]
		if !ok {
			return nil, fmt.Errorf("Unexpected field name %q", name)
		} else if pair.set {
			return nil, fmt.Errorf("Duplicate field %q", name)
		}
		*pair.v = value
		pair.set = true
		valueMap[name] = pair
	}

	var unset []string

	// Check if there's any unset fields
	for name, pair := range valueMap {
		if !pair.set {
			unset = append(unset, name)
		}
	}

	if len(unset) != 0 {
		return nil, fmt.Errorf("Some fields not provided: %+v", unset)
	}

	return &event, nil
}

// TODO: Open a PR in github.com/containerd/cgroups to expose this publicly. This function is
// *basically* just copied from there.
func fetchState(groupName string) (cgroup2.State, error) {
	path := cgroupPath(groupName, "cgroup.freeze")
	content, err := os.ReadFile(path)
	if err != nil {
		return cgroup2.Unknown, fmt.Errorf("Error reading file at %q: %w", path, err)
	}
	switch strings.TrimSpace(string(content)) {
	case "1":
		return cgroup2.Frozen, nil
	case "0":
		return cgroup2.Thawed, nil
	default:
		return cgroup2.Unknown, errors.New("Unexpected file content")
	}
}

// TODO: not great that we're implementing this function ourselves. It's required for fetchState and
// parseMemoryEvents, which we'd also like to get rid of.
func cgroupPath(groupName string, file string) string {
	// note: it's ok to use slashes, because this can only run on linux anyways.
	return filepath.Join("/sys/fs/cgroup", groupName, file) //nolint:gocritic // see comment above.
}

// SetMemLimit sets the memory.high limit of the cgroup, clearing
func (c *CgroupManager) SetHighMem(bytes uint64) error {
	high := int64(bytes)
	return c.manager.Update(&cgroup2.Resources{
		Memory: &cgroup2.Memory{
			High: &high,
		},
	})
}

// FetchState returns a cgroup2.State indicating whether the cgroup is currently frozen
func (c *CgroupManager) FetchState() (cgroup2.State, error) {
	return fetchState(c.name)
}

// CurrentMemoryUsage returns the value at memory.current -- the cgroup's current memory usage.
func (c *CgroupManager) CurrentMemoryUsage() (uint64, error) {
	path := cgroupPath(c.name, "memory.current")
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("Error reading file at %q: %w", path, err)
	}

	amount, err := strconv.ParseUint(strings.TrimSpace(string(content)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("Error parsing as uint64: %w", err)
	}
	return amount, nil
}

func (c *CgroupManager) Freeze() error {
	return c.manager.Freeze()
}

func (c *CgroupManager) Thaw() error {
	return c.manager.Thaw()
}
