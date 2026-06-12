// Loki push client for the AIQG demo-traffic generator.
//
// Pushes JSON log lines to Loki's HTTP push API under the stream labels
// the dashboard queries: {namespace="tas-llm-router"}. Each value is a
// [unix_nanoseconds_string, line] pair, where the line is the synthesized
// "aiqg response event" JSON. Loki's `| json` then parses the promoted
// fields the dashboard unwraps.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// lokiClient posts to {LokiURL}/loki/api/v1/push.
type lokiClient struct {
	URL    string // base, e.g. https://loki.tas.scharber.com
	OrgID  string // optional X-Scope-OrgID for multi-tenant Loki ("" = none)
	HTTP   *http.Client
}

func newLokiClient(url, orgID string, insecure bool) *lokiClient {
	hc := &http.Client{Timeout: 30 * time.Second}
	if insecure {
		// TAS Loki sits behind cert-manager's internal tas-ca-issuer,
		// whose CA isn't in the system trust store. Skip verification
		// when targeting the in-cluster ingress from outside.
		hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	return &lokiClient{URL: url, OrgID: orgID, HTTP: hc}
}

// stream is one Loki push stream: a label set + its timestamped values.
type lokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

type lokiPushBody struct {
	Streams []lokiStream `json:"streams"`
}

// entry pairs a timestamp with an already-marshalled JSON line.
type lokiEntry struct {
	TS   time.Time
	Line string
}

// push sends all entries as a single stream. Loki requires entries
// within a stream to be in ascending timestamp order, so we sort first.
func (c *lokiClient) push(ctx context.Context, labels map[string]string, entries []lokiEntry) error {
	if len(entries) == 0 {
		return nil
	}
	sorted := make([]lokiEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].TS.Before(sorted[j].TS) })

	values := make([][2]string, 0, len(sorted))
	for _, e := range sorted {
		values = append(values, [2]string{fmt.Sprintf("%d", e.TS.UnixNano()), e.Line})
	}

	body, err := json.Marshal(lokiPushBody{Streams: []lokiStream{{Stream: labels, Values: values}}})
	if err != nil {
		return fmt.Errorf("marshal push body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/loki/api/v1/push", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.OrgID != "" {
		req.Header.Set("X-Scope-OrgID", c.OrgID)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("push to loki: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("loki push returned %d: %s", resp.StatusCode, buf.String())
	}
	return nil
}

// marshalLine renders a field map to a compact JSON log line.
func marshalLine(fields map[string]any) (string, error) {
	b, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
