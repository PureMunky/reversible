package factory

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/PureMunky/reversible/model"
)

// The booking service (cmd/bookings) is the behaviours fixture: logic
// stated in words, with recorded model replies to the exact prompts.
func bookingSpec(t *testing.T, n string) ServiceSpec {
	t.Helper()
	s, err := LoadSpec("../cmd/bookings/intent/bookings-" + n + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var recorded = model.Replayer{FS: os.DirFS("../cmd/bookings/recordings")}

func TestBehaviourSpecsAreReady(t *testing.T) {
	for _, n := range []string{"1", "2"} {
		if qs := bookingSpec(t, n).Check(); len(qs) > 0 {
			t.Fatalf("bookings-%s: %v", n, qs)
		}
	}
}

func TestCheckHoldsBehavioursToTheirExamples(t *testing.T) {
	cases := map[string]struct {
		edit func(*ServiceSpec)
		want string
	}{
		"a behaviour with no examples": {func(s *ServiceSpec) {
			var kept []Example
			for _, e := range s.Examples {
				if e.Behaviour != "opening_hours" {
					kept = append(kept, e)
				}
			}
			s.Examples = kept
		}, "no example shows opening_hours"},
		"a behaviour with no rule": {func(s *ServiceSpec) { s.Resources[1].Behaviours[0].Rule = " " }, "what does it do"},
		"a computed field nobody computes": {func(s *ServiceSpec) {
			s.Resources[1].Behaviours = s.Resources[1].Behaviours[1:]
			s.Examples = withoutBehaviour(s.Examples, "booking_price")
		}, "which behaviour computes it"},
		"computing a field callers set":  {func(s *ServiceSpec) { s.Resources[1].Behaviours[0].Field = "customer" }, "must be marked computed"},
		"an unknown kind":                {func(s *ServiceSpec) { s.Resources[1].Behaviours[1].Kind = "trigger" }, "not one of computed, check, action"},
		"an example naming no behaviour": {func(s *ServiceSpec) { s.Examples[0].Behaviour = "nope" }, "isn't defined"},
		"a check on delete":              {func(s *ServiceSpec) { s.Resources[1].Behaviours[1].On = []string{"delete"} }, "not \"delete\""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := clone(bookingSpec(t, "1"))
			c.edit(&s)
			var blocking []string
			for _, q := range s.Check() {
				if q.Severity == Blocking {
					blocking = append(blocking, q.String())
				}
			}
			if !strings.Contains(strings.Join(blocking, "\n"), c.want) {
				t.Fatalf("want a blocking question containing %q, got %v", c.want, blocking)
			}
		})
	}
}

func withoutBehaviour(ex []Example, name string) []Example {
	var out []Example
	for _, e := range ex {
		if e.Behaviour != name {
			out = append(out, e)
		}
	}
	return out
}

func TestTheGeneratorAloneRefusesBehaviours(t *testing.T) {
	if _, err := (Codegen{}).Code(context.Background(), bookingSpec(t, "1"), nil, nil); err == nil || !strings.Contains(err.Error(), "a model has to write them") {
		t.Fatalf("err = %v", err)
	}
	if _, err := (BehaviourCoder{}).Code(context.Background(), bookingSpec(t, "1"), nil, nil); err == nil {
		t.Fatal("BehaviourCoder without a model wrote behaviours")
	}
	// Without behaviours, no model is needed at all.
	if files, err := (BehaviourCoder{}).Code(context.Background(), incidentSpec(t, "1"), nil, nil); err != nil || files["behaviours.go"] != "" {
		t.Fatalf("a spec without behaviours: %v %v", err, len(files))
	}
}

func TestModelWritesOnlyTheBehaviours(t *testing.T) {
	ctx := context.Background()
	f := &Factory{Root: t.TempDir(), Coder: BehaviourCoder{Model: recorded}, MaxAttempts: 1}
	for _, n := range []string{"1", "2"} {
		v, err := f.Build(ctx, bookingSpec(t, n))
		if err != nil {
			t.Fatalf("bookings-%s: %v", n, err)
		}
		if v.Files["main.go"] != Structure(v.Spec) {
			t.Fatal("main.go isn't the generated structure")
		}
		if !strings.Contains(v.Files["behaviours.go"], "func computeBookingPrice(") {
			t.Fatal("behaviours.go doesn't implement the hooks")
		}
	}
}

// A model's first attempt fails some examples; it's sent its code and
// exactly which examples failed, and repairs it.
func TestTheModelRepairsItsBehavioursFromTheFailures(t *testing.T) {
	ctx := context.Background()
	spec := bookingSpec(t, "1")
	good, err := recorded.Complete(ctx, BehaviourSystemPrompt, BehaviourPrompt(spec, Structure(spec), nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	broken := strings.Replace(good, "price *= 1.25", "price *= 1.0", 1) // forgets the weekend surcharge
	var prompts []string
	m := model.Func(func(_ context.Context, system, prompt string) (string, error) {
		prompts = append(prompts, prompt)
		if len(prompts) == 1 {
			return broken, nil
		}
		return good, nil
	})
	v, err := (&Factory{Root: t.TempDir(), Coder: BehaviourCoder{Model: m}}).Build(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if v.Attempts != 2 {
		t.Fatalf("attempts = %d", v.Attempts)
	}
	retry := prompts[1]
	for _, want := range []string{"YOUR PREVIOUS behaviours.go", "price *= 1.0", "a weekend booking costs 25% more"} {
		if !strings.Contains(retry, want) {
			t.Errorf("the retry prompt doesn't contain %q", want)
		}
	}
}

// When a rule changes, the review explains the differences by the
// behaviour; when nothing in the intent changed, a regenerated behaviour
// that answers differently is drift, and is flagged.
func TestReviewExplainsBehaviourChangesAndFlagsDrift(t *testing.T) {
	ctx := context.Background()
	traffic := []Exchange{}
	for _, c := range []HTTPCall{
		{Method: "POST", Path: "/services", Body: map[string]any{"name": "haircut", "minutes": 60, "price": 40}, Caller: "owner"},
		{Method: "POST", Path: "/bookings", Body: map[string]any{"id": "bk-1", "service": "haircut", "customer": "a", "date": "2026-06-14", "time": "10:00"}},
		{Method: "POST", Path: "/bookings", Body: map[string]any{"id": "bk-2", "service": "haircut", "customer": "b", "date": "2026-06-13", "time": "10:00"}},
		{Method: "POST", Path: "/bookings/bk-2/cancel", Body: map[string]any{"at": "2026-06-13T07:00"}},
	} {
		traffic = append(traffic, Exchange{Request: c})
	}
	f := &Factory{Root: t.TempDir(), Coder: BehaviourCoder{Model: recorded}, MaxAttempts: 1}
	v1, err := f.Build(ctx, bookingSpec(t, "1"))
	if err != nil {
		t.Fatal(err)
	}
	v2, err := f.Build(ctx, bookingSpec(t, "2"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := Diff(ctx, traffic, v1, v2)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Unexpected()) > 0 || len(d.Changes) < 2 {
		t.Fatalf("v1→v2: %s", d)
	}
	for _, want := range []string{"behaviour opening_hours changed", "behaviour cancel_booking changed"} {
		if !strings.Contains(d.String(), want) {
			t.Errorf("missing %q in:\n%s", want, d)
		}
	}

	// Same intent, different code. A drift the examples pin (the weekend
	// price) fails the build; a drift they don't (a refusal's wording) passes
	// it, and the review flags it: nothing in the intent explains it.
	spec := bookingSpec(t, "1")
	good, _ := recorded.Complete(ctx, BehaviourSystemPrompt, BehaviourPrompt(spec, Structure(spec), nil, nil))
	coder := func(reply string) Coder {
		return BehaviourCoder{Model: model.Func(func(context.Context, string, string) (string, error) { return reply, nil })}
	}
	pinned := strings.Replace(good, "price *= 1.25", "price *= 1.125", 1)
	if _, err := (&Factory{Root: t.TempDir(), Coder: coder(pinned), MaxAttempts: 1}).Build(ctx, spec); err == nil {
		t.Fatal("a drifted weekend price passed its examples")
	}
	reworded := strings.Replace(good, "bookings must fit within opening hours", "outside opening hours", 1)
	vd, err := (&Factory{Root: t.TempDir(), Coder: coder(reworded), MaxAttempts: 1}).Build(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	late := append(traffic[:1:1], Exchange{Request: HTTPCall{Method: "POST", Path: "/bookings", Body: map[string]any{"id": "bk-3", "service": "haircut", "customer": "c", "date": "2026-06-10", "time": "16:30"}}})
	dd, err := Diff(ctx, late, v1, vd)
	if err != nil {
		t.Fatal(err)
	}
	if len(dd.Unexpected()) != 1 {
		t.Fatalf("drift without an intent change should be unexplained:\n%s", dd)
	}
}
