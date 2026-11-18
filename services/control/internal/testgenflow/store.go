package testgenflow

import "context"

type Repository interface {
	CreateRun(context.Context, Run) (Run, bool, error)
	GetRun(context.Context, string) (Run, error)
	ApplyEvent(context.Context, string, Event) (Run, error)
}
