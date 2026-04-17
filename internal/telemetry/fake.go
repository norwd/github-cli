package telemetry

import "github.com/cli/cli/v2/internal/gh/ghtelemetry"

type EventRecorderSpy struct {
	Events []ghtelemetry.Event
}

func (r *EventRecorderSpy) Record(event ghtelemetry.Event) {
	r.Events = append(r.Events, event)
}

func (r *EventRecorderSpy) Disable() {}

func (r *EventRecorderSpy) Flush() {}
