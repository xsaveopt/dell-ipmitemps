package datasource

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xsaveopt/dell-ipmitemps/internal/config"
)

type capture struct {
	path   string
	query  url.Values
	header http.Header
}

func promServer(t *testing.T, status int, body string, seen *capture) *Fetcher {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			seen.path = r.URL.Path
			seen.query = r.URL.Query()
			seen.header = r.Header.Clone()
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	var cfg config.Datasource
	cfg.Type = config.Prometheus
	cfg.Prometheus.URL = srv.URL
	return New(cfg)
}

func grafanaServer(t *testing.T, status int, body string, seen *capture) *Fetcher {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			seen.path = r.URL.Path
			seen.query = r.URL.Query()
			seen.header = r.Header.Clone()
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	var cfg config.Datasource
	cfg.Type = config.Grafana
	cfg.Grafana.URL = srv.URL
	cfg.Grafana.Token = "secret-token"
	cfg.Grafana.DatasourceUID = "abc123"
	return New(cfg)
}

func instantBody(value string) string {
	return fmt.Sprintf(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"%s"]}]}}`, value)
}

func TestFetchTempPrometheus(t *testing.T) {
	var seen capture
	f := promServer(t, http.StatusOK, instantBody("42.5"), &seen)

	got, err := f.FetchTemp(context.Background(), `node_temp{chip="cpu"}`)
	if err != nil {
		t.Fatalf("FetchTemp: %v", err)
	}
	if got != 42.5 {
		t.Errorf("temp = %v, want 42.5", got)
	}
	if seen.path != "/api/v1/query" {
		t.Errorf("path = %q, want /api/v1/query", seen.path)
	}
	if q := seen.query.Get("query"); q != `node_temp{chip="cpu"}` {
		t.Errorf("query = %q, want it URL-decoded back to the original", q)
	}
}

func TestFetchTempGrafanaProxiesWithToken(t *testing.T) {
	var seen capture
	f := grafanaServer(t, http.StatusOK, instantBody("37"), &seen)

	got, err := f.FetchTemp(context.Background(), "cpu_temp")
	if err != nil {
		t.Fatalf("FetchTemp: %v", err)
	}
	if got != 37 {
		t.Errorf("temp = %v, want 37", got)
	}
	if want := "/api/datasources/proxy/uid/abc123/api/v1/query"; seen.path != want {
		t.Errorf("path = %q, want %q", seen.path, want)
	}
	if got := seen.header.Get("Authorization"); got != "Bearer secret-token" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestFetchTempUnknownType(t *testing.T) {
	f := New(config.Datasource{Type: "influx"})
	_, err := f.FetchTemp(context.Background(), "q")
	if err == nil || !strings.Contains(err.Error(), "unknown datasource type") {
		t.Fatalf("err = %v, want an unknown datasource type error", err)
	}
}

func TestFetchRangeUnknownType(t *testing.T) {
	f := New(config.Datasource{Type: ""})
	_, err := f.FetchRange(context.Background(), "q", time.Minute, time.Second)
	if err == nil || !strings.Contains(err.Error(), "unknown datasource type") {
		t.Fatalf("err = %v, want an unknown datasource type error", err)
	}
}

func TestFetchTempStatusClassification(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "authentication/authorization rejected"},
		{http.StatusForbidden, "authentication/authorization rejected"},
		{http.StatusNotFound, "endpoint not found"},
		{http.StatusTooManyRequests, "rate limited"},
		{http.StatusInternalServerError, "server error"},
		{http.StatusBadGateway, "server error"},
		{http.StatusTeapot, "HTTP 418"},
		{http.StatusBadRequest, "HTTP 400"},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			f := promServer(t, tc.status, `{"status":"error"}`, nil)
			_, err := f.FetchTemp(context.Background(), "q")
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestFetchTempStatusIsCheckedForRangeToo(t *testing.T) {
	f := promServer(t, http.StatusServiceUnavailable, "", nil)
	_, err := f.FetchRange(context.Background(), "q", time.Minute, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "server error") {
		t.Fatalf("err = %v, want a server error", err)
	}
}

func TestFetchTempBodyErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "not json",
			body: "<html>bad gateway</html>",
			want: "not valid JSON",
		},
		{
			name: "api error with detail",
			body: `{"status":"error","errorType":"bad_data","error":"parse error"}`,
			want: `errorType=bad_data error="parse error"`,
		},
		{
			name: "api error without detail",
			body: `{"status":"error"}`,
			want: `errorType=unknown error="(no message)"`,
		},
		{
			name: "empty result",
			body: `{"status":"success","data":{"resultType":"vector","result":[]}}`,
			want: "matched no series",
		},
		{
			name: "series without a value",
			body: `{"status":"success","data":{"result":[{"metric":{}}]}}`,
			want: "no sample value",
		},
		{
			name: "NaN sample",
			body: instantBody("NaN"),
			want: "non-numeric (NaN)",
		},
		{
			name: "positive infinity",
			body: instantBody("+Inf"),
			want: "non-numeric (+Inf)",
		},
		{
			name: "negative infinity",
			body: instantBody("-Inf"),
			want: "non-numeric (-Inf)",
		},
		{
			name: "non-numeric string",
			body: instantBody("warm"),
			want: `not a number: "warm"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := promServer(t, http.StatusOK, tc.body, nil)
			_, err := f.FetchTemp(context.Background(), "q")
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestFetchTempAcceptsNegativeAndScientificValues(t *testing.T) {
	cases := []struct {
		raw  string
		want float64
	}{
		{"0", 0},
		{"-5.5", -5.5},
		{"1e2", 100},
		{"93.750000001", 93.750000001},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			f := promServer(t, http.StatusOK, instantBody(tc.raw), nil)
			got, err := f.FetchTemp(context.Background(), "q")
			if err != nil {
				t.Fatalf("FetchTemp: %v", err)
			}
			if got != tc.want {
				t.Errorf("temp = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFetchTempFallsBackToLastRangeSample(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1,"10"],[2,"20"],[3,"30"]]}]}}`
	f := promServer(t, http.StatusOK, body, nil)
	got, err := f.FetchTemp(context.Background(), "q")
	if err != nil {
		t.Fatalf("FetchTemp: %v", err)
	}
	if got != 30 {
		t.Errorf("temp = %v, want the newest sample 30", got)
	}
}

func TestFetchRangeParsesMatrix(t *testing.T) {
	var seen capture
	body := `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1700000000,"40.5"],[1700000010,"41"],[1700000020,"42"]]}]}}`
	f := promServer(t, http.StatusOK, body, &seen)

	got, err := f.FetchRange(context.Background(), "cpu_temp", 2*time.Minute, 10*time.Second)
	if err != nil {
		t.Fatalf("FetchRange: %v", err)
	}
	want := []Sample{
		{T: 1700000000, V: 40.5},
		{T: 1700000010, V: 41},
		{T: 1700000020, V: 42},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d samples, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sample %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if seen.path != "/api/v1/query_range" {
		t.Errorf("path = %q, want /api/v1/query_range", seen.path)
	}
	if seen.query.Get("step") != "10" {
		t.Errorf("step = %q, want 10", seen.query.Get("step"))
	}
	start, err := strconv.ParseInt(seen.query.Get("start"), 10, 64)
	if err != nil {
		t.Fatalf("start = %q: %v", seen.query.Get("start"), err)
	}
	end, err := strconv.ParseInt(seen.query.Get("end"), 10, 64)
	if err != nil {
		t.Fatalf("end = %q: %v", seen.query.Get("end"), err)
	}
	if end-start != 120 {
		t.Errorf("window = %ds, want the 120s lookback", end-start)
	}
}

func TestFetchRangeSubSecondStep(t *testing.T) {
	var seen capture
	body := `{"status":"success","data":{"result":[{"values":[[1,"20"]]}]}}`
	f := promServer(t, http.StatusOK, body, &seen)
	if _, err := f.FetchRange(context.Background(), "q", time.Minute, 500*time.Millisecond); err != nil {
		t.Fatalf("FetchRange: %v", err)
	}
	if seen.query.Get("step") != "0.5" {
		t.Errorf("step = %q, want 0.5", seen.query.Get("step"))
	}
}

func TestFetchRangeGrafanaProxiesWithToken(t *testing.T) {
	var seen capture
	body := `{"status":"success","data":{"result":[{"values":[[1,"20"],[2,"21"]]}]}}`
	f := grafanaServer(t, http.StatusOK, body, &seen)

	got, err := f.FetchRange(context.Background(), "q", time.Minute, 10*time.Second)
	if err != nil {
		t.Fatalf("FetchRange: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d samples, want 2", len(got))
	}
	if want := "/api/datasources/proxy/uid/abc123/api/v1/query_range"; seen.path != want {
		t.Errorf("path = %q, want %q", seen.path, want)
	}
	if got := seen.header.Get("Authorization"); got != "Bearer secret-token" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestFetchRangeFallsBackToInstantValue(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"55"]}]}}`
	f := promServer(t, http.StatusOK, body, nil)
	got, err := f.FetchRange(context.Background(), "q", time.Minute, 10*time.Second)
	if err != nil {
		t.Fatalf("FetchRange: %v", err)
	}
	if len(got) != 1 || got[0].V != 55 || got[0].T != 1700000000 {
		t.Errorf("samples = %+v, want the single instant value", got)
	}
}

func TestFetchRangeSkipsUnusableSamples(t *testing.T) {
	body := `{"status":"success","data":{"result":[{"values":[
		[1,"40"],
		[2,"NaN"],
		[3,"+Inf"],
		[4,"-Inf"],
		[5,"warm"],
		[6],
		["notatime","44"],
		[7,"45"]
	]}]}}`
	f := promServer(t, http.StatusOK, body, nil)
	got, err := f.FetchRange(context.Background(), "q", time.Minute, 10*time.Second)
	if err != nil {
		t.Fatalf("FetchRange: %v", err)
	}
	want := []Sample{{T: 1, V: 40}, {T: 7, V: 45}}
	if len(got) != len(want) {
		t.Fatalf("samples = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sample %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestFetchRangeBodyErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "not json", body: "nope", want: "not valid JSON"},
		{
			name: "api error",
			body: `{"status":"error","errorType":"timeout","error":"query timed out"}`,
			want: `errorType=timeout error="query timed out"`,
		},
		{
			name: "api error without detail",
			body: `{"status":"error"}`,
			want: `errorType=unknown error="(no message)"`,
		},
		{
			name: "no series",
			body: `{"status":"success","data":{"result":[]}}`,
			want: "matched no series",
		},
		{
			name: "all samples unusable",
			body: `{"status":"success","data":{"result":[{"values":[[1,"NaN"],[2,"NaN"]]}]}}`,
			want: "no numeric samples in the window",
		},
		{
			name: "series present but empty",
			body: `{"status":"success","data":{"result":[{"metric":{}}]}}`,
			want: "no numeric samples in the window",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := promServer(t, http.StatusOK, tc.body, nil)
			_, err := f.FetchRange(context.Background(), "q", time.Minute, 10*time.Second)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestFetchTempConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close()

	var cfg config.Datasource
	cfg.Type = config.Prometheus
	cfg.Prometheus.URL = addr
	f := New(cfg)

	_, err := f.FetchTemp(context.Background(), "q")
	if err == nil {
		t.Fatal("want an error against a closed listener")
	}
	if !strings.Contains(err.Error(), "connection failed for") {
		t.Errorf("error = %q, want it classified as a connection failure", err)
	}
	if !strings.Contains(err.Error(), addr) {
		t.Errorf("error = %q, want it to name the URL", err)
	}
}

func TestFetchTempCancelledContext(t *testing.T) {
	f := promServer(t, http.StatusOK, instantBody("40"), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.FetchTemp(ctx, "q"); err == nil {
		t.Fatal("want an error for a cancelled context")
	}
}

type fakeTimeout struct{}

func (fakeTimeout) Error() string   { return "i/o timeout" }
func (fakeTimeout) Timeout() bool   { return true }
func (fakeTimeout) Temporary() bool { return false }

func TestClassifyTransportError(t *testing.T) {
	const raw = "http://prom:9090/api/v1/query"
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "timeout",
			err:  &url.Error{Op: "Get", URL: raw, Err: fakeTimeout{}},
			want: "request timed out after 10s",
		},
		{
			name: "dns",
			err:  &url.Error{Op: "Get", URL: raw, Err: &net.DNSError{Err: "no such host", Name: "prom"}},
			want: "DNS resolution failed for",
		},
		{
			name: "other",
			err:  errors.New("connection reset by peer"),
			want: "connection failed for",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyTransportError(tc.err, raw)
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), raw) {
				t.Errorf("error = %q, want it to name the URL", err)
			}
		})
	}
}

func TestClassifyTransportErrorUnwrapsOther(t *testing.T) {
	inner := errors.New("boom")
	err := classifyTransportError(inner, "http://x")
	if !errors.Is(err, inner) {
		t.Error("the underlying cause must stay unwrappable")
	}
}
