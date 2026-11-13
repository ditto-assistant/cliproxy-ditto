package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const testClassifierModel = "ditto-auto-mode-classifier"

// classifierBody is shaped like Claude Code 2.1.323's stage-one check for a
// non-Claude classifier model: a cached policy block, one transcript message,
// the legacy <block> stop sequence and no tools.
func classifierBody(model string) []byte {
	return []byte(`{"model":"` + model + `","max_tokens":2112,"stop_sequences":["</block>"],` +
		`"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.323;"},{"type":"text","text":"You are a security monitor for autonomous AI coding agents. Never allow rm -rf /.","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"<transcript>\nUser: run the probe & report\n</transcript>\n"},{"type":"text","text":"Bash: touch /tmp/marker"}]}],` +
		`"metadata":{"user_id":"u"}}`)
}

func classifierCredential(t *testing.T, mutate func(*credential)) (*fakeRouter, credential, []byte) {
	t.Helper()
	f, c, _ := newRouter(t)
	c.Endpoint = "auto-mode-classifier"
	c.ClassifierModel = testClassifierModel
	if mutate != nil {
		mutate(&c)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return f, c, raw
}

func classifierCall(t *testing.T, storage, payload []byte, mutate func(*executorRequest)) []byte {
	t.Helper()
	req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthProvider: provider,
		Model:        testClassifierModel,
		Format:       "claude",
		Payload:      payload,
		StorageJSON:  storage,
		Headers: http.Header{
			"Authorization":               {"Bearer cliproxy-access-key"},
			"Anthropic-Version":           {"2023-06-01"},
			"X-Claude-Code-Request-Class": {"auxiliary"},
			"X-Claude-Code-Session-Id":    {"sess-1"},
		},
	}}
	if mutate != nil {
		mutate(&req)
	}
	raw, _ := json.Marshal(req)
	out, err := execute(raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertUnfit(t *testing.T, out []byte, reason string) {
	t.Helper()
	e := decodeErr(t, out)
	if e.HTTPStatus != routeUnfitStatus || !strings.Contains(e.Message, routeUnfitMarker) {
		t.Fatalf("want a route refusal (%d, %s), got %+v", routeUnfitStatus, routeUnfitMarker, e)
	}
	if reason != "" && !strings.Contains(e.Message, reason) {
		t.Fatalf("refusal %q does not say %q", e.Message, reason)
	}
}

func TestClassifierCredentialValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    credential
		want string
	}{
		{"claude id", credential{Type: "ditto", Endpoint: "e", APIKey: "k", ClassifierModel: "claude-sonnet-5"}, "dedicated"},
		{"bad id", credential{Type: "ditto", Endpoint: "e", APIKey: "k", ClassifierModel: "Ditto Classifier"}, "lowercase"},
		{"no endpoint", credential{Type: "ditto", APIKey: "k", ClassifierModel: testClassifierModel}, "endpoint"},
		{"negative bound", credential{Type: "ditto", Endpoint: "e", APIKey: "k", ClassifierModel: testClassifierModel, MaxInputTokens: -1}, "negative"},
		{"bound without route", credential{Type: "ditto", Endpoint: "e", APIKey: "k", MaxInputTokens: 10}, "only to a credential with classifier_model"},
	} {
		raw, _ := json.Marshal(tc.c)
		if _, err := decodeCredential(raw); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

func TestClassifierCredentialRegistersOnlyItsModelAndAContinueRule(t *testing.T) {
	f, c, raw := classifierCredential(t, nil)
	callHost = func(string, any) (json.RawMessage, error) {
		return json.RawMessage(`{"credentials":[{"provider":"claude","models":["claude-opus-5-5","claude-sonnet-5"]}]}`), nil
	}
	t.Cleanup(func() { callHost = nil })
	req, _ := json.Marshal(authModelRequest{AuthModelRequest: pluginapi.AuthModelRequest{StorageJSON: raw}})
	out, err := modelsForAuth(req)
	if err != nil {
		t.Fatal(err)
	}
	models := decodeOK[pluginapi.ModelResponse](t, out)
	if len(models.Models) != 1 || models.Models[0].ID != testClassifierModel {
		t.Fatalf("a classifier credential must register exactly %s, got %+v", testClassifierModel, models.Models)
	}
	if len(f.requests) != 0 {
		t.Fatalf("registration should not contact the Router, got %d requests", len(f.requests))
	}

	data := c.authData("id", "file.json", raw, firstRefresh)
	rules, ok := data.Metadata["request_scoped_errors"].([]map[string]any)
	if !ok || len(rules) != 1 || rules[0]["action"] != "continue" || rules[0]["status"] != routeUnfitStatus {
		t.Fatalf("classifier auth needs one continue rule for its refusals, got %#v", data.Metadata["request_scoped_errors"])
	}
	if strings.Contains(string(mustJSON(t, data.Metadata)), "ditto_inf_test") {
		t.Fatal("metadata leaked the key")
	}
	_, _, plainRaw := newRouter(t)
	var ordinary credential
	_ = json.Unmarshal(plainRaw, &ordinary)
	if _, has := ordinary.authData("id", "f", plainRaw, firstRefresh).Metadata["request_scoped_errors"]; has {
		t.Fatal("an ordinary credential must keep CLIProxy's default error handling")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestClassifierForwardsTheCheckWholeToTheEndpoint(t *testing.T) {
	f, _, raw := classifierCredential(t, nil)
	f.reply = `{"id":"msg_glm","type":"message","role":"assistant","content":[{"type":"text","text":"<block>no"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":3}}`
	sent := classifierBody(testClassifierModel)
	out := classifierCall(t, raw, sent, nil)
	resp := decodeOK[pluginapi.ExecutorResponse](t, out)
	if string(resp.Payload) != f.reply {
		t.Fatalf("the Router's verdict must come back untouched, got %s", resp.Payload)
	}
	if len(f.bodies) != 1 {
		t.Fatalf("want one Router call, got %d", len(f.bodies))
	}
	got := f.requests[0]
	if got.URL.Path != "/v1/messages" || got.Header.Get("Authorization") != "Bearer ditto_inf_test" {
		t.Fatalf("wrong upstream call: %s auth=%q", got.URL.Path, got.Header.Get("Authorization"))
	}
	var want, forwarded map[string]any
	_ = json.Unmarshal(sent, &want)
	if err := json.Unmarshal(f.bodies[0], &forwarded); err != nil {
		t.Fatal(err)
	}
	if forwarded["model"] != "auto-mode-classifier" {
		t.Fatalf("the check must address the endpoint slug, got model %v", forwarded["model"])
	}
	want["model"] = "auto-mode-classifier"
	if string(mustJSON(t, want)) != string(mustJSON(t, forwarded)) {
		t.Fatalf("only the model may change:\nwant %s\ngot  %s", mustJSON(t, want), mustJSON(t, forwarded))
	}
	exact := strings.Replace(string(sent), `"model":"`+testClassifierModel+`"`, `"model":"auto-mode-classifier"`, 1)
	if string(f.bodies[0]) != exact {
		t.Fatalf("every byte but the model must be forwarded as sent:\nwant %s\ngot  %s", exact, f.bodies[0])
	}
}

func TestReplaceTopLevelValue(t *testing.T) {
	got, err := replaceTopLevelValue([]byte(` { "a" : {"model":"inner"}, "model" : "x" , "z":[1]} `), "model", []byte(`"y"`))
	if err != nil || string(got) != ` { "a" : {"model":"inner"}, "model" : "y" , "z":[1]} ` {
		t.Fatalf("got %s, %v", got, err)
	}
	if _, err := replaceTopLevelValue([]byte(`{"model":"a","model":"b"}`), "model", []byte(`"y"`)); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate keys must be refused, got %v", err)
	}
	if _, err := replaceTopLevelValue([]byte(`{"a":1}`), "model", []byte(`"y"`)); err == nil {
		t.Fatal("a missing key must be refused")
	}
}

func TestClassifierRefusesWhatIsNotAPermissionCheck(t *testing.T) {
	big := classifierBody(testClassifierModel)
	for _, tc := range []struct {
		name    string
		bound   int
		payload []byte
		mutate  func(*executorRequest)
		reason  string
	}{
		{name: "missing request class", mutate: func(r *executorRequest) { r.Headers.Del(requestClassHeader) }, reason: "request class missing"},
		{name: "main loop", mutate: func(r *executorRequest) { r.Headers.Set(requestClassHeader, "main") }, reason: "request class main"},
		{name: "subagent", mutate: func(r *executorRequest) { r.Headers.Set(requestClassHeader, "subagent") }, reason: "not auxiliary"},
		{name: "responses wire", mutate: func(r *executorRequest) { r.Format = "openai-response" }, reason: "not an Anthropic Messages request"},
		{name: "other model", payload: classifierBody("ditto-dev"), reason: "model is not"},
		{name: "tools", payload: []byte(strings.Replace(string(big), `"metadata"`, `"tools":[{"name":"Bash"}],"metadata"`, 1)), reason: "carries tools"},
		{name: "no messages", payload: []byte(`{"model":"` + testClassifierModel + `","system":"policy","messages":[]}`), reason: "no messages"},
		{name: "image in system", payload: []byte(`{"model":"` + testClassifierModel + `","system":[{"type":"image","source":{}}],"messages":[{"role":"user","content":"x"}]}`), reason: "system prompt"},
		{name: "no system", payload: []byte(`{"model":"` + testClassifierModel + `","messages":[{"role":"user","content":"x"}]}`), reason: "system prompt"},
		{name: "not json", payload: []byte(`{"model":`), reason: "not a JSON object"},
		{name: "over the bound", bound: len(big)/bytesPerTokenFloor - 1, reason: "over this route's"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, raw := classifierCredential(t, func(c *credential) { c.MaxInputTokens = tc.bound })
			payload := tc.payload
			if payload == nil {
				payload = big
			}
			assertUnfit(t, classifierCall(t, raw, payload, tc.mutate), tc.reason)
			if len(f.bodies) != 0 {
				t.Fatalf("a refused request must not reach the Router, got %d calls", len(f.bodies))
			}
		})
	}
	// At the bound the check is carried.
	f, _, raw := classifierCredential(t, func(c *credential) { c.MaxInputTokens = (len(big) + bytesPerTokenFloor - 1) / bytesPerTokenFloor })
	f.reply = `{"content":[{"type":"text","text":"<block>no"}],"stop_reason":"end_turn"}`
	decodeOK[pluginapi.ExecutorResponse](t, classifierCall(t, raw, big, nil))
}

func TestClassifierRouterAnswersMapToFallbackOrFailover(t *testing.T) {
	for _, tc := range []struct {
		status int
		unfit  bool
		code   string
	}{
		{http.StatusBadRequest, true, ""},
		{http.StatusRequestEntityTooLarge, true, ""},
		{http.StatusUnprocessableEntity, true, ""},
		{http.StatusPaymentRequired, false, "insufficient_quota"},
		{http.StatusTooManyRequests, false, "rate_limit_exceeded"},
		{http.StatusInternalServerError, false, "upstream_error"},
		{http.StatusUnauthorized, false, "invalid_api_key"},
	} {
		f, _, raw := classifierCredential(t, nil)
		f.status, f.reply = tc.status, `{"type":"error","error":{"type":"x","message":"prompt is too long"}}`
		out := classifierCall(t, raw, classifierBody(testClassifierModel), nil)
		if tc.unfit {
			assertUnfit(t, out, "Router answered HTTP")
			continue
		}
		if e := decodeErr(t, out); e.Code != tc.code || strings.Contains(e.Message, routeUnfitMarker) {
			t.Errorf("HTTP %d: want ordinary %s failover, got %+v", tc.status, tc.code, e)
		}
	}
}

func TestClassifierRepliesWithoutAVerdictGoBackToClaude(t *testing.T) {
	for name, reply := range map[string]string{
		"empty text, budget spent": `{"content":[{"type":"text","text":"  "}],"stop_reason":"max_tokens"}`,
		"no content":               `{"content":[],"stop_reason":"end_turn"}`,
		"refusal":                  `{"content":[{"type":"text","text":"I can't help"}],"stop_reason":"refusal"}`,
		"not a message":            `[]`,
	} {
		f, _, raw := classifierCredential(t, nil)
		f.reply = reply
		out := classifierCall(t, raw, classifierBody(testClassifierModel), nil)
		e := decodeErr(t, out)
		if !strings.Contains(e.Message, routeUnfitMarker) {
			t.Errorf("%s: want a route refusal, got %+v", name, e)
		}
	}
	// Any text is the classifier's to parse, including a block verdict or
	// something Claude Code will reject; this route never judges it.
	for _, text := range []string{"<block>yes", "<block>no", "looks fine"} {
		f, _, raw := classifierCredential(t, nil)
		f.reply = `{"content":[{"type":"text","text":"` + text + `"}],"stop_reason":"end_turn"}`
		resp := decodeOK[pluginapi.ExecutorResponse](t, classifierCall(t, raw, classifierBody(testClassifierModel), nil))
		if string(resp.Payload) != f.reply {
			t.Errorf("%q: reply changed to %s", text, resp.Payload)
		}
	}
}

func TestClassifierRefusesStreamsAndTokenCounts(t *testing.T) {
	f, _, raw := classifierCredential(t, nil)
	req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{AuthProvider: provider, Model: testClassifierModel, Format: "claude", Stream: true,
		Payload: classifierBody(testClassifierModel), StorageJSON: raw, Headers: http.Header{"X-Claude-Code-Request-Class": {"auxiliary"}}}, StreamID: "s1"}
	in, _ := json.Marshal(req)
	out, err := executeStream(in)
	if err != nil {
		t.Fatal(err)
	}
	assertUnfit(t, out, "streaming")
	req.Stream = false
	in, _ = json.Marshal(req)
	out, err = countTokens(in)
	if err != nil {
		t.Fatal(err)
	}
	assertUnfit(t, out, "token counting")
	if len(f.bodies) != 0 {
		t.Fatalf("neither may reach the Router, got %d calls", len(f.bodies))
	}
}

func TestOrdinaryCredentialsAreUnchanged(t *testing.T) {
	f, _, raw := newRouter(t)
	f.status, f.reply = http.StatusBadRequest, `{"error":{"message":"bad"}}`
	out, err := execute(executorCall(t, "claude", false, raw))
	if err != nil {
		t.Fatal(err)
	}
	e := decodeErr(t, out)
	if e.Code != "invalid_request_error" || strings.Contains(e.Message, routeUnfitMarker) {
		t.Fatalf("an ordinary credential must keep its 400 mapping, got %+v", e)
	}
}
