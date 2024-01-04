package controllers

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type qmpEvent struct {
	Arg    string
	Result string
}

type QMPMock struct {
	Events chan qmpEvent
}

func NewQMPMock() *QMPMock {
	return &QMPMock{
		Events: make(chan qmpEvent, 100),
	}
}

func (r *QMPMock) Expect(arg, result string) {
	r.Events <- qmpEvent{
		Arg:    arg,
		Result: result,
	}
}

func (r *QMPMock) Run(arg []byte) ([]byte, error) {
	expected := <-r.Events
	Expect(arg).Should(MatchJSON(expected.Arg))
	return []byte(expected.Result), nil
}

func (r *QMPMock) Done() {
	Expect(r.Events).To(BeEmpty())
}

var _ = Describe("VM QMP interaction", func() {
	Context("QMP test", func() {
		It("should support basic QMP operations", func() {
			By("adding memslot")
			qmp := NewQMPMock()
			defer qmp.Done()
			qmp.Expect(`
				{"execute": "object-add",
				 "arguments": {"id": "memslot1",
						"size": 100,
						"qom-type": "memory-backend-ram"}}`, `{}`)
			err := QmpAddMemoryBackend(qmp, 1, 100)
			Expect(err).To(Not(HaveOccurred()))
		})
	})
})
