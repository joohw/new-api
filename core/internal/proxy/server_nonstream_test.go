package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/clovapi/switcher/internal/apistyle"
	"github.com/clovapi/switcher/internal/profile"
	"github.com/clovapi/switcher/internal/provider"
)

// Codex subscriptions require an upstream stream even when the client asks for
// JSON. Exercise the public provider URL so its default protocol and stream mode
// cannot accidentally be overridden by the upstream's requirements.
func TestCodexSubscriptionNonStreamJSON(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_test","object":"response","model":"gpt-5.4","status":"in_progress","output":[]}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hello "}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"world"}`,
		``,
		`event: response.output_text.done`,
		`data: {"type":"response.output_text.done","text":"hello world"}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_test","type":"function_call","call_id":"call_test","name":"lookup","arguments":""}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_test","delta":"{\"city\":"}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_test","delta":"\"Paris\"}"}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"id":"fc_test","type":"function_call","call_id":"call_test","name":"lookup","arguments":"{\"city\":\"Paris\"}"}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_test","object":"response","status":"completed","model":"gpt-5.4","output":[],"usage":{"input_tokens":12,"output_tokens":3,"total_tokens":15,"input_tokens_details":{"cached_tokens":8},"output_tokens_details":{"reasoning_tokens":2}}}}`,
		``,
		``,
	}, "\n")
	for _, ingress := range []struct {
		name    string
		path    string
		request string
	}{
		{"responses", "/codex/v1/responses", `{"model":"gpt-5.4","input":"ping"`},
		{"chat", "/codex/v1/chat/completions", `{"model":"gpt-5.4","messages":[{"role":"user","content":"ping"}]`},
	} {
		for _, stream := range []struct {
			name  string
			field string
		}{
			{"false", `,"stream":false`},
			{"omitted", ""},
		} {
			for _, wire := range []string{"sse", "no-content-type", "gzip"} {
				t.Run(ingress.name+"/"+stream.name+"/"+wire, func(t *testing.T) {
					body := []byte(sse)
					if wire == "gzip" {
						var compressed bytes.Buffer
						zw := gzip.NewWriter(&compressed)
						if _, err := zw.Write(body); err != nil {
							t.Fatal(err)
						}
						if err := zw.Close(); err != nil {
							t.Fatal(err)
						}
						body = compressed.Bytes()
					}
					up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != "/codex/responses" {
							t.Errorf("upstream path = %q", r.URL.Path)
						}
						var request map[string]any
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Errorf("decode upstream request: %v", err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						if request["stream"] != true {
							t.Errorf("Codex upstream stream = %v, want true", request["stream"])
						}
						if wire == "no-content-type" {
							w.Header()["Content-Type"] = nil
						} else {
							w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
						}
						if wire == "gzip" {
							w.Header().Set("Content-Encoding", "gzip")
						}
						w.Header().Set("Content-Length", strconv.Itoa(len(body)))
						w.Header().Set("X-Request-Id", "upstream-test")
						_, _ = w.Write(body)
					}))
					defer up.Close()

					store := &profile.Store{
						Version: profile.StoreVersion,
						List: []profile.Profile{{
							Name:                   provider.CodexVendorName,
							Kind:                   "subscription",
							SubscriptionProviderID: provider.CodexProviderID,
							APIStyle:               apistyle.OpenAIResponses,
							BaseURL:                up.URL,
							APIKey:                 "test-token",
							AccountID:              "test-account",
							Model:                  "gpt-5.4",
							Models: []profile.Model{{
								ID: "gpt-5.4", Model: "gpt-5.4", APIStyle: apistyle.OpenAIResponses,
							}},
						}},
					}
					server := newTestServer(profile.ProxyConfig{Host: "127.0.0.1", Port: 0})
					server.ProfileLoader = func() (*profile.Store, error) { return store, nil }
					ts := httptest.NewServer(server.Server.Handler)
					defer ts.Close()

					resp, err := http.Post(ts.URL+ingress.path, "application/json", strings.NewReader(ingress.request+stream.field+"}"))
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					raw, err := io.ReadAll(resp.Body)
					if err != nil {
						t.Fatal(err)
					}
					if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
						t.Fatalf("expected JSON success, status=%d headers=%v body=%s", resp.StatusCode, resp.Header, raw)
					}
					if resp.Header.Get("Content-Encoding") != "" || resp.ContentLength != int64(len(raw)) {
						t.Fatalf("expected uncompressed JSON framing, headers=%v body length=%d", resp.Header, len(raw))
					}
					if resp.Header.Get("X-Request-Id") != "upstream-test" {
						t.Fatalf("upstream request ID lost: %v", resp.Header)
					}
					var result struct {
						Object string `json:"object"`
						Status string `json:"status"`
						Output []struct {
							Type      string `json:"type"`
							CallID    string `json:"call_id"`
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
							Content   []struct {
								Text string `json:"text"`
							} `json:"content"`
						} `json:"output"`
						Choices []struct {
							FinishReason string `json:"finish_reason"`
							Message      struct {
								Content   string `json:"content"`
								ToolCalls []struct {
									ID       string `json:"id"`
									Function struct {
										Name      string `json:"name"`
										Arguments string `json:"arguments"`
									} `json:"function"`
								} `json:"tool_calls"`
							} `json:"message"`
						} `json:"choices"`
						Usage map[string]any `json:"usage"`
					}
					if err := json.Unmarshal(raw, &result); err != nil {
						t.Fatalf("invalid non-stream JSON: %v, body=%s", err, raw)
					}
					inKey, outKey := "input_tokens", "output_tokens"
					if ingress.name == "responses" {
						if result.Object != "response" || result.Status != "completed" || len(result.Output) != 2 ||
							len(result.Output[0].Content) != 1 || result.Output[0].Content[0].Text != "hello world" ||
							result.Output[1].Type != "function_call" || result.Output[1].CallID != "call_test" ||
							result.Output[1].Name != "lookup" || result.Output[1].Arguments != `{"city":"Paris"}` {
							t.Fatalf("Responses text/tool aggregation failed: %s", raw)
						}
					} else {
						inKey, outKey = "prompt_tokens", "completion_tokens"
						if result.Object != "chat.completion" || len(result.Choices) != 1 ||
							result.Choices[0].FinishReason != "tool_calls" || result.Choices[0].Message.Content != "hello world" ||
							len(result.Choices[0].Message.ToolCalls) != 1 || result.Choices[0].Message.ToolCalls[0].ID != "call_test" ||
							result.Choices[0].Message.ToolCalls[0].Function.Name != "lookup" ||
							result.Choices[0].Message.ToolCalls[0].Function.Arguments != `{"city":"Paris"}` {
							t.Fatalf("Chat text/tool aggregation failed: %s", raw)
						}
					}
					inDetails, _ := result.Usage[inKey+"_details"].(map[string]any)
					outDetails, _ := result.Usage[outKey+"_details"].(map[string]any)
					if result.Usage[inKey] != float64(12) || result.Usage[outKey] != float64(3) || result.Usage["total_tokens"] != float64(15) ||
						inDetails["cached_tokens"] != float64(8) || outDetails["reasoning_tokens"] != float64(2) {
						t.Fatalf("token usage lost during aggregation: %s", raw)
					}
				})
			}
		}
	}
}
