// Command agentctl drives a durable reversible runtime from the shell.
// Every invocation is a new process, so it shows escrow, undo and crash
// recovery surviving restarts.
//
//	agentctl onboard acme          # account created, charge + email held
//	agentctl status                # list intents
//	agentctl undo onboard-acme     # as if it never happened
//	agentctl release onboard-acme  # apply held effects now
//	agentctl tick                  # apply escrow whose hold window passed
//	agentctl crash initech         # die mid-intent; next run reconciles and recovers it
//	agentctl world                 # show the fake world
//	agentctl log onboard-acme      # ledger for one intent
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/PureMunky/reversible"
)

// world is a stand-in for real systems (a database, a payment provider, an
// email service). It lives in a file so it outlives each process.
type world struct {
	Accounts   map[string]bool   `json:"accounts"`
	Workspaces map[string]string `json:"workspaces"` // idempotency key -> workspace
	Charges    map[string]int    `json:"charges"`
	Outbox     []string          `json:"outbox"`
}

type worldFile string

func (f worldFile) update(fn func(*world) error) error {
	w, err := f.load()
	if err != nil {
		return err
	}
	if err := fn(w); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(w, "", "  ")
	return os.WriteFile(string(f), b, 0o644)
}

func (f worldFile) load() (*world, error) {
	w := &world{Accounts: map[string]bool{}, Workspaces: map[string]string{}, Charges: map[string]int{}}
	b, err := os.ReadFile(string(f))
	if errors.Is(err, os.ErrNotExist) {
		return w, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, w); err != nil {
		return nil, err
	}
	if w.Workspaces == nil {
		w.Workspaces = map[string]string{}
	}
	return w, nil
}

func (w *world) String() string {
	var accts []string
	for a := range w.Accounts {
		accts = append(accts, a)
	}
	sort.Strings(accts)
	total := 0
	for _, c := range w.Charges {
		total += c
	}
	var wss []string
	for _, ws := range w.Workspaces {
		wss = append(wss, ws)
	}
	sort.Strings(wss)
	return fmt.Sprintf("accounts=%v  workspaces=%v  charged=$%d  emails_sent=%v", accts, wss, total, w.Outbox)
}

func registry(wf worldFile, crash bool) *reversible.Registry {
	reg := reversible.NewRegistry()
	reg.MustRegister("account.create", reversible.Handler{
		Class:    reversible.Reversible,
		Describe: func(p reversible.Params) string { return "create account " + p["name"] },
		Do: func(_ context.Context, p reversible.Params) (reversible.Receipt, error) {
			return reversible.Receipt{"account": p["name"]}, wf.update(func(w *world) error {
				w.Accounts[p["name"]] = true
				return nil
			})
		},
		Undo: func(_ context.Context, _ reversible.Params, r reversible.Receipt) error {
			return wf.update(func(w *world) error { delete(w.Accounts, r["account"]); return nil })
		},
	})
	reg.MustRegister("workspace.provision", reversible.Handler{
		Class:    reversible.Reversible,
		Describe: func(p reversible.Params) string { return "provision workspace for " + p["name"] },
		// The workspace is keyed by idempotency key, the way a real provider
		// would dedupe requests. That is what makes Reconcile possible.
		Do: func(ctx context.Context, p reversible.Params) (reversible.Receipt, error) {
			key, ws := reversible.IdempotencyKey(ctx), "ws-"+p["name"]
			if err := wf.update(func(w *world) error { w.Workspaces[key] = ws; return nil }); err != nil {
				return nil, err
			}
			if crash {
				fmt.Println("💥 process killed after the workspace was created, before it was recorded (simulated)")
				os.Exit(137)
			}
			return reversible.Receipt{"workspace": ws}, nil
		},
		Undo: func(ctx context.Context, _ reversible.Params, _ reversible.Receipt) error {
			return wf.update(func(w *world) error { delete(w.Workspaces, reversible.IdempotencyKey(ctx)); return nil })
		},
		Reconcile: func(ctx context.Context, _ reversible.Params) (bool, reversible.Receipt, error) {
			w, err := wf.load()
			if err != nil {
				return false, nil, err
			}
			ws, ok := w.Workspaces[reversible.IdempotencyKey(ctx)]
			if !ok {
				return false, nil, nil
			}
			return true, reversible.Receipt{"workspace": ws}, nil
		},
	})
	reg.MustRegister("card.charge", reversible.Handler{
		Class:    reversible.Escrowed,
		Describe: func(p reversible.Params) string { return fmt.Sprintf("charge %s $%s", p["name"], p["amount"]) },
		Do: func(_ context.Context, p reversible.Params) (reversible.Receipt, error) {
			var amt int
			fmt.Sscan(p["amount"], &amt)
			return reversible.Receipt{"account": p["name"]}, wf.update(func(w *world) error {
				w.Charges[p["name"]] += amt
				return nil
			})
		},
		Undo: func(_ context.Context, p reversible.Params, _ reversible.Receipt) error {
			var amt int
			fmt.Sscan(p["amount"], &amt)
			return wf.update(func(w *world) error { w.Charges[p["name"]] -= amt; return nil })
		},
	})
	reg.MustRegister("email.welcome", reversible.Handler{
		Class:    reversible.Escrowed,
		Describe: func(p reversible.Params) string { return "send welcome email to " + p["name"] },
		Do: func(_ context.Context, p reversible.Params) (reversible.Receipt, error) {
			return nil, wf.update(func(w *world) error { w.Outbox = append(w.Outbox, p["name"]); return nil })
		},
	})
	return reg
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: agentctl [-dir DIR] [-hold DURATION] <command> [args]

commands:
  onboard NAME [AMOUNT]   create account, provision, hold charge + welcome email
  crash NAME              same, but the process dies while provisioning
  status                  list intents and their state
  undo ID [REASON]        undo an intent
  release ID              apply an intent's held effects now
  tick                    apply held effects whose hold window has passed
  reconcile ID            re-check uncertain steps against the world
  world                   show the fake world
  log ID                  show the ledger for an intent`)
	os.Exit(2)
}

func main() {
	dir := flag.String("dir", ".reversible", "state directory")
	hold := flag.Duration("hold", time.Minute, "escrow hold window")
	flag.Usage = usage
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		usage()
	}
	if err := run(*dir, *hold, args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(dir string, hold time.Duration, args []string) error {
	ctx := context.Background()
	store, err := reversible.NewFileStore(filepath.Join(dir, "intents"))
	if err != nil {
		return err
	}
	wf := worldFile(filepath.Join(dir, "world.json"))
	reg := registry(wf, args[0] == "crash")
	rt := reversible.New(
		reversible.WithStore(store),
		reversible.WithRegistry(reg),
		reversible.WithLedger(reversible.NewFileLedger(filepath.Join(dir, "ledger.jsonl"))),
		reversible.WithHold(hold),
	)
	defer rt.Close()

	// Every process starts by recovering what the last one left behind.
	recovered, err := rt.Recover(ctx)
	if err != nil {
		return fmt.Errorf("recover: %w", err)
	}
	for _, res := range recovered {
		if res.Recovered {
			fmt.Printf("♻️  recovered %s from a crash: %s\n", res.IntentID, res.Status)
			if u := res.Uncertain(); len(u) > 0 {
				fmt.Printf("   uncertain (check the world): %v\n", u)
			}
		}
	}

	switch args[0] {
	case "onboard", "crash":
		if len(args) < 2 {
			usage()
		}
		name, amount := args[1], "49"
		if len(args) > 2 {
			amount = args[2]
		}
		in := reversible.Intent{ID: "onboard-" + name, Goal: fmt.Sprintf("onboard %s at $%s", name, amount),
			Effects: []reversible.Effect{
				reg.MustEffect("create-account", "account.create", reversible.Params{"name": name}),
				reg.MustEffect("provision", "workspace.provision", reversible.Params{"name": name}),
				reg.MustEffect("charge-card", "card.charge", reversible.Params{"name": name, "amount": amount}),
				reg.MustEffect("welcome-email", "email.welcome", reversible.Params{"name": name}),
			}}
		res, err := rt.Execute(ctx, in)
		if res != nil {
			printResult(res)
		}
		return err
	case "status":
		results := rt.List()
		if len(results) == 0 {
			fmt.Println("no intents yet")
		}
		for _, res := range results {
			printResult(res)
		}
	case "undo":
		if len(args) < 2 {
			usage()
		}
		res, err := rt.Undo(ctx, args[1], strings.Join(args[2:], " "))
		if err != nil {
			return err
		}
		printResult(res)
	case "release":
		if len(args) < 2 {
			usage()
		}
		res, err := rt.Release(ctx, args[1])
		if res != nil {
			printResult(res)
		}
		return err
	case "tick":
		results, err := rt.ReleaseDue(ctx)
		if len(results) == 0 {
			fmt.Println("nothing due")
		}
		for _, res := range results {
			printResult(res)
		}
		return err
	case "reconcile":
		if len(args) < 2 {
			usage()
		}
		res, err := rt.Reconcile(ctx, args[1])
		if res != nil {
			printResult(res)
		}
		if err != nil {
			return err
		}
	case "world":
		// handled below
	case "log":
		if len(args) < 2 {
			usage()
		}
		evs, err := rt.Ledger().Events(args[1])
		if err != nil {
			return err
		}
		for _, e := range evs {
			subject := "intent"
			if e.Effect != "" {
				subject = e.Effect
			}
			detail := e.Detail
			if e.Goal != "" {
				detail = "goal: " + e.Goal
			}
			fmt.Printf("%s  %-15s %-12s %s\n", e.Time.Format("15:04:05"), subject, e.Kind, detail)
		}
		return nil
	default:
		usage()
	}
	w, err := wf.load()
	if err != nil {
		return err
	}
	fmt.Println("world:", w)
	return nil
}

func printResult(res *reversible.Result) {
	fmt.Printf("%-22s %-22s %s\n", res.IntentID, res.Status, res.Reason)
	for _, s := range res.Steps {
		extra := ""
		if s.State == reversible.StepHeld {
			extra = "until " + s.ReleaseAt.Format("15:04:05")
		}
		fmt.Printf("    %-15s %-13s %-12s %s\n", s.Effect, s.Class, s.State, extra)
	}
}
