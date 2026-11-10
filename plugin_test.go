package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type fakeRouter struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
	status   int
	reply    string
	stream   string
}

func (f *fakeRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, r.Clone(r.Context()))
	f.bodies = append(f.bodies, body)
	f.mu.Unlock()
	switch r.URL.Path {
	case "/v1/models":
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"ditto-dev","owned_by":"ditto"},{"id":"fast","owned_by":"ditto"},{"id":"anthropic/claude-opus-4.5","owned_by":"anthropic"}]}`)
		return
	case "/v1/credits":
		_, _ = io.WriteString(w, `{"data":{"currency":"usd","billing_mode":"byok","balance":12.5,"endpoint":{"limit":20,"limit_remaining":5,"limit_reset":"monthly","usage":15},"limit":null,"usage":3}}`)
		return
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.reply)
		return
	}
	if f.stream != "" {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, f.stream)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Request-Id", "req_1")
	w.Header().Set("Set-Cookie", "must-not-leak=1")
	_, _ = io.WriteString(w, f.reply)
}

func newRouter(t *testing.T) (*fakeRouter, credential, []byte) {
	t.Helper()
	f := &fakeRouter{reply: `{"id":"msg_1"}`}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("ditto_inf_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := credential{Type: "ditto", Endpoint: "ditto-dev", APIKeyFile: keyFile, BaseURL: srv.URL}
	raw, _ := json.Marshal(c)
	return f, c, raw
}

func decodeOK[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %+v", env.Error)
	}
	var out T
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return out
}

func decodeErr(t *testing.T, raw []byte) envelopeError {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("expected error envelope, got %s", raw)
	}
	return *env.Error
}

func executorCall(t *testing.T, format string, stream bool, storage []byte) []byte {
	t.Helper()
	req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthProvider: provider,
		Model:        "claude-opus-5-5",
		Format:       format,
		Stream:       stream,
		Payload:      []byte(`{"model":"claude-opus-5-5","messages":[]}`),
		StorageJSON:  storage,
		Headers: http.Header{
			"Authorization":            {"Bearer cliproxy-access-key"},
			"X-Api-Key":                {"cliproxy-access-key"},
			"Anthropic-Version":        {"2023-06-01"},
			"Anthropic-Beta":           {"interleaved-thinking-2025-05-14"},
			"X-Claude-Code-Session-Id": {"sess-1"},
			"Cookie":                   {"secret=1"},
		},
	}}
	if stream {
		req.StreamID = "stream-1"
	}
	raw, _ := json.Marshal(req)
	return raw
}

func TestRegistrationDeclaresPassthroughWires(t *testing.T) {
	reg := decodeOK[registration](t, must(handleMethod(pluginabi.MethodPluginRegister, nil)))
	if !reg.Capabilities.Executor || !reg.Capabilities.AuthProvider || !reg.Capabilities.ModelProvider || !reg.Capabilities.QuotaProvider {
		t.Fatalf("missing capability: %+v", reg.Capabilities)
	}
	for _, want := range []string{"claude", "openai-response", "codex", "openai"} {
		if !contains(reg.Capabilities.ExecutorInputFormats, want) || !contains(reg.Capabilities.ExecutorOutputFormats, want) {
			t.Fatalf("format %q not declared both ways", want)
		}
		if _, err := upstreamPath(want); err != nil {
			t.Fatalf("declared format %q has no Router path: %v", want, err)
		}
	}
}

func TestConfigureAndPriority(t *testing.T) {
	t.Cleanup(func() { current.Store(&config{}) })
	cfgYAML, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte("credential-priority: -7\nfallback-models: [\"claude-*\"]\n")})
	must(handleMethod(pluginabi.MethodPluginReconfigure, cfgYAML))
	if got := cfg().priority(); got != -7 {
		t.Fatalf("priority = %d, want -7", got)
	}
	explicit := 3
	if got := (credential{Priority: &explicit}).priority(); got != 3 {
		t.Fatalf("credential priority = %d, want 3", got)
	}
}

func TestParseAuthKeepsKeyOutOfMetadata(t *testing.T) {
	raw := []byte(`{"type":"ditto","endpoint":"ditto-dev","api_key":"ditto_inf_secret"}`)
	req, _ := json.Marshal(pluginapi.AuthParseRequest{Provider: "ditto", FileName: "ditto-dev.json", RawJSON: raw})
	resp := decodeOK[pluginapi.AuthParseResponse](t, must(parseAuth(req)))
	if !resp.Handled || resp.Auth.Provider != provider || resp.Auth.ID != "ditto-dev" {
		t.Fatalf("unexpected parse: %+v", resp)
	}
	if resp.Auth.Attributes["priority"] != "-100" {
		t.Fatalf("default priority attribute = %q, want -100", resp.Auth.Attributes["priority"])
	}
	if _, ok := resp.Auth.Attributes["base_url"]; ok {
		t.Fatal("base_url attribute routes the auth to CLIProxy's OpenAI-compat executor, bypassing the plugin")
	}
	meta, _ := json.Marshal(resp.Auth.Metadata)
	attrs, _ := json.Marshal(resp.Auth.Attributes)
	if strings.Contains(string(meta)+string(attrs)+resp.Auth.Label, "ditto_inf_secret") {
		t.Fatal("key leaked into displayed auth fields")
	}

	other, _ := json.Marshal(pluginapi.AuthParseRequest{Provider: "claude", RawJSON: []byte(`{"type":"claude"}`)})
	if decodeOK[pluginapi.AuthParseResponse](t, must(parseAuth(other))).Handled {
		t.Fatal("claimed a non-Ditto credential")
	}
	relative, _ := json.Marshal(pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"ditto","api_key_file":"key.txt"}`)})
	if e := decodeErr(t, must(parseAuth(relative))); e.Code != "invalid_credential" {
		t.Fatalf("relative key file accepted: %+v", e)
	}
}

func TestExecuteForwardsBodyAndOnlyAllowedHeaders(t *testing.T) {
	f, _, storage := newRouter(t)
	resp := decodeOK[pluginapi.ExecutorResponse](t, must(execute(executorCall(t, "claude", false, storage))))
	if string(resp.Payload) != `{"id":"msg_1"}` {
		t.Fatalf("payload = %s", resp.Payload)
	}
	if resp.Headers.Get("Set-Cookie") != "" || resp.Headers.Get("Request-Id") != "req_1" {
		t.Fatalf("response headers not filtered: %v", resp.Headers)
	}
	r := f.requests[0]
	if r.URL.Path != "/v1/messages" {
		t.Fatalf("path = %s", r.URL.Path)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer ditto_inf_test" {
		t.Fatalf("Authorization = %q, want the Ditto key", got)
	}
	if r.Header.Get("X-Api-Key") != "" || r.Header.Get("Cookie") != "" {
		t.Fatal("client credentials reached Ditto")
	}
	for _, h := range []string{"Anthropic-Version", "Anthropic-Beta", "X-Claude-Code-Session-Id"} {
		if r.Header.Get(h) == "" {
			t.Fatalf("%s not forwarded", h)
		}
	}
	if r.Header.Get(viaHeader) != viaValue {
		t.Fatalf("%s = %q", viaHeader, r.Header.Get(viaHeader))
	}
	if string(f.bodies[0]) != `{"model":"claude-opus-5-5","messages":[]}` {
		t.Fatalf("body rewritten: %s", f.bodies[0])
	}
}

func TestWireFormatRoutes(t *testing.T) {
	f, _, storage := newRouter(t)
	for format, want := range map[string]string{"openai-response": "/v1/responses", "codex": "/v1/responses", "openai": "/v1/chat/completions"} {
		decodeOK[pluginapi.ExecutorResponse](t, must(execute(executorCall(t, format, false, storage))))
		if got := f.requests[len(f.requests)-1].URL.Path; got != want {
			t.Fatalf("%s -> %s, want %s", format, got, want)
		}
	}
	if e := decodeErr(t, must(execute(executorCall(t, "gemini", false, storage)))); e.Code != "unsupported_format" {
		t.Fatalf("gemini accepted: %+v", e)
	}
}

func TestUpstreamRefusalsMapToFailoverStatuses(t *testing.T) {
	f, _, storage := newRouter(t)
	for status, want := range map[int]int{402: 403, 401: 401, 429: 429, 503: 503, 400: 400} {
		f.status, f.reply = status, `{"error":"nope"}`
		for _, stream := range []bool{false, true} {
			var raw []byte
			if stream {
				raw = must(executeStream(executorCall(t, "claude", true, storage)))
			} else {
				raw = must(execute(executorCall(t, "claude", false, storage)))
			}
			if e := decodeErr(t, raw); e.HTTPStatus != want {
				t.Fatalf("status %d stream=%v -> http_status %d, want %d", status, stream, e.HTTPStatus, want)
			}
		}
	}
}

type emitted struct {
	mu     sync.Mutex
	chunks []string
	closed chan string
}

func captureHost(t *testing.T) *emitted {
	t.Helper()
	e := &emitted{closed: make(chan string, 1)}
	prev := callHost
	t.Cleanup(func() { callHost = prev })
	callHost = func(method string, payload any) (json.RawMessage, error) {
		switch p := payload.(type) {
		case streamEmit:
			e.mu.Lock()
			e.chunks = append(e.chunks, string(p.Payload))
			e.mu.Unlock()
		case streamClose:
			e.closed <- p.Error
		default:
			t.Errorf("unexpected host call %s", method)
		}
		return json.RawMessage(`{}`), nil
	}
	return e
}

func TestStreamEmitsWholeSSEEvents(t *testing.T) {
	f, _, storage := newRouter(t)
	f.stream = "event: message_start\ndata: {\"a\":1}\n\nevent: content_block_delta\ndata: {\"b\":2}\n\nevent: message_stop\ndata: {}\n"
	host := captureHost(t)
	start := decodeOK[streamStart](t, must(executeStream(executorCall(t, "claude", true, storage))))
	if start.Headers.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("headers = %v", start.Headers)
	}
	if msg := <-host.closed; msg != "" {
		t.Fatalf("stream closed with error %q", msg)
	}
	want := []string{
		"event: message_start\ndata: {\"a\":1}\n\n",
		"event: content_block_delta\ndata: {\"b\":2}\n\n",
		"event: message_stop\ndata: {}\n",
	}
	if strings.Join(host.chunks, "|") != strings.Join(want, "|") {
		t.Fatalf("chunks = %q", host.chunks)
	}
	if f.requests[0].Header.Get("Accept") != "text/event-stream" {
		t.Fatal("stream request did not ask for SSE")
	}
}

func TestModelSelection(t *testing.T) {
	mirrored := []string{"claude-opus-5-5", "gpt-6.1-sol", "muse-spark-1.3", "claude-opus-5-5"}
	router := []routerModel{{ID: "ditto-dev", OwnedBy: "ditto"}, {ID: "anthropic/claude-opus-4.5", OwnedBy: "anthropic"}}
	ids := func(ms []pluginapi.ModelInfo) string {
		var out []string
		for _, m := range ms {
			out = append(out, m.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids(selectModels(config{}, mirrored, router)); got != "claude-opus-5-5,ditto-dev,gpt-6.1-sol" {
		t.Fatalf("default = %s", got)
	}
	narrow := config{FallbackModels: []string{"claude-*"}, ExtraModels: []string{"kimi-k2"}}
	if got := ids(selectModels(narrow, mirrored, router)); got != "claude-opus-5-5,ditto-dev,kimi-k2" {
		t.Fatalf("narrow = %s", got)
	}
	if got := ids(selectModels(config{FallbackModels: []string{}, ExposeCatalog: true}, mirrored, router)); got != "anthropic/claude-opus-4.5,ditto-dev" {
		t.Fatalf("catalog = %s", got)
	}
}

func TestModelsForAuthMirrorsOtherCredentials(t *testing.T) {
	_, _, storage := newRouter(t)
	prev := callHost
	t.Cleanup(func() { callHost = prev })
	callHost = func(method string, _ any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostAuthModels {
			t.Fatalf("unexpected host call %s", method)
		}
		return json.Marshal(hostAuthModels{Credentials: []pluginapi.HostAuthModels{
			{Provider: "claude", Models: []string{"claude-opus-5-5", "claude-sonnet-5-5"}},
			{Provider: "codex", Models: []string{"gpt-6.1-sol"}},
			{Provider: "ditto", Models: []string{"stale-self"}},
		}})
	}
	req, _ := json.Marshal(authModelRequest{AuthModelRequest: pluginapi.AuthModelRequest{StorageJSON: storage}})
	resp := decodeOK[pluginapi.ModelResponse](t, must(modelsForAuth(req)))
	var got []string
	for _, m := range resp.Models {
		got = append(got, m.ID)
	}
	if strings.Join(got, ",") != "claude-opus-5-5,claude-sonnet-5-5,ditto-dev,fast,gpt-6.1-sol" {
		t.Fatalf("models = %v", got)
	}
}

func TestQuotaFromCredits(t *testing.T) {
	_, _, storage := newRouter(t)
	req, _ := json.Marshal(quotaRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{StorageJSON: storage}})
	q := decodeOK[pluginapi.QuotaFetchResponse](t, must(fetchQuota(req)))
	if q.Summary[0].Value != 12.5 || q.Summary[0].Currency != "USD" {
		t.Fatalf("summary = %+v", q.Summary)
	}
	if len(q.Groups) != 1 || len(q.Groups[0].Buckets) != 1 || q.Groups[0].Buckets[0].RemainingFraction != 0.25 {
		t.Fatalf("groups = %+v", q.Groups)
	}
}

func must(raw []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return raw
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
