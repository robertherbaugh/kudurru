package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// BenchmarkListZones1000 measures GET /api/v1/zones over 1,000 zones with
// one record set each, through the full middleware chain and the binding
// authorizer (admin at cluster scope).
func BenchmarkListZones1000(b *testing.B) {
	h := newHarness(b)
	for i := range 1000 {
		z := zoneObj(fmt.Sprintf("zone-%04d", i), fmt.Sprintf("z%04d.example.", i))
		rs := recordSetObj("default", fmt.Sprintf("rs-%04d", i), z.Name, ep(fmt.Sprintf("www.z%04d.example", i), "A", "192.0.2.1"))
		for _, o := range []store.Object{z, rs} {
			if _, err := h.st.Apply(context.Background(), o, store.ApplyOptions{Manager: "api"}); err != nil {
				b.Fatal(err)
			}
		}
	}
	for _, user := range []string{"admin", "zoneadmin"} {
		b.Run(user, func(b *testing.B) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/zones", nil)
			req.Header.Set("Authorization", apitest.Bearer(user))
			b.ReportAllocs()
			for b.Loop() {
				rec := httptest.NewRecorder()
				h.srv.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					b.Fatalf("status %d", rec.Code)
				}
			}
		})
	}
}
