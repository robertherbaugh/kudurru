package querylog

import (
	"encoding/json"
	"fmt"

	"github.com/robertherbaugh/kudurru/internal/config"
)

// SinkSpecsFrom maps querylog.sinks onto host specs with the defaults
// applied. The settings of a sink type live in the entry's field named
// after the type and reach Sink.Open unchanged as SinkSpec.Config, so a new
// type adds its settings struct, schema fields and factory, and nothing in
// the host.
func SinkSpecsFrom(cfg config.Querylog) ([]SinkSpec, error) {
	specs := make([]SinkSpec, 0, len(cfg.Sinks))
	for i, s := range cfg.Sinks {
		raw, err := json.Marshal(s)
		if err != nil {
			return nil, fmt.Errorf("querylog.sinks[%d]: %w", i, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("querylog.sinks[%d]: %w", i, err)
		}
		specs = append(specs, SinkSpec{
			Name: s.Name, Type: s.Type,
			QueueSize: s.QueueSize, BatchSize: s.BatchSize, Interval: s.FlushInterval.Std(),
			Timeout: s.Timeout.Std(), Attempts: s.Attempts, Backoff: s.RetryBackoff.Std(),
			Config: fields[s.Type],
		}.withDefaults())
	}
	return specs, nil
}
