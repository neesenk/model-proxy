package web

import (
	"reflect"
	"testing"
)

// The browser e2e suites each start their own browser and serve stack, and
// node runs test files concurrently by default — which is why the documented
// on-demand gate passes --test-concurrency=1 (docs/engineering/testing.md
// 「UI 浏览器 e2e」). The Go driver must serialize them the same way whenever
// MP_UI_E2E turns them on, and stay plainly concurrent otherwise.
func TestJSTestArgsSerializeBrowserE2E(t *testing.T) {
	t.Parallel()

	files := []string{"jstests/a.test.mjs", "jstests/uie2e.test.mjs"}
	if got := jsTestArgs(files, false); !reflect.DeepEqual(got, []string{"--test", files[0], files[1]}) {
		t.Fatalf("args without browser e2e = %v", got)
	}
	want := []string{"--test", "--test-concurrency=1", files[0], files[1]}
	if got := jsTestArgs(files, true); !reflect.DeepEqual(got, want) {
		t.Fatalf("args with browser e2e = %v, want %v", got, want)
	}
}
