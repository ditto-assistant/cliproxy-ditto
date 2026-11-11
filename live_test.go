//go:build live

package main

// Checks the plugin's reading of the real Ditto Router's /v1/models and
// /v1/credits. Run with:
//
//	DITTO_KEY_FILE=/abs/key DITTO_ENDPOINT=<slug> go test -tags live -run Live -v

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func liveCredential(t *testing.T) (credential, []byte) {
	t.Helper()
	keyFile, endpoint := os.Getenv("DITTO_KEY_FILE"), os.Getenv("DITTO_ENDPOINT")
	if keyFile == "" || endpoint == "" {
		t.Skip("set DITTO_KEY_FILE and DITTO_ENDPOINT")
	}
	storage, _ := json.Marshal(credential{Type: provider, Endpoint: endpoint, APIKeyFile: keyFile, BaseURL: os.Getenv("DITTO_BASE_URL")})
	c, err := decodeCredential(storage)
	if err != nil {
		t.Fatal(err)
	}
	return c, storage
}

func TestLiveRouterModels(t *testing.T) {
	c, _ := liveCredential(t)
	router, err := fetchRouterModels(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	var own []string
	for _, m := range router {
		if m.OwnedBy == "ditto" {
			own = append(own, m.ID)
		}
	}
	if !contains(own, c.Endpoint) {
		t.Fatalf("endpoint %q not among Ditto-owned ids %v", c.Endpoint, own)
	}
	selected := selectModels(config{}, []string{"claude-opus-5-5"}, router)
	t.Logf("router ids=%d ditto-owned=%v selected=%d", len(router), own, len(selected))
}

func TestLiveCredits(t *testing.T) {
	_, storage := liveCredential(t)
	req, _ := json.Marshal(quotaRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{StorageJSON: storage}})
	q := decodeOK[pluginapi.QuotaFetchResponse](t, must(fetchQuota(req)))
	if len(q.Summary) == 0 || q.Subscription == nil || q.Subscription.Plan == "" {
		t.Fatalf("quota = %+v", q)
	}
	for _, m := range q.Summary {
		t.Logf("%s = %.4f %s", m.Label, m.Value, m.Currency)
	}
	for _, g := range q.Groups {
		for _, b := range g.Buckets {
			t.Logf("bucket %s: %s (%.3f)", b.Window, b.Description, b.RemainingFraction)
		}
	}
	t.Logf("plan=%s", q.Subscription.Plan)
}
