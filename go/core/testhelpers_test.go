package core

import "context"

// fakeClient is a minimal scripted ChatClient shared by tests that predate the
// reusable MockLlmProvider. New tests should prefer MockLlmProvider (see
// llm_provider.go / llm_provider_test.go); this remains so the existing per-feature
// suites (cast, checkpoint, cost, memory, rerank, thread, vector, human_gate,
// subagent) keep compiling unchanged.
type fakeClient struct {
	scripted []ChatResponse
	calls    []ChatRequest
}

func (f *fakeClient) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	f.calls = append(f.calls, req)
	resp := f.scripted[0]
	f.scripted = f.scripted[1:]
	return resp, nil
}
