package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// credits is the Router's GET /v1/credits document for an endpoint key.
type credits struct {
	Currency    string        `json:"currency"`
	BillingMode string        `json:"billing_mode"`
	TopUpURL    string        `json:"top_up_url"`
	Balance     float64       `json:"balance"`
	Endpoint    spendSnapshot `json:"endpoint"`
	spendSnapshot
}

type spendSnapshot struct {
	Limit          *float64 `json:"limit"`
	LimitRemaining *float64 `json:"limit_remaining"`
	Period         *string  `json:"limit_reset"`
	Usage          float64  `json:"usage"`
}

func quotaFromCredits(c credits) pluginapi.QuotaFetchResponse {
	currency := strings.ToUpper(strings.TrimSpace(c.Currency))
	if currency == "" {
		currency = "USD"
	}
	money := func(key, label string, v float64) pluginapi.QuotaMetric {
		return pluginapi.QuotaMetric{Key: key, Label: label, Value: v, Format: "currency", Currency: currency}
	}
	out := pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{Plan: c.BillingMode},
		Summary: []pluginapi.QuotaMetric{
			money("balance", "Ditto balance", c.Balance),
			money("endpoint_usage", "Endpoint spend", c.Endpoint.Usage),
		},
	}
	var buckets []pluginapi.QuotaBucket
	for _, s := range []struct {
		window string
		snap   spendSnapshot
	}{{"endpoint", c.Endpoint}, {"key", c.spendSnapshot}} {
		if s.snap.Limit == nil || *s.snap.Limit <= 0 || s.snap.LimitRemaining == nil {
			continue
		}
		desc := fmt.Sprintf("%.2f of %.2f %s left", *s.snap.LimitRemaining, *s.snap.Limit, currency)
		if s.snap.Period != nil {
			desc += ", resets " + *s.snap.Period
		}
		buckets = append(buckets, pluginapi.QuotaBucket{
			Window:            s.window,
			RemainingFraction: min(1, max(0, *s.snap.LimitRemaining / *s.snap.Limit)),
			Description:       desc,
		})
	}
	if len(buckets) > 0 {
		out.Groups = []pluginapi.QuotaGroup{{DisplayName: "Spend limits", Buckets: buckets}}
	}
	return out
}

type quotaRequest struct {
	pluginapi.QuotaFetchRequest
}

func fetchQuota(raw []byte) ([]byte, error) {
	var req quotaRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	c, err := decodeCredential(req.StorageJSON)
	if err != nil {
		return errorEnvelope("invalid_credential", err.Error(), 400), nil
	}
	key, err := c.key()
	if err != nil {
		return errorEnvelope("invalid_credential", err.Error(), 401), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL()+"/v1/credits", nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+key)
	httpReq.Header.Set(viaHeader, viaValue)
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return transportError(err), nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return transportError(err), nil
	}
	if resp.StatusCode != http.StatusOK {
		return upstreamError(resp.StatusCode, body), nil
	}
	var doc credits
	if err := json.Unmarshal(body, &doc); err != nil {
		return errorEnvelope("upstream_error", "decode Ditto credits: "+err.Error(), 502), nil
	}
	return okEnvelope(quotaFromCredits(doc))
}
