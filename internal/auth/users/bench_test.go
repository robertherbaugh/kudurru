package users

import (
	"context"
	"testing"
)

// BenchmarkHash measures one Argon2id hash with the §8.7 parameters
// (m=19 MiB, t=2, p=1): the cost of every password login and of the dummy
// hash for unknown users.
func BenchmarkHash(b *testing.B) {
	h := NewHasher(1)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := h.Hash(ctx, "correct horse battery staple"); err != nil {
			b.Fatal(err)
		}
	}
}
