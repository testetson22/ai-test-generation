package testkit

import (
	"errors"
	"sync"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/provider"
)

var ErrObservationIndex = errors.New("observation index out of range")

// ObservationQueue allows tests to delay, drop, duplicate, and reorder provider
// deliveries without changing the provider's own durable state.
type ObservationQueue struct {
	mu     sync.Mutex
	queued []provider.Observation
}

func (q *ObservationQueue) Enqueue(observation provider.Observation) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queued = append(q.queued, observation)
}

func (q *ObservationQueue) Deliver(index int) (provider.Observation, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if index < 0 || index >= len(q.queued) {
		return provider.Observation{}, ErrObservationIndex
	}
	observation := q.queued[index]
	q.queued = append(q.queued[:index], q.queued[index+1:]...)
	return observation, nil
}

func (q *ObservationQueue) Duplicate(index int) (provider.Observation, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if index < 0 || index >= len(q.queued) {
		return provider.Observation{}, ErrObservationIndex
	}
	return q.queued[index], nil
}

func (q *ObservationQueue) Drop(index int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if index < 0 || index >= len(q.queued) {
		return ErrObservationIndex
	}
	q.queued = append(q.queued[:index], q.queued[index+1:]...)
	return nil
}

func (q *ObservationQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.queued)
}
