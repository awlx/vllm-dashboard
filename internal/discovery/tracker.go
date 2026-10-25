package discovery

import (
	"time"

	"github.com/awlx/vllm-dashboard/internal/config"
)

// Tracker keeps the discovered models up to date. A discovered model whose
// process stops stays in the list for the grace period. The poller then
// shows it as offline. After the grace period, the tracker removes it. A
// new model on the URL of a discovered model replaces it at once.
type Tracker struct {
	grace    time.Duration
	lastSeen map[string]time.Time
}

// NewTracker returns a tracker with the given grace period.
func NewTracker(grace time.Duration) *Tracker {
	return &Tracker{grace: grace, lastSeen: map[string]time.Time{}}
}

// Update returns the new model list and the models that it removed. known
// is the current list. found is the result of one discovery run. Configured
// models (Discovered is false) are never removed.
func (t *Tracker) Update(known, found []config.Model, now time.Time) (models, removed []config.Model) {
	byURL := map[string]config.Model{}
	for _, model := range found {
		byURL[model.MetricsURL] = model
	}
	kept := make([]config.Model, 0, len(known))
	for _, model := range known {
		if !model.Discovered {
			kept = append(kept, model)
			continue
		}
		if _, ok := t.lastSeen[model.MetricsURL]; !ok {
			// A model from a scan before the tracker existed starts its grace period now.
			t.lastSeen[model.MetricsURL] = now
		}
		current, running := byURL[model.MetricsURL]
		switch {
		case running && current.Identity == model.Identity:
			model.Unit, model.UserUnit = current.Unit, current.UserUnit
			t.lastSeen[model.MetricsURL] = now
			delete(byURL, model.MetricsURL)
			kept = append(kept, model)
		case running:
			removed = append(removed, model)
		case now.Sub(t.lastSeen[model.MetricsURL]) > t.grace:
			delete(t.lastSeen, model.MetricsURL)
			removed = append(removed, model)
		default:
			kept = append(kept, model)
		}
	}
	var fresh []config.Model
	for _, model := range found {
		if _, ok := byURL[model.MetricsURL]; ok {
			fresh = append(fresh, model)
			t.lastSeen[model.MetricsURL] = now
		}
	}
	return Merge(kept, fresh), removed
}
