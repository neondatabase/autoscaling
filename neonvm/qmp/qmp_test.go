package qmp

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type qmpEvent struct {
	arg    string
	result string
}

type qmpMock struct {
	Events chan qmpEvent
}

func newQMPMock() *qmpMock {
	return &qmpMock{
		Events: make(chan qmpEvent, 100),
	}
}

func (r *qmpMock) expect(arg, result string) {
	r.Events <- qmpEvent{
		arg:    arg,
		result: result,
	}
}

func (r *qmpMock) Run(arg []byte) ([]byte, error) {
	expected := <-r.Events
	Expect(arg).Should(MatchJSON(expected.arg))
	return []byte(expected.result), nil
}

func (r *qmpMock) done() {
	Expect(r.Events).To(BeEmpty())
}

var _ = Describe("VM QMP interaction", func() {
	Context("QMP test", func() {
		It("should support basic QMP operations", func() {
			By("adding memslot")
			mon := newQMPMock()
			defer mon.done()
			mon.expect(`
				{"execute": "object-add",
				 "arguments": {"id": "memslot1",
						"size": 100,
						"qom-type": "memory-backend-ram"}}`, `{}`)

			err := addMemoryBackend(mon, 1, 100)
			Expect(err).To(Not(HaveOccurred()))
		})
	})
})
