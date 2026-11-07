package internalapi

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// PathEvents receives authenticated peer hints. Delivery is best effort; normal
// refresh timers remain authoritative. Receivers never rebroadcast a peer hint.
const PathEvents = "/internal/events"

// Event identifies an exact configuration generation. No remote data or serial
// is trusted: a secondary hint only asks the receiver to poll its own primary.
type Event struct {
	Type     string `json:"type"`
	Identity string `json:"identity"`
}

func (e Event) valid() bool {
	const prefix = "sha256:"
	decoded, err := hex.DecodeString(strings.TrimPrefix(e.Identity, prefix))
	return e.Type == "secondary-notify" && strings.HasPrefix(e.Identity, prefix) && err == nil && len(decoded) == 32 && len(e.Identity) == len(prefix)+64
}

func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(statusWriteTimeout))
	var event Event
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil || !event.valid() {
		http.Error(w, "invalid event", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid event", http.StatusBadRequest)
		return
	}
	if !s.opts.Event(event) {
		http.Error(w, "event not applicable or queue full", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// SendEvent posts one hint with token rotation support. Each exchange has a
// fixed deadline independent of the caller's potentially unbounded context.
func (c *Client) SendEvent(ctx context.Context, addr string, event Event) error {
	if !event.valid() {
		return errors.New("internalapi: invalid event")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	for _, tok := range c.creds.tokensToTry() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+addr+PathEvents, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		drain(resp)
		if resp.StatusCode == http.StatusAccepted {
			return nil
		}
		if resp.StatusCode == http.StatusUnauthorized {
			continue
		}
		return fmt.Errorf("internalapi: peer event: %s", resp.Status)
	}
	return ErrUnauthorized
}
