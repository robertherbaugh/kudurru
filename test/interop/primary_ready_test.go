package interop

import (
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// primaryStatusProbe waits for both the controller's Ready projection and the
// API's live committed serving view. An early successful SOA can precede that
// status write, whose new source RV temporarily closes DNS admission. Historical
// Ready=true alone also cannot prove that source alignment has finished.
// This probe reads readiness only; it never attempts or retries a transfer.
func primaryStatusProbe(t *testing.T, spec kudurrutest.NodeSpec) func() (uint32, bool) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), kudurruBin, "admin", "create-token", "--socket", filepath.Join(spec.DataDir, "admin.sock")).Output() //nolint:gosec // test-owned binary and socket
	if err != nil {
		t.Fatalf("create primary fixture token: %v", err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		t.Fatal("primary fixture token is empty")
	}
	client := &http.Client{Timeout: 3 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	return func() (uint32, bool) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+spec.Ports.API+"/api/v1/zones/interop-test", nil)
		if err != nil {
			t.Fatal("build primary readiness request")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			return 0, false
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusNotFound {
			return 0, false
		}
		// In particular, a credential or authority refusal is a test failure.
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("primary readiness read: HTTP %d", resp.StatusCode)
		}
		var detail struct {
			Ready  bool           `json:"ready"`
			Serial uint32         `json:"serial"`
			Object public.DNSZone `json:"object"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&detail); err != nil {
			t.Fatal("decode primary readiness response")
		}
		z := &detail.Object
		ready := meta.FindStatusCondition(z.Status.Conditions, public.ConditionReady)
		if z.Name != "interop-test" || ready == nil || ready.Status != metav1.ConditionTrue ||
			ready.ObservedGeneration != z.Generation || z.Status.ObservedGeneration != z.Generation ||
			!detail.Ready || detail.Serial == 0 || z.Status.Serial != detail.Serial {
			return 0, false
		}
		return detail.Serial, true
	}
}
