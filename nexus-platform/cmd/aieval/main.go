// aieval — A4 regression harness for the AI Builder (EASE Nước 7 enabler).
//
// Runs the golden prompt set (cmd/aieval/golden.json) against a LIVE Nexus server's
// POST /admin/ai/builder/chat and scores two machine-checkable metrics per case:
//
//  1. draft-kind match  — the turn produced ≥1 draft of an EXPECTED kind (or, for adversarial
//     cases, produced ZERO drafts / refused);
//  2. entity match      — when the golden pins an entity name, some draft references it.
//
// Every propose_* tool already validates config-time with the SAME validator the apply path
// uses (validator-parity), so "a draft came back" is itself a strong schema-pass signal —
// pass-rate here drops BEFORE semantic quality does, which is exactly what a cheap regression
// gate needs. Run this after ANY persona/model/tool change (prompt-diet, model swap) and
// compare pass-rates against the last recorded baseline in golden.json's `baseline` field.
//
// Usage:
//
//	go run ./cmd/aieval \
//	  -base http://localhost:8090 -tenant lab -email admin@lab.vn -pass '...' \
//	  [-only case-id] [-timeout 180]
//
// NEVER point this at a customer tenant: turns burn real tokens and mint sessions. Use the
// lab tenant. The tool is read-only toward the golden file and never applies drafts.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

type goldenCase struct {
	ID     string `json:"id"`
	Group  string `json:"group"` // demo | edge | adversarial | replay
	Prompt string `json:"prompt"`
	// ExpectKinds: at least ONE draft of one of these kinds must appear. Empty + ExpectNoDraft
	// false = any draft counts. For adversarial cases set ExpectNoDraft.
	ExpectKinds []string `json:"expectKinds,omitempty"`
	// ExpectAllKinds requires EVERY named kind, where ExpectKinds requires only one of them.
	//
	// "Any one of" is a weak bar and it hid the thing this harness exists to catch: a request that
	// needs a table AND a screen AND a permission scored a pass when the model proposed only the
	// table. What the person asked for was an app they could use; what was measured was that the
	// assistant said something on-topic.
	ExpectAllKinds []string `json:"expectAllKinds,omitempty"`
	// ForbidKinds fails the case if any of these appear. A request to ADD something must never
	// come back proposing a deletion — that is not a lower score, it is the one outcome nobody
	// can undo with a second prompt.
	ForbidKinds   []string `json:"forbidKinds,omitempty"`
	ExpectEntity  string   `json:"expectEntity,omitempty"`
	ExpectNoDraft bool     `json:"expectNoDraft,omitempty"`
	// Probe turns this case from a SHAPE check into a USABILITY check (TRỤ 4): the draft's backbone
	// is applied through the real admin APIs and the server is then asked whether the result is an
	// app you can put data in. Only runs under -apply, because applying writes real schema.
	Probe *probeSpec `json:"probe,omitempty"`
	Note  string     `json:"note,omitempty"`
}

type goldenFile struct {
	Baseline string       `json:"baseline"` // human-recorded "27/30 @ 2026-07-25 cc/claude-sonnet-4-6"
	Cases    []goldenCase `json:"cases"`
}

type draftItem struct {
	Kind string `json:"kind"`
}

func main() {
	base := flag.String("base", "http://localhost:8090", "Nexus base URL")
	tenant := flag.String("tenant", "lab", "tenant slug (NEVER a customer tenant)")
	email := flag.String("email", "", "admin email")
	pass := flag.String("pass", "", "admin password")
	only := flag.String("only", "", "run a single case id")
	timeout := flag.Int("timeout", 240, "per-case timeout seconds")
	golden := flag.String("golden", "cmd/aieval/golden.json", "golden set path")
	// -apply is OFF by default and stays that way: it writes real entities into the target tenant.
	// The shape run is the cheap gate you can fire after any prompt change; the apply run is the
	// expensive one that answers "is the proposal actually buildable", and it must be asked for.
	apply := flag.Bool("apply", false, "cases with a `probe` block: APPLY the draft's backbone and verify the result is usable (writes schema, then cleans up)")
	// builder-bench (bench.go): grade a recorded run of nexus-web/scripts/builder-bench.mjs. No server.
	bench := flag.String("bench", "", "builder-bench dir (holds cases/): print the report tables of a recorded run")
	obs := flag.String("obs", "", "with -bench: the driver's observations dir (<out-dir>/obs)")
	flag.Parse()
	if *bench != "" {
		runBench(*bench, *obs)
		return
	}

	raw, err := os.ReadFile(*golden)
	if err != nil {
		fatal("read golden: %v", err)
	}
	var gf goldenFile
	if err := json.Unmarshal(raw, &gf); err != nil {
		fatal("parse golden: %v", err)
	}

	token := login(*base, *tenant, *email, *pass)
	client := &http.Client{Timeout: time.Duration(*timeout) * time.Second}

	// blocked counts cases the SERVER refused to run (429: budget/rate limit). They are not
	// content failures and must never be mixed into the score — a run that hits the tenant's daily
	// token budget half way through would otherwise read as "the assistant got worse", which is
	// the exact misreading a baseline number invites. Counted, reported and excluded.
	pass2, fail, blocked := 0, 0, 0
	for _, c := range gf.Cases {
		if *only != "" && c.ID != *only {
			continue
		}
		ok, detail := runCase(client, *base, token, c, *apply)
		status := "PASS"
		switch {
		case isBlocked(detail):
			// The server never called the model — nothing about the assistant was measured here.
			status = "BLOCK"
			blocked++
		case !ok:
			status = "FAIL"
			fail++
		default:
			pass2++
		}
		fmt.Printf("[%s] %-22s (%-11s) %s\n", status, c.ID, c.Group, detail)
	}
	scored := pass2 + fail
	fmt.Printf("\n== %d/%d pass", pass2, scored)
	if blocked > 0 {
		// Loud, and the score is explicitly disowned: a partial run is not a baseline. Recording
		// one would put a number in the golden file that measures the budget knob, not the model.
		fmt.Printf(" — ⚠️ %d case BỊ CHẶN (429 budget/rate-limit), KHÔNG tính điểm.\n", blocked)
		fmt.Printf("   Lượt chạy này KHÔNG dùng làm mốc: %d/%d case không hề gọi tới model.\n", blocked, blocked+scored)
		fmt.Printf("   Nâng AI_DAILY_TOKEN_BUDGET / AI_RATELIMIT_MAX_TURNS cho tenant eval rồi chạy lại.\n")
	} else {
		fmt.Printf(" (baseline: %s)\n", gf.Baseline)
	}
	if blocked > 0 {
		os.Exit(2) // distinct from a content failure: the measurement did not happen
	}
	if fail > 0 {
		os.Exit(1)
	}
}

func runCase(client *http.Client, base, token string, c goldenCase, apply bool) (bool, string) {
	body, _ := json.Marshal(map[string]any{"message": c.Prompt})
	req, _ := http.NewRequest("POST", base+"/api/v1/admin/ai/builder/chat", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Sprintf("transport: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Data struct {
			Reply string `json:"reply"`
			// Keep the raw draft blob: per-kind payload keys (workflow/report/layout/...) live
			// under kind-specific keys, and re-marshaling a partial struct would drop them —
			// the ExpectEntity substring check needs the FULL payload.
			Draft json.RawMessage `json:"draft"`
			Usage struct {
				TotalTokens int `json:"totalTokens"`
			} `json:"usage"`
		} `json:"data"`
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, fmt.Sprintf("decode (%d): %v", resp.StatusCode, err)
	}
	if resp.StatusCode != 200 {
		return false, fmt.Sprintf("HTTP %d %s", resp.StatusCode, out.Code)
	}

	ok, detail := gradeKinds(c, out.Data.Draft, out.Data.Reply)
	if !ok || c.ExpectNoDraft {
		return ok, detail
	}
	// The shape checks above all passed. Under -apply, that is no longer the whole question: a
	// well-shaped proposal can still build a table nobody can write a row into, and THAT is what
	// the customer means by "the app works".
	if apply && c.Probe != nil {
		ok, pd := applyAndProbe(client, base, token, out.Data.Draft, *c.Probe)
		return ok, detail + " · " + pd
	}
	if c.Probe != nil {
		return true, detail + " (probe bỏ qua — chạy với -apply để đo 'dùng được thật')"
	}
	return true, detail
}

// gradeKinds scores the SHAPE of one turn's draft: the kinds it holds against the case's expected,
// all-of and forbidden kinds, "no draft at all" for adversarial cases, and the pinned entity. The
// live run above and builder-bench's recorded turns (bench.go) share it, so the two cannot drift.
func gradeKinds(c goldenCase, draft json.RawMessage, reply string) (bool, string) {
	var items []draftItem
	if len(draft) > 0 {
		_ = json.Unmarshal(draft, &items)
	}
	kinds := make([]string, 0, len(items))
	for _, d := range items {
		kinds = append(kinds, d.Kind)
	}
	detail := fmt.Sprintf("drafts=%v", kinds)

	if c.ExpectNoDraft {
		if len(items) == 0 {
			return true, detail + " (refused as expected)"
		}
		return false, detail + " — adversarial case must NOT draft"
	}
	if len(items) == 0 {
		return false, detail + " — expected a draft, got none. reply: " + trim(reply, 120)
	}
	if len(c.ExpectKinds) > 0 {
		hit := false
		for _, k := range kinds {
			for _, want := range c.ExpectKinds {
				if k == want {
					hit = true
				}
			}
		}
		if !hit {
			return false, detail + fmt.Sprintf(" — want one of %v", c.ExpectKinds)
		}
	}
	if len(c.ExpectAllKinds) > 0 {
		have := map[string]bool{}
		for _, k := range kinds {
			have[k] = true
		}
		var missing []string
		for _, want := range c.ExpectAllKinds {
			if !have[want] {
				missing = append(missing, want)
			}
		}
		if len(missing) > 0 {
			return false, detail + fmt.Sprintf(" — missing %v (proposal is partial: the ask needs all of %v)", missing, c.ExpectAllKinds)
		}
	}
	for _, k := range kinds {
		for _, bad := range c.ForbidKinds {
			if k == bad {
				return false, detail + fmt.Sprintf(" — proposed FORBIDDEN kind %q for an additive request", bad)
			}
		}
	}
	if c.ExpectEntity != "" {
		if !strings.Contains(strings.ToLower(string(draft)), strings.ToLower(c.ExpectEntity)) {
			return false, detail + " — expected entity " + c.ExpectEntity
		}
	}
	return true, detail
}

// isBlocked reports whether a case failed because the SERVER refused to call the model (429
// budget/rate-limit) rather than because the assistant answered wrong. Matching on the detail
// string is crude but it is where runCase already puts the HTTP status and code, and the two codes
// are stable API contract (AI_BUDGET_EXCEEDED / AI_RATE_LIMITED).
func isBlocked(detail string) bool {
	return strings.Contains(detail, "AI_BUDGET_EXCEEDED") || strings.Contains(detail, "AI_RATE_LIMITED")
}

func login(base, tenant, email, pass string) string {
	body, _ := json.Marshal(map[string]string{"tenant": tenant, "email": email, "password": pass})
	resp, err := http.Post(base+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		fatal("login: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Data struct {
			AccessToken string `json:"accessToken"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Data.AccessToken == "" {
		fatal("login failed (%d)", resp.StatusCode)
	}
	return out.Data.AccessToken
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "aieval: "+f+"\n", a...)
	os.Exit(2)
}
