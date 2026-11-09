package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Requests carry this so the Router can tell CLIProxy overflow from its
// other traffic, and refuse to relay it back into the same CLIProxy.
const viaHeader = "X-Ditto-Via"

var viaValue = "cliproxy-ditto/" + pluginVersion

var httpClient = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout:   15 * time.Second,
	ResponseHeaderTimeout: 10 * time.Minute, // a long reasoning turn may stream nothing for minutes
	MaxIdleConnsPerHost:   16,
	IdleConnTimeout:       90 * time.Second,
	ForceAttemptHTTP2:     true,
}}

// Client headers the Router uses: protocol versions and betas, the harness
// identity (its savings features key on the client), and session ids for
// cache affinity. Everything else stays behind, in particular the client's
// own Authorization, which is a CLIProxy access key.
var forwardHeaders = []string{
	"Accept",
	"Anthropic-Version",
	"Anthropic-Beta",
	"Anthropic-Dangerous-Direct-Browser-Access",
	"OpenAI-Beta",
	"User-Agent",
	"Originator",
	"Version",
	"Session_id",
	"X-Claude-Code-Session-Id",
	"X-Session-Id",
	"X-Codex-Turn-State",
	"X-Codex-Turn-Metadata",
}

func upstreamPath(format string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "claude", "anthropic":
		return "/v1/messages", nil
	case "openai-response", "responses", "openai-responses", "codex":
		return "/v1/responses", nil
	case "openai", "chat-completions", "":
		return "/v1/chat/completions", nil
	default:
		return "", fmt.Errorf("Ditto does not accept the %q wire format", format)
	}
}

type executorRequest struct {
	pluginapi.ExecutorRequest
	StreamID string `json:"stream_id,omitempty"`
}

func (r executorRequest) body() []byte {
	if len(r.Payload) > 0 {
		return r.Payload
	}
	return r.OriginalRequest
}

func buildUpstream(ctx context.Context, r executorRequest, path string) (*http.Request, error) {
	c, err := decodeCredential(r.StorageJSON)
	if err != nil {
		return nil, err
	}
	key, err := c.key()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL()+path, bytes.NewReader(r.body()))
	if err != nil {
		return nil, err
	}
	for _, name := range forwardHeaders {
		for _, v := range r.Headers.Values(name) {
			req.Header.Add(name, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set(viaHeader, viaValue)
	if r.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	return req, nil
}

// upstreamError turns a Router refusal into an error CLIProxy classifies.
// 402 (Ditto credits or the endpoint's spend limit) becomes a quota error so
// the Ditto credential cools down instead of being retried.
func upstreamError(status int, body []byte) []byte {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 2048 {
		msg = msg[:2048]
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	switch {
	case status == http.StatusPaymentRequired:
		return errorEnvelope("insufficient_quota", "Ditto: "+msg, http.StatusForbidden)
	case status == http.StatusUnauthorized:
		return errorEnvelope("invalid_api_key", "Ditto: "+msg, status)
	case status == http.StatusTooManyRequests:
		return errorEnvelope("rate_limit_exceeded", "Ditto: "+msg, status)
	case status >= 500:
		return errorEnvelope("upstream_error", "Ditto: "+msg, status)
	default:
		return errorEnvelope("invalid_request_error", "Ditto: "+msg, status)
	}
}

func transportError(err error) []byte {
	return errorEnvelope("upstream_unreachable", "Ditto Router unreachable: "+err.Error(), http.StatusBadGateway)
}

func responseHeaders(h http.Header) http.Header {
	out := http.Header{}
	for _, name := range []string{"Content-Type", "Request-Id", "X-Request-Id", "Retry-After"} {
		if v := h.Get(name); v != "" {
			out.Set(name, v)
		}
	}
	return out
}

func decodeExecutorRequest(raw []byte) (executorRequest, string, []byte) {
	var r executorRequest
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, "", errorEnvelope("invalid_request", "decode executor request: "+err.Error(), 400)
	}
	path, err := upstreamPath(r.Format)
	if err != nil {
		return r, "", errorEnvelope("unsupported_format", err.Error(), 400)
	}
	return r, path, nil
}

func execute(raw []byte) ([]byte, error) {
	r, path, bad := decodeExecutorRequest(raw)
	if bad != nil {
		return bad, nil
	}
	return roundTrip(r, path)
}

func countTokens(raw []byte) ([]byte, error) {
	r, path, bad := decodeExecutorRequest(raw)
	if bad != nil {
		return bad, nil
	}
	if path != "/v1/messages" {
		return errorEnvelope("unsupported", "Ditto counts tokens only for Anthropic Messages requests", 404), nil
	}
	r.Stream = false
	return roundTrip(r, "/v1/messages/count_tokens")
}

func roundTrip(r executorRequest, path string) ([]byte, error) {
	req, err := buildUpstream(context.Background(), r, path)
	if err != nil {
		return errorEnvelope("invalid_credential", err.Error(), 401), nil
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return transportError(err), nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return transportError(err), nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return upstreamError(resp.StatusCode, body), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: body, Headers: responseHeaders(resp.Header)})
}

type streamStart struct {
	Headers http.Header `json:"headers,omitempty"`
}

type streamEmit struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
}

type streamClose struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

// executeStream waits for the Router's status before answering, so a refusal
// reaches CLIProxy as an ordinary error it can fail over on; only an accepted
// stream is pumped to the host in the background.
func executeStream(raw []byte) ([]byte, error) {
	r, path, bad := decodeExecutorRequest(raw)
	if bad != nil {
		return bad, nil
	}
	if strings.TrimSpace(r.StreamID) == "" {
		return errorEnvelope("executor_error", "stream_id is required for executor.execute_stream", 0), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, err := buildUpstream(ctx, r, path)
	if err != nil {
		cancel()
		return errorEnvelope("invalid_credential", err.Error(), 401), nil
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		cancel()
		return transportError(err), nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		cancel()
		return upstreamError(resp.StatusCode, body), nil
	}
	go func() {
		defer cancel()
		defer resp.Body.Close()
		errPump := pumpEvents(resp.Body, func(event []byte) error {
			_, err := callHost(pluginabi.MethodHostStreamEmit, streamEmit{StreamID: r.StreamID, Payload: event})
			return err
		})
		msg := ""
		if errPump != nil && !errors.Is(errPump, errEmitStopped) {
			msg = "Ditto stream interrupted: " + errPump.Error()
		}
		_, _ = callHost(pluginabi.MethodHostStreamClose, streamClose{StreamID: r.StreamID, Error: msg})
	}()
	return okEnvelope(streamStart{Headers: responseHeaders(resp.Header)})
}

var errEmitStopped = errors.New("host stopped accepting chunks")

// pumpEvents forwards one complete SSE event (through its blank line) per
// chunk, the unit CLIProxy's own executors emit. A refused emit means the
// client went away; the caller then drops the upstream connection.
func pumpEvents(body io.Reader, emit func([]byte) error) error {
	reader := bufio.NewReaderSize(body, 64<<10)
	var event bytes.Buffer
	flush := func() error {
		if event.Len() == 0 {
			return nil
		}
		chunk := bytes.Clone(event.Bytes())
		event.Reset()
		if err := emit(chunk); err != nil {
			return fmt.Errorf("%w: %v", errEmitStopped, err)
		}
		return nil
	}
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			event.Write(line)
			if len(bytes.TrimRight(line, "\r\n")) == 0 {
				if errFlush := flush(); errFlush != nil {
					return errFlush
				}
			}
		}
		if err == io.EOF {
			return flush()
		}
		if err != nil {
			return err
		}
	}
}
