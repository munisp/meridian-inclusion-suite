package main

import (
	"fmt"
	"testing"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

func BenchmarkFindDuplicateLevy10000(b *testing.B) {
	st, _ := store.Open("")
	st.RegisterIndex("payments", "tin_hash")
	svc := &PaymentService{st: st}
	for i := 0; i < 10000; i++ {
		id := fmt.Sprintf("pay_%d", i)
		_ = st.Put("payments", id, Payment{ID: id, TINHash: fmt.Sprintf("tin_%d", i), Period: "2026", Status: "captured"})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := svc.findDuplicateLevy("tin_9999", "2026", false); err != nil {
			b.Fatal(err)
		}
	}
}
