package datasource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/sratabix/dell-ipmitemps/internal/config"
)

type Fetcher struct {
	cfg    config.Datasource
	client *http.Client
}

func New(cfg config.Datasource) *Fetcher {
	return &Fetcher{
		cfg:    cfg,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (f *Fetcher) FetchTemp(ctx context.Context, query string) (float64, error) {
	switch f.cfg.Type {
	case config.Prometheus:
		u := fmt.Sprintf("%s/api/v1/query?query=%s",
			f.cfg.Prometheus.URL, url.QueryEscape(query))
		body, err := f.get(ctx, u, nil)
		if err != nil {
			return 0, err
		}
		return parsePromResponse(body)
	case config.Grafana:
		u := fmt.Sprintf("%s/api/datasources/proxy/uid/%s/api/v1/query?query=%s",
			f.cfg.Grafana.URL, f.cfg.Grafana.DatasourceUID, url.QueryEscape(query))
		body, err := f.get(ctx, u, map[string]string{
			"Authorization": "Bearer " + f.cfg.Grafana.Token,
		})
		if err != nil {
			return 0, err
		}
		return parsePromResponse(body)
	default:
		return 0, fmt.Errorf("unknown datasource type: %q", f.cfg.Type)
	}
}

type Sample struct {
	T float64
	V float64
}

func (f *Fetcher) FetchRange(ctx context.Context, query string, lookback, step time.Duration) ([]Sample, error) {
	now := time.Now()
	start := now.Add(-lookback)
	params := url.Values{}
	params.Set("query", query)
	params.Set("start", strconv.FormatInt(start.Unix(), 10))
	params.Set("end", strconv.FormatInt(now.Unix(), 10))
	params.Set("step", strconv.FormatFloat(step.Seconds(), 'f', -1, 64))

	switch f.cfg.Type {
	case config.Prometheus:
		u := fmt.Sprintf("%s/api/v1/query_range?%s", f.cfg.Prometheus.URL, params.Encode())
		body, err := f.get(ctx, u, nil)
		if err != nil {
			return nil, err
		}
		return parsePromRange(body)
	case config.Grafana:
		u := fmt.Sprintf("%s/api/datasources/proxy/uid/%s/api/v1/query_range?%s",
			f.cfg.Grafana.URL, f.cfg.Grafana.DatasourceUID, params.Encode())
		body, err := f.get(ctx, u, map[string]string{
			"Authorization": "Bearer " + f.cfg.Grafana.Token,
		})
		if err != nil {
			return nil, err
		}
		return parsePromRange(body)
	default:
		return nil, fmt.Errorf("unknown datasource type: %q", f.cfg.Type)
	}
}

func (f *Fetcher) get(ctx context.Context, rawURL string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, classifyTransportError(err, rawURL)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, classifyStatus(resp.StatusCode)
	}
	return body, nil
}

func classifyTransportError(err error, rawURL string) error {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("request timed out after 10s: %s", rawURL)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return fmt.Errorf("DNS resolution failed for %s", rawURL)
	}
	return fmt.Errorf("connection failed for %s: %w", rawURL, err)
}

func classifyStatus(code int) error {
	switch {
	case code == http.StatusUnauthorized, code == http.StatusForbidden:
		return fmt.Errorf("HTTP %d — authentication/authorization rejected", code)
	case code == http.StatusNotFound:
		return errors.New("HTTP 404 — endpoint not found (check URL/datasource UID)")
	case code == http.StatusTooManyRequests:
		return errors.New("HTTP 429 — rate limited")
	case code >= 500:
		return fmt.Errorf("HTTP %d — server error", code)
	default:
		return fmt.Errorf("HTTP %d", code)
	}
}

type promResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		Result []struct {
			Value  []json.RawMessage   `json:"value"`
			Values [][]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func parsePromResponse(body []byte) (float64, error) {
	var resp promResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, fmt.Errorf("response is not valid JSON (got %d bytes)", len(body))
	}

	if resp.Status != "success" {
		errType := resp.ErrorType
		if errType == "" {
			errType = "unknown"
		}
		errMsg := resp.Error
		if errMsg == "" {
			errMsg = "(no message)"
		}
		return 0, fmt.Errorf("API returned status=%s errorType=%s error=%q",
			resp.Status, errType, errMsg)
	}

	if len(resp.Data.Result) == 0 {
		return 0, errors.New("query matched no series (metric missing or label filters too narrow)")
	}

	sample, ok := sampleValue(resp.Data.Result[0].Value, resp.Data.Result[0].Values)
	if !ok {
		return 0, errors.New("series exists but has no sample value")
	}

	switch sample {
	case "NaN", "+Inf", "-Inf":
		return 0, fmt.Errorf("value is non-numeric (%s) — sensor likely stale or absent", sample)
	}

	v, err := strconv.ParseFloat(sample, 64)
	if err != nil {
		return 0, fmt.Errorf("value is not a number: %q", sample)
	}
	return v, nil
}

func parsePromRange(body []byte) ([]Sample, error) {
	var resp promResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("response is not valid JSON (got %d bytes)", len(body))
	}

	if resp.Status != "success" {
		errType := resp.ErrorType
		if errType == "" {
			errType = "unknown"
		}
		errMsg := resp.Error
		if errMsg == "" {
			errMsg = "(no message)"
		}
		return nil, fmt.Errorf("API returned status=%s errorType=%s error=%q",
			resp.Status, errType, errMsg)
	}

	if len(resp.Data.Result) == 0 {
		return nil, errors.New("query matched no series (metric missing or label filters too narrow)")
	}

	pairs := resp.Data.Result[0].Values
	if len(pairs) == 0 && len(resp.Data.Result[0].Value) == 2 {
		pairs = [][]json.RawMessage{resp.Data.Result[0].Value}
	}

	samples := make([]Sample, 0, len(pairs))
	for _, pair := range pairs {
		if len(pair) != 2 {
			continue
		}
		var ts float64
		if err := json.Unmarshal(pair[0], &ts); err != nil {
			continue
		}
		raw, ok := unquoteSample(pair)
		if !ok {
			continue
		}
		switch raw {
		case "NaN", "+Inf", "-Inf":
			continue
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			continue
		}
		samples = append(samples, Sample{T: ts, V: v})
	}

	if len(samples) == 0 {
		return nil, errors.New("series has no numeric samples in the window (sensor stale or absent)")
	}
	return samples, nil
}

func sampleValue(value []json.RawMessage, values [][]json.RawMessage) (string, bool) {
	if s, ok := unquoteSample(value); ok {
		return s, true
	}
	if n := len(values); n > 0 {
		if s, ok := unquoteSample(values[n-1]); ok {
			return s, true
		}
	}
	return "", false
}

func unquoteSample(pair []json.RawMessage) (string, bool) {
	if len(pair) != 2 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(pair[1], &s); err != nil || s == "" {
		return "", false
	}
	return s, true
}
