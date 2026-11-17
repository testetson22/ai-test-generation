package testkit

import (
	"testing"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/provider"
)

func TestObservationQueueControlsOrderDuplicatesAndDrops(t *testing.T) {
	queue := &ObservationQueue{}
	queue.Enqueue(provider.Observation{Sequence: 1, Status: provider.StatusRunning})
	queue.Enqueue(provider.Observation{Sequence: 2, Status: provider.StatusCompleted})

	duplicate, err := queue.Duplicate(0)
	if err != nil || duplicate.Sequence != 1 {
		t.Fatalf("duplicate = %#v, %v", duplicate, err)
	}
	second, err := queue.Deliver(1)
	if err != nil || second.Sequence != 2 {
		t.Fatalf("out-of-order delivery = %#v, %v", second, err)
	}
	first, err := queue.Deliver(0)
	if err != nil || first.Sequence != 1 {
		t.Fatalf("delayed delivery = %#v, %v", first, err)
	}
	if queue.Len() != 0 {
		t.Fatalf("queue length = %d, want 0", queue.Len())
	}
	if _, err := queue.Deliver(0); err != ErrObservationIndex {
		t.Fatalf("empty delivery error = %v, want index error", err)
	}

	queue.Enqueue(provider.Observation{
		Sequence:   3,
		Status:     provider.StatusFailed,
		ObservedAt: time.Date(2026, time.November, 17, 19, 0, 0, 0, time.UTC),
	})
	if err := queue.Drop(0); err != nil || queue.Len() != 0 {
		t.Fatalf("drop result = %v, queue length = %d", err, queue.Len())
	}
}
