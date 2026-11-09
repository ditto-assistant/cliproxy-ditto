package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const loginHint = "Ditto has no browser login here: mint an endpoint key with " +
	"`heyditto endpoints keys create <endpoint> --store file --key-file <path>` and " +
	"add a {\"type\":\"ditto\",\"endpoint\":\"<endpoint>\",\"api_key_file\":\"<path>\"} credential file"

// Model ids are mirrored from the other credentials, which may load after
// this one, so the first refresh comes soon and later ones keep the mirror
// current as subscriptions are added or removed.
const (
	firstRefresh = 30 * time.Second
	refreshEvery = 10 * time.Minute
)

// credential is the auth file CLIProxy finds in its auth directory. The key
// lives in the file itself or, preferably, in a separate file the CLI wrote.
type credential struct {
	Type       string `json:"type"`
	Endpoint   string `json:"endpoint"`
	Label      string `json:"label,omitempty"`
	APIKey     string `json:"api_key,omitempty"`
	APIKeyFile string `json:"api_key_file,omitempty"`
	BaseURL    string `json:"base_url,omitempty"`
	Priority   *int   `json:"priority,omitempty"`
	Disabled   bool   `json:"disabled,omitempty"`
}

func decodeCredential(raw []byte) (credential, error) {
	var c credential
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("decode Ditto credential: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(c.Type), provider) {
		return c, errNotDitto
	}
	if c.APIKey == "" && c.APIKeyFile == "" {
		return c, errors.New("Ditto credential needs api_key or api_key_file")
	}
	if c.APIKeyFile != "" && !filepath.IsAbs(c.APIKeyFile) {
		return c, errors.New("Ditto credential api_key_file must be an absolute path")
	}
	return c, nil
}

var errNotDitto = errors.New("not a Ditto credential")

func (c credential) key() (string, error) {
	key := c.APIKey
	if c.APIKeyFile != "" {
		raw, err := os.ReadFile(c.APIKeyFile)
		if err != nil {
			return "", fmt.Errorf("read Ditto key file: %w", err)
		}
		key = string(raw)
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return "", errors.New("Ditto key is empty or malformed")
	}
	return key, nil
}

func (c credential) baseURL() string {
	if s := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/"); s != "" {
		return s
	}
	return cfg().baseURL()
}

func (c credential) priority() int {
	if c.Priority != nil {
		return *c.Priority
	}
	return cfg().priority()
}

// authData builds the runtime record. The key never goes into metadata or
// attributes, which the management API displays; only StorageJSON holds it.
func (c credential) authData(id, fileName string, raw []byte, refreshAfter time.Duration) pluginapi.AuthData {
	label := c.Label
	if label == "" {
		label = "Ditto"
		if c.Endpoint != "" {
			label += " · " + c.Endpoint
		}
	}
	priority := strconv.Itoa(c.priority())
	return pluginapi.AuthData{
		Provider:    provider,
		ID:          id,
		FileName:    fileName,
		Label:       label,
		Disabled:    c.Disabled,
		StorageJSON: append([]byte(nil), raw...),
		Metadata:    map[string]any{"type": provider, "endpoint": c.Endpoint, "priority": c.priority()},
		// No "base_url" attribute: CLIProxy hands any auth carrying one to its
		// built-in OpenAI-compatible executor instead of this plugin.
		Attributes: map[string]string{"priority": priority, "endpoint": c.Endpoint},
		// Mirroring models happens at registration, so a refresh is what
		// picks up subscriptions that loaded after this credential.
		NextRefreshAfter: time.Now().Add(refreshAfter),
	}
}

func authID(fileName, endpoint string) string {
	base := strings.TrimSuffix(filepath.Base(fileName), filepath.Ext(fileName))
	if base == "" || base == "." {
		base = "ditto-" + endpoint
	}
	return base
}

func parseAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	c, err := decodeCredential(req.RawJSON)
	if errors.Is(err, errNotDitto) {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	if err != nil {
		return errorEnvelope("invalid_credential", err.Error(), 400), nil
	}
	data := c.authData(authID(req.FileName, c.Endpoint), req.FileName, req.RawJSON, firstRefresh)
	return okEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: data})
}

type refreshRequest struct {
	pluginapi.AuthRefreshRequest
}

func refreshAuth(raw []byte) ([]byte, error) {
	var req refreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	c, err := decodeCredential(req.StorageJSON)
	if err != nil {
		return errorEnvelope("invalid_credential", err.Error(), 400), nil
	}
	// An empty file name keeps the host's.
	data := c.authData(req.AuthID, "", req.StorageJSON, refreshEvery)
	return okEnvelope(pluginapi.AuthRefreshResponse{Auth: data, NextRefreshAfter: data.NextRefreshAfter})
}
