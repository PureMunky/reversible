package factory

import (
	"context"
	"strings"
	"testing"
)

// lossy writes back only the fields it knows: the classic way an old
// version destroys a new version's data during a rollout.
type lossy struct{}

func (lossy) Code(ctx context.Context, s ServiceSpec, prev map[string]string, f []Result) (map[string]string, error) {
	files, err := Codegen{}.Code(ctx, s, prev, f)
	if err == nil {
		files["main.go"] = strings.Replace(files["main.go"], "\t\t\t\tfields[k] = v\n", "\t\t\t\t_ = v\n", 1)
	}
	return files, err
}

func TestMixedVersionsKeepEachOthersData(t *testing.T) {
	ctx := context.Background()
	v1, v2 := build(t, spec(t, "flags-v1.json")), build(t, spec(t, "flags-v2.json"))
	problems, err := MixedCheck(ctx, v1, v2)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
}

func TestMixedCheckCatchesAVersionThatDropsUnknownFields(t *testing.T) {
	ctx := context.Background()
	s := spec(t, "flags-v1.json")
	s.Purpose += " "
	old, err := (&Factory{Root: t.TempDir(), Coder: lossy{}}).Build(ctx, s)
	if err != nil {
		t.Fatalf("the defect is invisible to the examples: %v", err)
	}
	v2 := build(t, spec(t, "flags-v2.json"))

	// Replay and state diffs can't see this: neither runs two versions at once.
	if d, _ := DiffState(ctx, "/nonexistent", old, v2); len(d.Unexpected()) != 0 {
		t.Fatal("unexpected state diff")
	}
	problems, err := MixedCheck(ctx, old, v2)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "rollout_percent set by the candidate was lost when the live version rewrote the record") {
		t.Fatalf("problems: %v", problems)
	}
}
