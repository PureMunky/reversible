package factory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
)

// MixedCheck runs the live version and the candidate side by side on the
// same data, the way a canary does, and checks that neither destroys the
// other's data. A record the candidate writes with its new fields is
// rewritten by the live version, then read back by the candidate: the new
// fields must survive. The same in reverse for fields only the live
// version knows. A version that drops fields it doesn't recognise passes
// every example and every replay, and quietly corrupts data the moment
// two versions overlap; this is the check that sees it.
//
// It returns one problem per field that didn't survive.
func MixedCheck(ctx context.Context, live, candidate *Version) ([]string, error) {
	dir, err := os.MkdirTemp("", "factory-mixed-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	data := filepath.Join(dir, "shared.json")
	a, err := Start(ctx, live.Binary, data)
	if err != nil {
		return nil, err
	}
	defer a.Stop()
	b, err := Start(ctx, candidate.Binary, data)
	if err != nil {
		return nil, err
	}
	defer b.Stop()

	var problems []string
	check := func(writer, rewriter, reader *Instance, writerSpec, rewriterSpec ServiceSpec, key, writerName, rewriterName string) error {
		only := fieldsOnlyIn(writerSpec, rewriterSpec)
		if len(only) == 0 {
			return nil
		}
		body, ok := sampleRecord(writerSpec, key, only)
		if !ok {
			problems = append(problems, fmt.Sprintf("couldn't build a sample %s to check (key pattern); fields %v unchecked", writerSpec.Resource.Name, names(only)))
			return nil
		}
		r := writerSpec.Resource
		created, err := Call(ctx, writer.URL, HTTPCall{Method: "POST", Path: "/" + r.Plural, Body: body})
		if err != nil {
			return err
		}
		if created.Status/100 != 2 {
			problems = append(problems, fmt.Sprintf("the %s couldn't create a sample %s: %d %s", writerName, r.Name, created.Status, short(created.Body)))
			return nil
		}
		if patch, ok := touch(writerSpec, rewriterSpec); ok {
			resp, err := Call(ctx, rewriter.URL, HTTPCall{Method: "PATCH", Path: "/" + r.Plural + "/" + key, Body: patch})
			if err != nil {
				return err
			}
			if resp.Status/100 != 2 {
				problems = append(problems, fmt.Sprintf("the %s couldn't update a %s the %s created: %d %s", rewriterName, r.Name, writerName, resp.Status, short(resp.Body)))
				return nil
			}
		}
		got, err := Call(ctx, reader.URL, HTTPCall{Method: "GET", Path: "/" + r.Plural + "/" + key})
		if err != nil {
			return err
		}
		m, _ := got.Body.(map[string]any)
		for _, f := range only {
			if !reflect.DeepEqual(m[f.Name], body[f.Name]) {
				problems = append(problems, fmt.Sprintf("%s set by the %s was lost when the %s rewrote the record (wrote %v, read back %v)",
					f.Name, writerName, rewriterName, body[f.Name], m[f.Name]))
			}
		}
		return nil
	}
	if err := check(b, a, b, candidate.Spec, live.Spec, "mixed-check-new", "candidate", "live version"); err != nil {
		return nil, err
	}
	if err := check(a, b, a, live.Spec, candidate.Spec, "mixed-check-old", "live version", "candidate"); err != nil {
		return nil, err
	}
	return problems, nil
}

func fieldsOnlyIn(a, b ServiceSpec) []Field {
	inB := map[string]bool{}
	for _, f := range b.Resource.Fields {
		inB[f.Name] = true
	}
	var out []Field
	for _, f := range a.Resource.Fields {
		if !inB[f.Name] && f.Name != a.Resource.Key {
			out = append(out, f)
		}
	}
	return out
}

func names(fs []Field) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return out
}

// sampleValue picks a valid value for a field that differs from its
// default, so it's visible whether it survived.
func sampleValue(f Field) (any, bool) {
	switch f.Type {
	case "bool":
		d, _ := f.Default.(bool)
		return !d, true
	case "int", "number":
		v := 7.0
		if f.Min != nil && f.Max != nil {
			v = float64(int((*f.Min + *f.Max) / 2))
		} else if f.Min != nil {
			v = *f.Min + 1
		} else if f.Max != nil {
			v = *f.Max - 1
		}
		if d, ok := f.Default.(float64); ok && d == v {
			v++
			if f.Max != nil && v > *f.Max {
				v -= 2
			}
		}
		return v, true
	case "string":
		if len(f.Enum) > 0 {
			return f.Enum[len(f.Enum)-1], true
		}
		v := "mixed-check"
		if f.Pattern != "" && !regexp.MustCompile(f.Pattern).MatchString(v) {
			return nil, false
		}
		return v, true
	}
	return nil, false
}

func sampleRecord(s ServiceSpec, key string, extra []Field) (map[string]any, bool) {
	r := s.Resource
	body := map[string]any{r.Key: key}
	for _, f := range r.Fields {
		if f.Name == r.Key {
			if f.Pattern != "" && !regexp.MustCompile(f.Pattern).MatchString(key) {
				return nil, false
			}
			continue
		}
		if f.Required {
			v, ok := sampleValue(f)
			if !ok {
				return nil, false
			}
			body[f.Name] = v
		}
	}
	for _, f := range extra {
		if v, ok := sampleValue(f); ok {
			body[f.Name] = v
		}
	}
	return body, true
}

// touch builds an update to a field both versions know, so the rewriting
// version has to rewrite the whole record.
func touch(writer, rewriter ServiceSpec) (map[string]any, bool) {
	inW := map[string]Field{}
	for _, f := range writer.Resource.Fields {
		inW[f.Name] = f
	}
	for _, f := range rewriter.Resource.Fields {
		if f.Name == rewriter.Resource.Key {
			continue
		}
		if _, ok := inW[f.Name]; !ok {
			continue
		}
		if v, ok := sampleValue(f); ok {
			if f.Type == "string" {
				if s, _ := v.(string); s == "mixed-check" {
					v = "touched-by-the-other-version"
					if f.Pattern != "" && !regexp.MustCompile(f.Pattern).MatchString(v.(string)) {
						continue
					}
				}
			}
			return map[string]any{f.Name: v}, true
		}
	}
	return nil, false
}
