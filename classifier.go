package main

// The Auto Mode classifier route.
//
// Claude Code asks a model to judge each tool call it would run in Auto Mode.
// CLAUDE_CODE_AUTO_MODE_MODEL names that model, and Claude Code reads it for
// nothing else, so `heyditto claude --cliproxy --auto-mode-classifier <id>`
// gives permission checks a model id of their own. A credential with
// "classifier_model" set serves exactly that id and sends each check to its
// endpoint, whose model (for example GLM Flash) answers.
//
// This route must never decide a permission itself. It forwards the check
// unchanged except for the model, never edits or shortens the policy or the
// transcript, and returns Router answers untouched. Whatever it cannot carry
// faithfully it refuses with routeUnfitMarker: the credential's
// request-scoped rule tells CLIProxy to send that same request to the next
// credential serving the id (the Claude subscriptions, through an OAuth
// model alias), without cooling this credential down. With no such
// credential the refusal reaches Claude Code, which blocks the action, as it
// does for any classifier error.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

const (
	// routeUnfitMarker is in every refusal the classifier route makes on its
	// own behalf; the credential's request-scoped rule matches it.
	routeUnfitMarker = "ditto_classifier_route_unfit"
	routeUnfitStatus = http.StatusServiceUnavailable

	requestClassHeader = "X-Claude-Code-Request-Class"

	// bytesPerTokenFloor turns a request size into a token estimate that errs
	// high: Claude-tokenized English and JSON average more bytes per token
	// than this, so an estimate under the limit is not an undercount.
	bytesPerTokenFloor = 3
)

var classifierModelID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func validateClassifierCredential(c credential) error {
	if c.ClassifierModel == "" {
		if c.MaxInputTokens != 0 {
			return fmt.Errorf("max_input_tokens applies only to a credential with classifier_model")
		}
		return nil
	}
	if !classifierModelID.MatchString(c.ClassifierModel) {
		return fmt.Errorf("classifier_model must be lowercase letters, digits, '.', '_' or '-' (at most 64 characters)")
	}
	if strings.HasPrefix(c.ClassifierModel, "claude-") {
		return fmt.Errorf("classifier_model must be a dedicated id, not a claude-* id the subscriptions also serve under that name")
	}
	if strings.TrimSpace(c.Endpoint) == "" {
		return fmt.Errorf("a classifier credential needs the endpoint whose model answers the checks")
	}
	if c.MaxInputTokens < 0 {
		return fmt.Errorf("max_input_tokens cannot be negative")
	}
	return nil
}

// classifierScopedErrors is the CLIProxy request-scoped rule for this
// credential's own refusals: try the next credential, no cooldown.
func classifierScopedErrors() []map[string]any {
	return []map[string]any{{"status": routeUnfitStatus, "match": []string{routeUnfitMarker}, "action": "continue"}}
}

func routeUnfit(reason string) []byte {
	return errorEnvelope(routeUnfitMarker, "Ditto classifier route did not take this request ("+reason+"); "+routeUnfitMarker, routeUnfitStatus)
}

// classifierPayload checks that a request is an Auto Mode permission check
// this route can carry intact and returns the body addressed to the endpoint.
// Every check here can only send a request back to the other credentials.
func classifierPayload(c credential, r executorRequest, path string) ([]byte, []byte) {
	if path != "/v1/messages" {
		return nil, routeUnfit("not an Anthropic Messages request")
	}
	if r.Stream {
		return nil, routeUnfit("streaming; Claude Code's classifier does not stream")
	}
	if class := strings.TrimSpace(r.Headers.Get(requestClassHeader)); !strings.EqualFold(class, "auxiliary") {
		if class == "" {
			class = "missing"
		}
		return nil, routeUnfit("request class " + class + ", not auxiliary; launch Claude Code with CLAUDE_CODE_GATEWAY_HINT_HEADERS=1")
	}
	body := r.body()
	if c.MaxInputTokens > 0 {
		if estimate := (len(body) + bytesPerTokenFloor - 1) / bytesPerTokenFloor; estimate > c.MaxInputTokens {
			return nil, routeUnfit(fmt.Sprintf("about %d input tokens, over this route's %d", estimate, c.MaxInputTokens))
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, routeUnfit("body is not a JSON object")
	}
	var model string
	if json.Unmarshal(fields["model"], &model) != nil || model != c.ClassifierModel {
		return nil, routeUnfit("model is not " + c.ClassifierModel)
	}
	if tools := bytes.TrimSpace(fields["tools"]); len(tools) > 0 && !bytes.Equal(tools, []byte("null")) && !bytes.Equal(tools, []byte("[]")) {
		return nil, routeUnfit("carries tools; a permission check has none")
	}
	var messages []json.RawMessage
	if json.Unmarshal(fields["messages"], &messages) != nil || len(messages) == 0 {
		return nil, routeUnfit("no messages")
	}
	// On a non-Claude model the Router flattens `system` to text and drops a
	// shape it cannot read. The security policy lives there, so anything
	// other than a string or plain text blocks stays with Claude.
	if !systemIsPlainText(fields["system"]) {
		return nil, routeUnfit("system prompt is not plain text the Router keeps whole")
	}
	// The transcript is the evidence being judged, and the same translation
	// drops block types it does not know. Only text goes to the route.
	for _, m := range messages {
		if !messageIsPlainText(m) {
			return nil, routeUnfit("a message carries content other than text, which the Router may drop")
		}
	}
	slug, err := json.Marshal(c.Endpoint)
	if err != nil {
		return nil, routeUnfit("endpoint slug")
	}
	out, err := replaceTopLevelValue(body, "model", slug)
	if err != nil {
		return nil, routeUnfit(err.Error())
	}
	return out, nil
}

// replaceTopLevelValue swaps one top-level member's value in place, so every
// other byte of the policy and transcript is forwarded exactly as Claude Code
// sent it. A body that names the key more than once is refused: readers
// disagree on which duplicate wins.
func replaceTopLevelValue(body []byte, key string, value []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("body is not a JSON object")
	}
	start, end := -1, -1
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("body is not valid JSON")
		}
		name, _ := tok.(string)
		var member json.RawMessage
		if err := dec.Decode(&member); err != nil {
			return nil, fmt.Errorf("body is not valid JSON")
		}
		if name != key {
			continue
		}
		if start >= 0 {
			return nil, fmt.Errorf("body names %q twice", key)
		}
		end = int(dec.InputOffset())
		start = end - len(member)
	}
	if start < 0 {
		return nil, fmt.Errorf("body has no %q", key)
	}
	out := make([]byte, 0, len(body)-(end-start)+len(value))
	out = append(out, body[:start]...)
	out = append(out, value...)
	return append(out, body[end:]...), nil
}

// messageIsPlainText accepts a user or assistant message whose content is a
// string or text blocks only.
func messageIsPlainText(raw json.RawMessage) bool {
	var m struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil || (m.Role != "user" && m.Role != "assistant") {
		return false
	}
	content := bytes.TrimSpace(m.Content)
	var s string
	if json.Unmarshal(content, &s) == nil {
		return true
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		var kind, text string
		if json.Unmarshal(b["type"], &kind) != nil || kind != "text" || json.Unmarshal(b["text"], &text) != nil {
			return false
		}
	}
	return true
}

func systemIsPlainText(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s) != ""
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil || len(blocks) == 0 {
		return false
	}
	text := 0
	for _, b := range blocks {
		var kind, value string
		if json.Unmarshal(b["type"], &kind) != nil || kind != "text" || json.Unmarshal(b["text"], &value) != nil {
			return false
		}
		text += len(strings.TrimSpace(value))
	}
	return text > 0
}

// classifierUpstreamError maps a Router refusal of a permission check. A
// refusal of this request (bad parameter, too long, too large) goes back to
// the other credentials, which may well accept it; credit, auth, rate and
// server failures keep their usual meaning, so CLIProxy fails over and cools
// this credential down.
func classifierUpstreamError(status int, body []byte) []byte {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return routeUnfit(fmt.Sprintf("Router answered HTTP %d", status))
	default:
		return upstreamError(status, body)
	}
}

// classifierReplyUnfit reports a 200 reply that cannot carry a verdict: no
// text at all (for example a reasoning model that spent its whole budget) or
// a provider refusal. Claude Code would block the action on either; Claude
// gets the chance to answer instead. A reply with text is never judged here.
func classifierReplyUnfit(body []byte) string {
	var reply struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return "reply is not a Messages response"
	}
	if reply.StopReason == "refusal" {
		return "the model refused"
	}
	for _, block := range reply.Content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			return ""
		}
	}
	return "reply has no text (stop_reason " + reply.StopReason + ")"
}
