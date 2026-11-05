# Working in this repo

Read `PRINCIPLES.md` first. The goal of this project is to **change the way software is built**: intent is the source of truth, code is a cache generated from it, history is decisions rather than diffs, reversibility replaces permission, and behaviour (not code) is what gets reviewed.

Everything here exists to serve that idea. When you plan, build, document or commit:

- **Apply the framing check** in `PRINCIPLES.md` to every change: which principle it serves, whether it treats the software itself as the thing being built, whether it is domain-neutral, and where a human sits and why.
- **Lead with the root idea.** In plans, READMEs, demo output and summaries, say how the work moves toward software being built from intent, before describing mechanics.
- **Don't let a domain become the point.** Pricing, refunds and customers are examples. New demos should show the mechanism in other domains (infrastructure, internal tools, APIs), and docs should say when something is just an example.
- **Name the gap honestly.** If a piece only governs behaviour a person still writes by hand, say so, and say what's missing for the software itself to be generated.
- **Commit messages** end their body with a line `Toward the root idea: ...` naming the principle the change serves.
- **Keep `PRINCIPLES.md` current.** Update its "where the repo stands" table when a principle moves forward.

Engineering conventions: Go, standard library only; `gofmt`, `go vet` and `go test -race ./...` must pass; every demo runs in CI with assertions on its output.
