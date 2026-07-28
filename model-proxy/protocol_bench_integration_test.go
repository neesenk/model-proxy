package main

import (
	"bytes"
	"testing"
)

// A 1KB body for the estimation benchmark.
var benchBody1K = bytes.Repeat([]byte("The quick brown fox jumps over the lazy dog. "), 25)

func BenchmarkEstimateInputTokens(b *testing.B) {
	b.SetBytes(int64(len(benchBody1K)))
	for i := 0; i < b.N; i++ {
		estimateInputTokens(benchBody1K)
	}
}
