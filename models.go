package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Ditto registers three kinds of model ids for a credential:
//
//   - mirrored: ids your other credentials (subscriptions) already serve that
//     match fallback-models. Registering the same id at a lower priority is
//     what makes Ditto the overflow for that model.
//   - Ditto-native: the endpoint's slug and aliases (owned_by "ditto"), so a
//     client can ask for "ditto-dev" and get that endpoint's routing.
//   - catalog: with expose-catalog, every provider/model id Ditto serves.
type routerModel struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by"`
	Created int64  `json:"created"`
}

func fetchRouterModels(ctx context.Context, c credential) ([]routerModel, error) {
	key, err := c.key()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL()+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set(viaHeader, viaValue)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Ditto model list: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var list struct {
		Data []routerModel `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode Ditto model list: %w", err)
	}
	return list.Data, nil
}

type hostAuthModels struct {
	Credentials []pluginapi.HostAuthModels `json:"credentials"`
}

// mirroredModels asks the host which ids the other credentials registered.
func mirroredModels() ([]string, error) {
	if callHost == nil {
		return nil, nil
	}
	raw, err := callHost(pluginabi.MethodHostAuthModels, pluginapi.HostAuthModelsRequest{})
	if err != nil {
		return nil, err
	}
	var resp hostAuthModels
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode host auth models: %w", err)
	}
	var out []string
	for _, cred := range resp.Credentials {
		if strings.EqualFold(cred.Provider, provider) {
			continue
		}
		out = append(out, cred.Models...)
	}
	return out, nil
}

func matchesAny(id string, globs []string) bool {
	for _, g := range globs {
		if ok, _ := path.Match(strings.ToLower(strings.TrimSpace(g)), strings.ToLower(id)); ok {
			return true
		}
	}
	return false
}

// selectModels is the pure core of model registration.
func selectModels(conf config, mirrored []string, router []routerModel) []pluginapi.ModelInfo {
	seen := map[string]bool{}
	var out []pluginapi.ModelInfo
	add := func(id, ownedBy, display string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, pluginapi.ModelInfo{ID: id, Object: "model", OwnedBy: ownedBy, DisplayName: display, Type: provider})
	}
	globs := conf.fallbackModels()
	for _, id := range mirrored {
		if matchesAny(id, globs) {
			add(id, provider, id+" (Ditto fallback)")
		}
	}
	for _, m := range router {
		if m.OwnedBy == "ditto" {
			add(m.ID, provider, "Ditto · "+m.ID)
		}
	}
	for _, id := range conf.ExtraModels {
		add(id, provider, id+" (Ditto)")
	}
	if conf.ExposeCatalog {
		for _, m := range router {
			add(m.ID, m.OwnedBy, m.ID)
		}
	}
	slices.SortFunc(out, func(a, b pluginapi.ModelInfo) int { return strings.Compare(a.ID, b.ID) })
	return out
}

type authModelRequest struct {
	pluginapi.AuthModelRequest
}

func modelsForAuth(raw []byte) ([]byte, error) {
	var req authModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	c, err := decodeCredential(req.StorageJSON)
	if err != nil {
		return errorEnvelope("invalid_credential", err.Error(), 400), nil
	}
	// A classifier credential serves its one id and nothing else: mirroring
	// would make it a fallback for the main models, and the endpoint's own
	// ids would let other traffic reach the classifier's model.
	if c.ClassifierModel != "" {
		return okEnvelope(pluginapi.ModelResponse{Provider: provider, Models: []pluginapi.ModelInfo{{
			ID: c.ClassifierModel, Object: "model", OwnedBy: provider, Type: provider,
			DisplayName: "Ditto · " + c.Endpoint + " (Auto Mode checks)",
		}}})
	}
	conf := cfg()
	// Each source degrades on its own: a Router outage still leaves the
	// mirrored fallback ids, and a host without the callback still leaves
	// the endpoint's own ids.
	mirrored, errMirror := mirroredModels()
	router, errRouter := fetchRouterModels(context.Background(), c)
	models := selectModels(conf, mirrored, router)
	if len(models) == 0 && (errMirror != nil || errRouter != nil) {
		return errorEnvelope("model_discovery_failed", fmt.Sprintf("Ditto models unavailable (host: %v; router: %v)", errMirror, errRouter), 502), nil
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: provider, Models: models})
}
