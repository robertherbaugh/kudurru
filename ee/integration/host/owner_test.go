package host

import "testing"

func TestProviderOwnerNeverFollowsSameNameReplacement(t *testing.T) {
	h, j, _, _, _ := engineFixture(t)
	p := providerHost{h, j}
	original := p.IntegrationOwner()
	j.cancel()
	h.mu.Lock()
	replacement := *j
	replacement.guard.Owner.UID = "new-uid"
	h.jobs[j.key] = &replacement
	h.mu.Unlock()
	if got := p.IntegrationOwner(); got != original {
		t.Fatal("old provider inherited recreated owner", got)
	}
	if got := (providerHost{h, &replacement}).IntegrationOwner(); got.UID != "new-uid" {
		t.Fatal("new provider lost its identity", got)
	}
}

func TestProviderOwnerConcurrentGuardRefresh(t *testing.T) {
	h, j, _, _, _ := engineFixture(t)
	p := providerHost{h, j}
	original := p.IntegrationOwner()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			h.mu.Lock()
			j.guard.LeaseID = "new-lease"
			j.guard.IntegrationResourceVersion = "refreshed"
			h.mu.Unlock()
		}
	}()
	for range 1000 {
		if p.IntegrationOwner() != original {
			t.Fatal("guard refresh changed incarnation")
		}
	}
	<-done
}
