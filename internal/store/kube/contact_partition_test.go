package kube

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type contactPartitionRoundTrip func(*http.Request) (*http.Response, error)

func (trip contactPartitionRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return trip(request)
}

func TestContactPartitionDrainsAdmittedIO(t *testing.T) {
	for _, phase := range []string{"response", "body-read"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				markEntered, markRelease := make(chan struct{}), make(chan struct{})
				arm := false
				tracker := &contact{now: func() time.Time {
					if arm {
						arm = false
						close(markEntered)
						<-markRelease
					}
					return time.Now()
				}}
				partition := &outage{}
				partition.connect(t.Context())
				transport := partition.wrap(tracker.wrap(contactPartitionRoundTrip(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("watch event"))}, nil
				})))
				request := httptest.NewRequest(http.MethodGet, "/", nil)
				complete := func() error {
					response, err := transport.RoundTrip(request)
					if err == nil {
						err = response.Body.Close()
					}
					return err
				}
				if phase == "body-read" {
					response, err := transport.RoundTrip(request)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = response.Body.Close() }()
					complete = func() error {
						_, err := response.Body.Read(make([]byte, 1))
						return err
					}
				}
				before := tracker.Last()
				time.Sleep(time.Nanosecond)
				arm = true
				done := make(chan error, 1)
				go func() { done <- complete() }()
				<-markEntered
				drained := make(chan time.Time, 1)
				go func() {
					partition.disconnect()
					drained <- tracker.Last()
				}()
				synctest.Wait()
				if len(drained) != 0 {
					t.Error("partition baseline preceded admitted contact")
				}
				if _, err := transport.RoundTrip(request); !errors.Is(err, errOutage) {
					t.Errorf("new request during the drain: %v, want partition refusal", err)
				}
				close(markRelease)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				frozen := <-drained
				if !frozen.After(before) || !tracker.Last().Equal(frozen) {
					t.Fatal("drained baseline did not include admitted contact")
				}
				time.Sleep(time.Nanosecond)
				if _, err := transport.RoundTrip(request); !errors.Is(err, errOutage) || !tracker.Last().Equal(frozen) {
					t.Fatal("new request changed contact during the partition", err)
				}
				partition.connect(t.Context())
				response, err := transport.RoundTrip(request)
				if err != nil {
					t.Fatal(err)
				}
				_ = response.Body.Close()
				if !tracker.Last().After(frozen) {
					t.Fatal("fresh response did not advance contact after healing")
				}
			})
		})
	}
}
