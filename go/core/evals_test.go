package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

// LLM-as-judge eval suite for the Go core — the Go sibling of the Rust, C#,
// Python and TS suites. Every scenario comes from the SHARED corpus at
// spec/evals/scenarios.json; nothing is defined here. See that file's $comment
// for why (the scenarios used to be hand-duplicated in five places and forked).
//
// Two tests live here:
//
//   - TestEvalCorpusMatchesSpec — OFFLINE, always runs. The drift guard.
//   - TestEvalAggregateMeanClearsThreshold — gated on SMOOTH_AGENT_E2E=1 +
//     SMOOAI_GATEWAY_KEY, so it's a no-op (never fails) without credentials.

const gatewayURL = "https://llm.smoo.ai/v1"

const defaultEvalModel = "claude-haiku-4-5"

// minScenarios is a RATCHET, not a duplicate of the corpus. Comparing the loaded
// set against the file catches a language that subsets or mis-parses it, but not
// a scenario deleted from the file itself — both sides shrink together and every
// language stays green. This floor is what makes a deletion loud. Raise it when
// you add scenarios; lowering it should require saying why in the PR.
const minScenarios = 15

// evalDoc is one knowledge-base document in the shared corpus.
type evalDoc struct {
	Content string `json:"content"`
	Source  string `json:"source"`
}

// evalScenario is one scenario as it appears in spec/evals/scenarios.json.
type evalScenario struct {
	ID          string   `json:"id"`
	Tier        string   `json:"tier"`
	Intent      string   `json:"intent"`
	KbDocs      []string `json:"kb_docs"`
	UserTurns   []string `json:"user_turns"`
	GroundTruth string   `json:"ground_truth"`
	Rubric      string   `json:"rubric"`
}

type evalCorpus struct {
	SupportPrompt     string             `json:"support_prompt"`
	JudgeSystemPrompt string             `json:"judge_system_prompt"`
	AggregateMean     float64            `json:"aggregate_mean_threshold"`
	HardAggregateMean float64            `json:"hard_aggregate_mean_threshold"`
	Docs              map[string]evalDoc `json:"docs"`
	Scenarios         []evalScenario     `json:"scenarios"`
}

// evalCorpusPath locates the shared corpus relative to this package (go/core),
// mirroring how go/protocol's conformance test reaches spec/.
func evalCorpusPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "spec", "evals", "scenarios.json"))
	if err != nil {
		t.Fatalf("resolve corpus path: %v", err)
	}
	return p
}

func loadEvalCorpus(t *testing.T) evalCorpus {
	t.Helper()
	raw, err := os.ReadFile(evalCorpusPath(t))
	if err != nil {
		t.Fatalf("read shared eval corpus: %v", err)
	}
	var c evalCorpus
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("parse shared eval corpus: %v", err)
	}
	return c
}

// docsFor resolves a scenario's kb_docs keys into (content, source) pairs.
func (c evalCorpus) docsFor(t *testing.T, sc evalScenario) [][2]string {
	t.Helper()
	out := make([][2]string, 0, len(sc.KbDocs))
	for _, key := range sc.KbDocs {
		d, ok := c.Docs[key]
		if !ok {
			t.Fatalf("scenario %s references unknown doc %q", sc.ID, key)
		}
		out = append(out, [2]string{d.Content, d.Source})
	}
	return out
}

// TestEvalCorpusMatchesSpec is the drift guard: it runs OFFLINE in normal CI and
// asserts the scenario set this suite would execute is exactly the scenario set
// in spec/evals/scenarios.json — same count, same ids. A language that subsets,
// filters, or fails to parse part of the corpus goes red here instead of quietly
// running a forked suite (which is how the .NET corpus drifted).
func TestEvalCorpusMatchesSpec(t *testing.T) {
	corpus := loadEvalCorpus(t)

	// The raw ids in the file, independent of the typed decode above.
	raw, err := os.ReadFile(evalCorpusPath(t))
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var rawFile struct {
		Scenarios []map[string]json.RawMessage `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &rawFile); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	var wantIDs []string
	for _, s := range rawFile.Scenarios {
		var id string
		if err := json.Unmarshal(s["id"], &id); err != nil {
			t.Fatalf("scenario id: %v", err)
		}
		wantIDs = append(wantIDs, id)
	}

	var gotIDs []string
	for _, sc := range corpus.Scenarios {
		gotIDs = append(gotIDs, sc.ID)
	}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("corpus count drift: loaded %d scenarios, spec has %d", len(gotIDs), len(wantIDs))
	}
	sort.Strings(gotIDs)
	sort.Strings(wantIDs)
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("corpus id drift: loaded %v, spec has %v", gotIDs, wantIDs)
		}
	}

	// Every scenario must be runnable: resolvable docs and a non-empty prompt,
	// ground truth and rubric. Catches a malformed corpus before a nightly burns
	// gateway spend discovering it.
	core := 0
	for _, sc := range corpus.Scenarios {
		if sc.Tier == "core" {
			core++
		}
		if len(sc.UserTurns) == 0 || sc.GroundTruth == "" || sc.Rubric == "" {
			t.Errorf("scenario %s is incomplete (turns/ground truth/rubric)", sc.ID)
		}
		for _, key := range sc.KbDocs {
			if _, ok := corpus.Docs[key]; !ok {
				t.Errorf("scenario %s references unknown doc %q", sc.ID, key)
			}
		}
	}
	if core == 0 {
		t.Error("corpus has no core-tier scenarios")
	}
	if corpus.SupportPrompt == "" || corpus.JudgeSystemPrompt == "" {
		t.Error("corpus is missing the support/judge prompts")
	}
	if len(corpus.Scenarios) < minScenarios {
		t.Errorf("corpus shrank: %d scenarios < ratchet floor %d — a scenario was deleted from spec/evals/scenarios.json", len(corpus.Scenarios), minScenarios)
	}
	t.Logf("[go-eval] corpus in sync: %d scenarios (%d core)", len(corpus.Scenarios), core)
}

var jsonObjRe = regexp.MustCompile(`(?s)\{.*\}`)

func parseVerdict(text string) (int, string, error) {
	m := jsonObjRe.FindString(text)
	if m == "" {
		return 0, "", fmt.Errorf("judge did not return JSON: %q", text)
	}
	var v struct {
		Score     int    `json:"score"`
		Reasoning string `json:"reasoning"`
	}
	if err := json.Unmarshal([]byte(m), &v); err != nil {
		return 0, "", err
	}
	return v.Score, v.Reasoning, nil
}

func TestEvalAggregateMeanClearsThreshold(t *testing.T) {
	if os.Getenv("SMOOTH_AGENT_E2E") != "1" {
		t.Skip("SMOOTH_AGENT_E2E != \"1\" — skipping live-gateway eval suite.")
	}
	apiKey := os.Getenv("SMOOAI_GATEWAY_KEY")
	if apiKey == "" {
		t.Skip("SMOOAI_GATEWAY_KEY unset/empty — skipping live-gateway eval suite.")
	}
	judgeModel := os.Getenv("SMOOTH_AGENT_JUDGE_MODEL")
	if judgeModel == "" {
		judgeModel = defaultEvalModel
	}

	corpus := loadEvalCorpus(t)
	ctx := context.Background()
	client := NewGatewayClient(gatewayURL, apiKey)

	// Tiers are scored separately: core must clear the real bar, hard sits on a
	// lenient floor so one adversarial miss is an improvement target, not a red CI.
	totals := map[string]int{}
	counts := map[string]int{}

	for _, sc := range corpus.Scenarios {
		kb := &InMemoryKnowledge{}
		for _, d := range corpus.docsFor(t, sc) {
			kb.Ingest(d[0], d[1])
		}
		agent := NewSmoothAgent(client, AgentOptions{Instructions: corpus.SupportPrompt, Model: defaultEvalModel, Knowledge: kb})

		var history []ChatMessage
		reply := ""
		for _, turn := range sc.UserTurns {
			res, err := agent.Run(ctx, turn, history)
			if err != nil {
				t.Fatalf("scenario %s: agent run: %v", sc.ID, err)
			}
			reply = res.Text
			history = append(history, ChatMessage{Role: "user", Content: turn}, ChatMessage{Role: "assistant", Content: reply})
		}

		judgeUser := fmt.Sprintf("GROUND TRUTH:\n%s\n\nRUBRIC:\n%s\n\nAGENT REPLY:\n%s\n\nScore it now as JSON.", sc.GroundTruth, sc.Rubric, reply)
		jresp, err := client.Chat(ctx, ChatRequest{
			Model:     judgeModel,
			Messages:  []ChatMessage{{Role: "system", Content: corpus.JudgeSystemPrompt}, {Role: "user", Content: judgeUser}},
			MaxTokens: 300,
		})
		if err != nil {
			t.Fatalf("scenario %s: judge: %v", sc.ID, err)
		}
		score, reasoning, err := parseVerdict(jresp.Content)
		if err != nil {
			t.Fatalf("scenario %s: parse verdict: %v", sc.ID, err)
		}
		totals[sc.Tier] += score
		counts[sc.Tier]++
		t.Logf("[go-eval] (%s) %s: %d/5 — %s", sc.Tier, sc.ID, score, reasoning)
	}

	for tier, threshold := range map[string]float64{"core": corpus.AggregateMean, "hard": corpus.HardAggregateMean} {
		if counts[tier] == 0 {
			continue
		}
		mean := float64(totals[tier]) / float64(counts[tier])
		t.Logf("[go-eval] %s aggregate mean %.2f/5 across %d scenarios", tier, mean, counts[tier])
		if mean < threshold {
			t.Errorf("%s eval aggregate mean %.2f < %.1f", tier, mean, threshold)
		}
	}
}
