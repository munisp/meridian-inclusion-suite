package store

import (
	"fmt"
	"testing"
)

type benchDoc struct {
	ID    string `json:"id"`
	Value int    `json:"value"`
}

func BenchmarkPutFile5000(b *testing.B) {
	st, _ := Open(b.TempDir() + "/db.json")
	st.EnableDebouncedPersistence()
	for i := 0; i < 5000; i++ {
		if err := st.Put("docs", fmt.Sprintf("d_%d", i), benchDoc{ID: fmt.Sprintf("d_%d", i), Value: i}); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := st.Put("docs", fmt.Sprintf("d_%d", i%5000), benchDoc{ID: "x", Value: i}); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if err := st.Flush(); err != nil {
		b.Fatal(err)
	}
}

func BenchmarkList10000(b *testing.B) {
	st, _ := Open("")
	for i := 0; i < 10000; i++ {
		_ = st.Put("docs", fmt.Sprintf("d_%d", i), benchDoc{ID: fmt.Sprintf("d_%d", i), Value: i})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var out []benchDoc
		if err := st.List("docs", &out); err != nil {
			b.Fatal(err)
		}
		if len(out) != 10000 {
			b.Fatal(len(out))
		}
	}
}

func BenchmarkListWhere10000(b *testing.B) {
	st, _ := Open("")
	st.RegisterIndex("docs", "id")
	for i := 0; i < 10000; i++ {
		_ = st.Put("docs", fmt.Sprintf("d_%d", i), benchDoc{ID: fmt.Sprintf("d_%d", i), Value: i})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var out []benchDoc
		if err := st.ListWhere("docs", "id", "d_9999", &out); err != nil {
			b.Fatal(err)
		}
		if len(out) != 1 {
			b.Fatal(len(out))
		}
	}
}
