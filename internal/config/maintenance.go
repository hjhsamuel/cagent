package config

import "time"

// Maintenance bounds local observation only; it never declares a provider task terminal.
type Maintenance struct {
	Workers          int
	CancelGrace      time.Duration
	DetachedGrace    time.Duration
	OutageGrace      time.Duration
	InteractionGrace time.Duration
	MaxBackoff       time.Duration
}

func (m Maintenance) Validate() error {
	if m.Workers < 1 || m.Workers > 100000 {
		return invalid("maintenance.workers", "must be between 1 and 100000")
	}
	for _, item := range []struct {
		name  string
		value time.Duration
	}{{"cancel_grace", m.CancelGrace}, {"detached_grace", m.DetachedGrace}, {"outage_grace", m.OutageGrace}, {"interaction_grace", m.InteractionGrace}, {"max_backoff", m.MaxBackoff}} {
		if item.value <= 0 {
			return invalid("maintenance."+item.name, "must be greater than zero")
		}
	}
	return nil
}
