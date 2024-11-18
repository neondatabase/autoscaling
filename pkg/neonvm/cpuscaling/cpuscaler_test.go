package cpuscaling

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

// MockState is a mock implementation of the cpuState to avoid IO during testing
type MockState struct {
	state []cpuState
}

func NewMockState(size int) *MockState {
	m := &MockState{
		state: make([]cpuState, size),
	}

	for i := 0; i < size; i++ {
		m.state[i] = cpuOffline
	}
	m.state[0] = cpuOnline
	return m
}

func (m *MockState) GetState(cpuNum int) (cpuState, error) {
	if cpuNum < 0 || cpuNum >= len(m.state) {
		return cpuOffline, os.ErrNotExist
	}
	return m.state[cpuNum], nil
}

func (m *MockState) SetState(cpuNum int, state cpuState) error {
	if cpuNum < 0 || cpuNum >= len(m.state) {
		return os.ErrNotExist
	}
	m.state[cpuNum] = state
	return nil
}

func (cs *MockState) PossibleCPUs() (int, int, error) {
	return 0, len(cs.state) - 1, nil
}

func (cs *MockState) ActiveCPUsCount() (int, error) {
	count := 0
	for _, state := range cs.state {
		if state == cpuOnline {
			count++
		}
	}
	return count, nil
}

func TestReconcileCPU(t *testing.T) {
	t.Run("multiple cpu available", func(t *testing.T) {
		scaler := &CPUScaler{
			cpuState: NewMockState(3),
		}

		// Initially all offline except first
		assertActiveCPUsCount(t, scaler, 1)

		// Scale up
		assert.NoError(t, scaler.ReconcileOnlineCPU(3))
		assertActiveCPUsCount(t, scaler, 3)

		// Scale down
		assert.NoError(t, scaler.ReconcileOnlineCPU(2))
		assertActiveCPUsCount(t, scaler, 2)
	})

	t.Run("scale to the current value does nothing", func(t *testing.T) {
		scaler := &CPUScaler{
			cpuState: NewMockState(3),
		}

		// Initially all offline except first
		assertActiveCPUsCount(t, scaler, 1)

		// first scale up to 2 active CPU
		err := scaler.ReconcileOnlineCPU(2)
		assert.NoError(t, err)
		assertActiveCPUsCount(t, scaler, 2)

		// second reconciliation to the same value doesn't change anything
		err = scaler.ReconcileOnlineCPU(2)
		assert.NoError(t, err)
		assertActiveCPUsCount(t, scaler, 2)
	})

	t.Run("gradually test scaling up and down", func(t *testing.T) {
		scaler := &CPUScaler{
			cpuState: NewMockState(16),
		}

		// initially only 1 cpu is online
		assertActiveCPUsCount(t, scaler, 1)

		// scaling up gradually
		for i := 2; i <= 16; i++ {
			assert.NoError(t, scaler.ReconcileOnlineCPU(i))
			assertActiveCPUsCount(t, scaler, i)
		}

		// scaling down gradually
		for i := 15; i >= 1; i-- {
			assert.NoError(t, scaler.ReconcileOnlineCPU(i))
			assertActiveCPUsCount(t, scaler, i)
		}

		// at the end we should be in the initial state of 1 cpu
		assertActiveCPUsCount(t, scaler, 1)
	})
}

func assertActiveCPUsCount(t *testing.T, scaler *CPUScaler, n int) {
	activeCPUs, err := scaler.cpuState.ActiveCPUsCount()
	assert.NoError(t, err)
	assert.Equal(t, n, activeCPUs)
}
