package factory

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// With a runner, the factory never executes generated code itself: every
// build's examples, every review and production all go through it.
func TestFactoryRunsGeneratedCodeOnlyThroughTheRunner(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	h, stopAll := RunnerHandler(root)
	defer stopAll()
	srv := httptest.NewServer(h)
	defer srv.Close()
	t.Setenv("REVERSIBLE_RUNNER", srv.URL)
	t.Setenv("TMPDIR", root) // as in the pod: the factory works on the volume it shares with the runner

	before := mRunnerStartsValue()
	f := &Factory{Root: filepath.Join(root, "builds"), Coder: Codegen{}}
	v1, err := f.Build(ctx, incidentSpec(t, "1"))
	if err != nil {
		t.Fatal(err)
	}
	v2, err := f.Build(ctx, incidentSpec(t, "2"))
	if err != nil {
		t.Fatal(err)
	}
	if problems, err := MixedCheck(ctx, v1, v2); err != nil || len(problems) > 0 {
		t.Fatalf("mixed check through the runner: %v %v", problems, err)
	}
	if started := mRunnerStartsValue() - before; started < 4 {
		t.Fatalf("runner started %v services; the factory ran them itself", started)
	}

	// Outside the root is refused.
	if _, err := Start(ctx, "/bin/true", ""); err == nil || !strings.Contains(err.Error(), "must be under") {
		t.Fatalf("runner ran a binary outside its root: %v", err)
	}
}

func mRunnerStartsValue() float64 {
	var b strings.Builder
	tel.WritePrometheus(&b)
	for _, l := range strings.Split(b.String(), "\n") {
		if strings.HasPrefix(l, `reversible_runner_starts_total{result="started"} `) {
			var v float64
			_, _ = fmtSscan(strings.Fields(l)[1], &v)
			return v
		}
	}
	return 0
}

func fmtSscan(s string, v *float64) (int, error) { return fmt.Sscan(s, v) }
