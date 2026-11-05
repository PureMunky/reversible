package factory

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// compile writes generated files into dir and builds them into a binary.
// Generated code runs as its own process, never inside the factory.
func compile(ctx context.Context, dir string, files map[string]string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if _, ok := files["main.go"]; !ok {
		return "", errors.New("generated code has no main.go")
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module generated\n\ngo 1.22\n"), 0o644); err != nil {
		return "", err
	}
	for name, src := range files {
		if strings.Contains(name, "/") || strings.Contains(name, "..") || !strings.HasSuffix(name, ".go") {
			return "", fmt.Errorf("refusing generated file name %q", name)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			return "", err
		}
	}
	bin := filepath.Join(dir, "service")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "CGO_ENABLED=0", "GOFLAGS=-mod=mod")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build failed:\n%s", strings.TrimSpace(string(out)))
	}
	return bin, nil
}

// Instance is a running generated service.
type Instance struct {
	URL string
	cmd *exec.Cmd
}

// Start runs a built service. dataPath is its persistent state ("" for none).
func Start(ctx context.Context, binary, dataPath string) (*Instance, error) {
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(), "ADDR=127.0.0.1:0", "DATA="+dataPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	line := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		if sc.Scan() {
			line <- sc.Text()
		}
		close(line)
		io.Copy(io.Discard, stdout)
	}()
	select {
	case l, ok := <-line:
		if !ok || !strings.HasPrefix(l, "LISTENING ") {
			cmd.Process.Kill()
			cmd.Wait()
			return nil, fmt.Errorf("service did not announce its address (got %q): %s", l, strings.TrimSpace(stderr.String()))
		}
		return &Instance{URL: strings.TrimSpace(strings.TrimPrefix(l, "LISTENING ")), cmd: cmd}, nil
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		cmd.Wait()
		return nil, errors.New("service did not start within 10s")
	case <-ctx.Done():
		cmd.Process.Kill()
		cmd.Wait()
		return nil, ctx.Err()
	}
}

// Stop shuts the service down.
func (i *Instance) Stop() {
	if i == nil || i.cmd == nil || i.cmd.Process == nil {
		return
	}
	i.cmd.Process.Kill()
	i.cmd.Wait()
}

// Response is what a service returned.
type Response struct {
	Status int `json:"status"`
	Body   any `json:"body,omitempty"`
}

var client = &http.Client{Timeout: 10 * time.Second}

// Call sends one request.
func Call(ctx context.Context, base string, c HTTPCall) (Response, error) {
	var body io.Reader
	if c.Body != nil {
		b, err := json.Marshal(c.Body)
		if err != nil {
			return Response{}, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, c.Method, base+c.Path, body)
	if err != nil {
		return Response{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Caller != "" {
		req.Header.Set("X-Caller", c.Caller)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	out := Response{Status: resp.StatusCode}
	if len(bytes.TrimSpace(raw)) > 0 {
		if json.Unmarshal(raw, &out.Body) != nil {
			out.Body = string(raw)
		}
	}
	return out, nil
}

// Result is one example checked against a running build.
type Result struct {
	Example Example
	Got     Response
	Pass    bool
	Problem string
}

// runExamples starts a fresh instance and runs every example in order.
func runExamples(ctx context.Context, binary string, examples []Example) ([]Result, error) {
	dir, err := os.MkdirTemp("", "factory-verify-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	inst, err := Start(ctx, binary, filepath.Join(dir, "data.json"))
	if err != nil {
		return nil, err
	}
	defer inst.Stop()
	var out []Result
	for _, e := range examples {
		got, err := Call(ctx, inst.URL, e.Request)
		r := Result{Example: e, Got: got}
		switch {
		case err != nil:
			r.Problem = err.Error()
		case got.Status != e.Expect.Status:
			r.Problem = fmt.Sprintf("expected status %d, got %d %s", e.Expect.Status, got.Status, compact(got.Body))
		case e.Expect.Body != nil && !subset(e.Expect.Body, got.Body):
			r.Problem = fmt.Sprintf("expected body to include %s, got %s", compact(e.Expect.Body), compact(got.Body))
		default:
			r.Pass = true
		}
		out = append(out, r)
	}
	return out, nil
}

// subset reports whether every key and element in want appears in got.
func subset(want, got any) bool {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range w {
			if !subset(v, g[k]) {
				return false
			}
		}
		return true
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !subset(w[i], g[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(want, got)
	}
}

func compact(v any) string {
	if v == nil {
		return "(empty)"
	}
	b, _ := json.Marshal(v)
	s := string(b)
	if len(s) > 200 {
		s = s[:197] + "..."
	}
	return s
}
