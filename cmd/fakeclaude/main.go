// Command fakeclaude is a stand-in for the Claude Messages API, for
// rehearsing a live run with no key and no cost. It answers coding
// requests (factory.ModelCoder's prompt) with what the deterministic
// generator writes for the spec in the prompt, wrapped the way a model
// would reply. Point a demo at it with ANTHROPIC_BASE_URL.
//
// A rehearsal proves the plumbing, not the model: the key reaches the
// process, requests leave and come back, a reply is parsed, and the code
// in it is built and run where it would be in a live run (in kind, inside
// the locked-down pod). It can't draft specs from conversation, so it
// rehearses cmd/incidents, which only asks for code.
//
//	go run ./cmd/fakeclaude -addr :8080
//	ANTHROPIC_API_KEY=rehearsal ANTHROPIC_BASE_URL=http://localhost:8080 go run ./cmd/incidents
//
// -flaky N fails the first N requests with an overload error, to rehearse
// the client's retries.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/PureMunky/reversible/factory"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flaky := flag.Int("flaky", 0, "fail this many requests first with 529 overloaded, to rehearse retries")
	flag.Parse()
	var mu sync.Mutex
	failed := 0
	http.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	http.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" {
			apiError(w, 401, "authentication_error", "missing x-api-key")
			return
		}
		mu.Lock()
		fail := failed < *flaky
		if fail {
			failed++
		}
		mu.Unlock()
		if fail {
			log.Printf("rehearsing an overload: 529")
			apiError(w, 529, "overloaded_error", "rehearsal: overloaded")
			return
		}
		var req struct {
			Model    string `json:"model"`
			System   string `json:"system"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) == 0 {
			apiError(w, 400, "invalid_request_error", "unreadable request")
			return
		}
		prompt := req.Messages[0].Content
		if req.System != factory.CoderSystemPrompt {
			apiError(w, 400, "invalid_request_error", "fakeclaude only rehearses coding requests (factory.ModelCoder); it can't draft specs from conversation")
			return
		}
		spec, err := specIn(prompt)
		if err != nil {
			apiError(w, 400, "invalid_request_error", err.Error())
			return
		}
		files, err := factory.Codegen{}.Code(context.Background(), spec, nil, nil)
		if err != nil {
			apiError(w, 500, "api_error", err.Error())
			return
		}
		text := "```go\n" + files["main.go"] + "```\n"
		in, out := len(req.System+prompt)/4, len(text)/4 // rough, for the usage line
		log.Printf("wrote %s for spec %s (%s): ~%d in / ~%d out tokens", "main.go", spec.Hash(), spec.Service, in, out)
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"type": "message", "role": "assistant", "model": req.Model, "stop_reason": "end_turn",
			"content": []map[string]string{{"type": "text", "text": text}},
			"usage":   map[string]int{"input_tokens": in, "output_tokens": out},
		})
	})
	log.Printf("fakeclaude: rehearsal stand-in for the Messages API on %s (no model, no cost)", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// specIn pulls the spec out of a ModelCoder prompt: the JSON object after
// the SPECIFICATION heading.
func specIn(prompt string) (factory.ServiceSpec, error) {
	i := strings.Index(prompt, "\nSPECIFICATION\n")
	if i < 0 {
		return factory.ServiceSpec{}, fmt.Errorf("no SPECIFICATION in the prompt")
	}
	dec := json.NewDecoder(strings.NewReader(prompt[i+len("\nSPECIFICATION\n"):]))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return factory.ServiceSpec{}, fmt.Errorf("specification: %v", err)
	}
	return factory.ParseSpec(raw)
}

func apiError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}})
}
