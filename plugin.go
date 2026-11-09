package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	provider        = "ditto"
	pluginVersion   = "0.1.0"
	defaultBaseURL  = "https://inference.heyditto.ai"
	defaultPriority = -100
)

// Model families mirrored from your other credentials by default: the
// subscription providers CLIProxy serves (Claude, Codex/ChatGPT, Gemini, Grok).
var defaultFallbackModels = []string{"claude-*", "gpt-*", "o1*", "o3*", "o4*", "codex-*", "gemini-*", "grok-*"}

type config struct {
	// BaseURL is the Ditto Router origin; a credential file may override it.
	BaseURL string `yaml:"base-url"`
	// FallbackModels are globs over model ids registered by your other
	// credentials. Matching ids are also registered for Ditto, so CLIProxy
	// reaches Ditto for them only after every higher-priority credential is
	// cooling down or unavailable.
	FallbackModels []string `yaml:"fallback-models"`
	// ExtraModels are ids registered for Ditto regardless of other credentials.
	ExtraModels []string `yaml:"extra-models"`
	// ExposeCatalog also registers Ditto's whole provider catalog
	// (OpenRouter-style ids such as "anthropic/claude-opus-4.5").
	ExposeCatalog bool `yaml:"expose-catalog"`
	// CredentialPriority is used for credential files that set no priority.
	CredentialPriority *int `yaml:"credential-priority"`
}

func (c config) priority() int {
	if c.CredentialPriority != nil {
		return *c.CredentialPriority
	}
	return defaultPriority
}

func (c config) baseURL() string {
	if s := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/"); s != "" {
		return s
	}
	return defaultBaseURL
}

func (c config) fallbackModels() []string {
	if c.FallbackModels != nil {
		return c.FallbackModels
	}
	return defaultFallbackModels
}

var current atomic.Pointer[config]

func init() { current.Store(&config{}) }

func cfg() config { return *current.Load() }

// callHost is the host callback channel; abi.go installs the cgo version and
// tests install fakes.
var callHost func(method string, payload any) (json.RawMessage, error)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string, httpStatus int) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message, HTTPStatus: httpStatus}})
	return raw
}

func decodeHostEnvelope(method string, raw []byte, code int) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response (code %d)", method, code)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode host callback %s: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("host callback %s: %s: %s", method, env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if code != 0 {
		return nil, fmt.Errorf("host callback %s returned code %d", method, code)
	}
	return env.Result, nil
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return fmt.Errorf("decode lifecycle request: %w", err)
		}
	}
	next := config{}
	if len(req.ConfigYAML) > 0 {
		if err := yaml.Unmarshal(req.ConfigYAML, &next); err != nil {
			return fmt.Errorf("decode plugin config: %w", err)
		}
	}
	current.Store(&next)
	return nil
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

type capabilities struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats"`
	QuotaProvider         bool                         `json:"quota_provider"`
}

// Ditto Router speaks these wires natively (Codex is the Responses API), so
// declaring them lets CLIProxy hand the client's request over untranslated.
var wireFormats = []string{"claude", "openai-response", "codex", "openai"}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             provider,
			Version:          pluginVersion,
			Author:           "Ditto",
			GitHubRepository: "https://github.com/ditto-assistant/cliproxy-ditto",
			Logo:             "https://heyditto.ai/favicon.svg",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "base-url", Type: pluginapi.ConfigFieldTypeString, Default: defaultBaseURL, Description: "Ditto Router origin."},
				{Name: "fallback-models", Type: pluginapi.ConfigFieldTypeArray, Default: defaultFallbackModels, Description: "Globs over model ids your other credentials serve; Ditto backs them up at low priority."},
				{Name: "extra-models", Type: pluginapi.ConfigFieldTypeArray, Description: "Model ids to register for Ditto even when no other credential serves them."},
				{Name: "expose-catalog", Type: pluginapi.ConfigFieldTypeBoolean, Default: false, Description: "Also register Ditto's full catalog (provider/model ids, OpenRouter style)."},
				{Name: "credential-priority", Type: pluginapi.ConfigFieldTypeInteger, Default: defaultPriority, Description: "Priority for Ditto credentials without their own; lower than your subscriptions keeps Ditto last."},
			},
		},
		Capabilities: capabilities{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  wireFormats,
			ExecutorOutputFormats: wireFormats,
			QuotaProvider:         true,
		},
	}
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(request); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return okEnvelope(map[string]any{})

	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier, pluginabi.MethodQuotaIdentifier:
		return okEnvelope(identifierResponse{Identifier: provider})
	case pluginabi.MethodAuthParse:
		return parseAuth(request)
	case pluginabi.MethodAuthRefresh:
		return refreshAuth(request)
	case pluginabi.MethodAuthLoginStart, pluginabi.MethodAuthLoginPoll:
		return errorEnvelope("login_unsupported", loginHint, 400), nil

	case pluginabi.MethodModelStatic:
		return okEnvelope(pluginapi.ModelResponse{Provider: provider})
	case pluginabi.MethodModelForAuth:
		return modelsForAuth(request)

	case pluginabi.MethodExecutorExecute:
		return execute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return executeStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return countTokens(request)
	case pluginabi.MethodExecutorHTTPRequest:
		return errorEnvelope("unsupported", "the Ditto executor only serves model requests", 501), nil

	case pluginabi.MethodQuotaDescribe:
		return okEnvelope(pluginapi.QuotaDescribeResponse{SupportedProviders: []string{provider}, DisplayName: "Ditto credits"})
	case pluginabi.MethodQuotaFetch:
		return fetchQuota(request)
	case pluginabi.MethodQuotaReset:
		return okEnvelope(pluginapi.QuotaResetResponse{Success: false, Message: "Ditto spend limits are managed at heyditto.ai"})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, 0), nil
	}
}
