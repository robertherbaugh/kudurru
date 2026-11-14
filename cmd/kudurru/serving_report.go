package main

import (
	"errors"
	"log/slog"
	"sync"

	"github.com/robertherbaugh/kudurru/internal/serving"
)

// servingFailureLog writes why the complete serving generation is not
// published. Readiness and the API report only that it is unavailable
// (prepare_failed, generation_pending): which object or owner the candidate
// failed on is for the operator, and without this line it was nowhere.
//
// The publisher tries again every second while a candidate keeps failing, so
// a failure is logged when it starts and when its text changes, and once
// more when a generation is published again. A candidate that only lost the
// race against a newer one is not a failure.
type servingFailureLog struct {
	logger *slog.Logger // nil: the default logger at the time of the report

	mu   sync.Mutex
	last string
}

// maxServingFailureText bounds one logged error.
const maxServingFailureText = 2000

// report is serving.Options.Report.
func (l *servingFailureLog) report(reason string, err error) {
	text := ""
	if err != nil && (reason != "candidate_changed" || errors.Is(err, serving.ErrDependency)) {
		text = err.Error()
		if len(text) > maxServingFailureText {
			text = text[:maxServingFailureText] + " ..."
		}
	}
	if err != nil && text == "" {
		return // a superseded candidate says nothing about the next one
	}
	l.mu.Lock()
	previous := l.last
	l.last = text
	l.mu.Unlock()
	if text == previous {
		return
	}
	logger := l.logger
	if logger == nil {
		logger = slog.Default()
	}
	if text == "" {
		logger.Info("complete serving generation published again", "component", "serving")
		return
	}
	logger.Warn("complete serving generation not published", "component", "serving", "reason", reason, "error", text)
}
