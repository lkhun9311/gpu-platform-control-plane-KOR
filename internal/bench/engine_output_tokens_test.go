package bench

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestEngineOutputTokensComeFromUsageNotFromFrameCount closes the most attractive way to be wrong.
//
// OutputTokens counts SSE frames that carried content, one per frame. That equals the token count only
// while the server emits one token per frame, and nothing in the protocol promises it. A server that
// batched two tokens into a frame would halve every throughput figure, every tenant share and every
// inter-token time this harness reports, while the GPU did exactly the same work -- and every number would
// still look plausible, which is the failure this project exists to avoid.
//
// The request already asks for usage. The engine's own count was arriving and being thrown away.
func TestEngineOutputTokensComeFromUsageNotFromFrameCount(t *testing.T) {
	// Three content frames carrying six tokens between them, then the usage chunk vLLM sends last.
	body := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"a b\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"c d\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"e f\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":6}}\n\n" +
		"data: [DONE]\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	s := NewHTTPSender(srv.URL, "m", nil, 5*time.Second, SenderConn{MaxIdleConnsPerHost: 2})
	res := s.Send(context.Background(), TraceRow{Index: 0, Tenant: "premium-1", PromptLenChars: 200}, time.Now().UnixNano())

	if res.OutputTokens != 3 {
		t.Errorf("frame tally = %d, want 3: the fixture sends three content frames", res.OutputTokens)
	}
	if res.EngineOutputTokens != 6 {
		t.Errorf("engine output tokens = %d, want 6: the usage chunk says six were generated",
			res.EngineOutputTokens)
	}
	if res.PromptTokens != 50 {
		t.Errorf("prompt tokens = %d, want 50", res.PromptTokens)
	}
}
