package execution

import "time"

type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time {
	return time.Now()
}

func SystemClock() Clock {
	return systemClock{}
}
