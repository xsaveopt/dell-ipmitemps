package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const minimalYAML = `
datasource:
  type: prometheus
  prometheus:
    url: http://prom:9090
ipmi:
  local: true
sensors:
  - name: cpu
    query: cpu_temp
    weight: 1
fan_curve:
  - temp: 30
    percent: 10
  - temp: 80
    percent: 100
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func validConfig() *Config {
	return &Config{
		Datasource: Datasource{Type: Prometheus},
		IPMI:       IPMI{Local: true, User: "root"},
		Sensors:    []Sensor{{Name: "cpu", Query: "cpu_temp", Weight: 1}},
		FanCurve: []CurvePoint{
			{Temp: 30, Percent: 10},
			{Temp: 80, Percent: 100},
		},
		TempCritical:         90,
		PollInterval:         30,
		MaxFetchFailures:     3,
		FetchFailureFanSpeed: 100,
	}
}

func TestLoadMinimal(t *testing.T) {
	c, err := Load(writeConfig(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Datasource.Type != Prometheus {
		t.Errorf("type = %q, want prometheus", c.Datasource.Type)
	}
	if c.Datasource.Prometheus.URL != "http://prom:9090" {
		t.Errorf("url = %q", c.Datasource.Prometheus.URL)
	}
	if len(c.Sensors) != 1 || c.Sensors[0].Name != "cpu" {
		t.Errorf("sensors = %+v", c.Sensors)
	}
	if c.PollInterval != 30 || c.MaxFetchFailures != 3 || c.FetchFailureFanSpeed != 100 {
		t.Errorf("defaults not applied: %+v", c)
	}
}

func TestLoadMissingFileIsExplicit(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil {
		t.Fatal("want an error for a missing config")
	}
	if !strings.Contains(err.Error(), "config not found") {
		t.Errorf("error = %q, want it to say the config was not found", err)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	_, err := Load(writeConfig(t, minimalYAML+"\nnot_a_real_key: 1\n"))
	if err == nil {
		t.Fatal("want an error for an unknown field")
	}
	if !strings.Contains(err.Error(), "not_a_real_key") {
		t.Errorf("error = %q, want it to name the unknown field", err)
	}
}

func TestLoadRejectsMalformedYAML(t *testing.T) {
	if _, err := Load(writeConfig(t, "datasource: [unterminated\n")); err == nil {
		t.Fatal("want an error for malformed YAML")
	}
}

func TestLoadSurfacesValidationErrors(t *testing.T) {
	body := strings.Replace(minimalYAML, "    url: http://prom:9090", "    url: \"\"", 1)
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("want a validation error")
	}
	if !strings.Contains(err.Error(), "invalid config") {
		t.Errorf("error = %q, want it wrapped as an invalid config", err)
	}
}

func TestLoadParsesDurations(t *testing.T) {
	body := minimalYAML + `
prediction:
  enabled: true
  lookback: 5m
  step: 15s
`
	c, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Prediction.Lookback.Std(); got != 5*time.Minute {
		t.Errorf("lookback = %v, want 5m", got)
	}
	if got := c.Prediction.Step.Std(); got != 15*time.Second {
		t.Errorf("step = %v, want 15s", got)
	}
}

func TestDurationUnmarshalYAML(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{name: "seconds", in: "d: 30s", want: 30 * time.Second},
		{name: "minutes", in: "d: 2m", want: 2 * time.Minute},
		{name: "compound", in: "d: 1h30m", want: 90 * time.Minute},
		{name: "fractional", in: "d: 1.5s", want: 1500 * time.Millisecond},
		{name: "zero", in: "d: 0s", want: 0},
		{name: "negative", in: "d: -5s", want: -5 * time.Second},
		{name: "quoted", in: `d: "45s"`, want: 45 * time.Second},
		{name: "unitless", in: "d: 30", wantErr: true},
		{name: "garbage", in: "d: banana", wantErr: true},
		{name: "empty", in: `d: ""`, wantErr: true},
		{name: "sequence", in: "d: [1s]", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var holder struct {
				D Duration `yaml:"d"`
			}
			err := yaml.Unmarshal([]byte(tc.in), &holder)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error for %q", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unmarshal %q: %v", tc.in, err)
			}
			if got := holder.D.Std(); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDurationInvalidErrorNamesTheValue(t *testing.T) {
	var holder struct {
		D Duration `yaml:"d"`
	}
	err := yaml.Unmarshal([]byte("d: banana"), &holder)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "banana") {
		t.Errorf("error = %q, want it to quote the offending value", err)
	}
}

func TestApplyDefaultsTopLevel(t *testing.T) {
	var c Config
	c.applyDefaults()
	if c.PollInterval != 30 {
		t.Errorf("poll_interval = %d, want 30", c.PollInterval)
	}
	if c.MaxFetchFailures != 3 {
		t.Errorf("max_fetch_failures = %d, want 3", c.MaxFetchFailures)
	}
	if c.FetchFailureFanSpeed != 100 {
		t.Errorf("fetch_failure_fan_speed = %d, want 100", c.FetchFailureFanSpeed)
	}
	if c.TempCritical != 90 {
		t.Errorf("temp_critical = %g, want 90", c.TempCritical)
	}
	if c.IPMI.User != "root" {
		t.Errorf("ipmi.user = %q, want root", c.IPMI.User)
	}
}

func TestApplyDefaultsKeepsExplicitValues(t *testing.T) {
	c := Config{
		TempCritical:         70,
		PollInterval:         5,
		MaxFetchFailures:     9,
		FetchFailureFanSpeed: 60,
		IPMI:                 IPMI{User: "admin"},
	}
	c.applyDefaults()
	if c.TempCritical != 70 || c.PollInterval != 5 || c.MaxFetchFailures != 9 ||
		c.FetchFailureFanSpeed != 60 || c.IPMI.User != "admin" {
		t.Errorf("explicit values were overwritten: %+v", c)
	}
}

func TestApplyDefaultsSkipsDisabledSections(t *testing.T) {
	var c Config
	c.applyDefaults()
	if c.Prediction.Lookback != 0 || c.Prediction.Quantile != 0 {
		t.Errorf("prediction defaults applied while disabled: %+v", c.Prediction)
	}
	if c.Smoothing.Deadband != 0 || c.Smoothing.MaxStepDown != 0 {
		t.Errorf("smoothing defaults applied while disabled: %+v", c.Smoothing)
	}
	if c.ProcessPrediction.ProcPath != "" || c.ProcessPrediction.Decay != 0 {
		t.Errorf("process_prediction defaults applied while disabled: %+v", c.ProcessPrediction)
	}
}

func TestApplyDefaultsPrediction(t *testing.T) {
	c := Config{Prediction: Prediction{Enabled: true}}
	c.applyDefaults()
	p := c.Prediction
	if p.Lookback.Std() != 120*time.Second {
		t.Errorf("lookback = %v, want 2m", p.Lookback.Std())
	}
	if p.Step.Std() != 10*time.Second {
		t.Errorf("step = %v, want 10s", p.Step.Std())
	}
	if p.Quantile != 0.6 {
		t.Errorf("quantile = %g, want 0.6", p.Quantile)
	}
	if p.TrendHorizon.Std() != 30*time.Second {
		t.Errorf("trend_horizon = %v, want 30s", p.TrendHorizon.Std())
	}
	if p.CriticalDwell.Std() != 15*time.Second {
		t.Errorf("critical_dwell = %v, want 15s", p.CriticalDwell.Std())
	}
}

func TestApplyDefaultsSmoothingUrgentTempTracksCritical(t *testing.T) {
	c := Config{TempCritical: 75, Smoothing: Smoothing{Enabled: true}}
	c.applyDefaults()
	if c.Smoothing.UrgentTemp != 65 {
		t.Errorf("urgent_temp = %g, want temp_critical minus 10", c.Smoothing.UrgentTemp)
	}
	if c.Smoothing.Deadband != 3 || c.Smoothing.MaxStepUp != 100 || c.Smoothing.MaxStepDown != 5 {
		t.Errorf("smoothing defaults = %+v", c.Smoothing)
	}
}

func TestApplyDefaultsSmoothingUrgentTempUsesDefaultedCritical(t *testing.T) {
	c := Config{Smoothing: Smoothing{Enabled: true}}
	c.applyDefaults()
	if c.Smoothing.UrgentTemp != 80 {
		t.Errorf("urgent_temp = %g, want 80 from the defaulted temp_critical", c.Smoothing.UrgentTemp)
	}
}

func TestApplyDefaultsProcessPrediction(t *testing.T) {
	c := Config{ProcessPrediction: ProcessPrediction{Enabled: true}}
	c.applyDefaults()
	pp := c.ProcessPrediction
	if pp.ProcPath != "/proc" {
		t.Errorf("proc_path = %q", pp.ProcPath)
	}
	if pp.ModelPath != "/var/lib/dellipmifanctl/procmodel.json" {
		t.Errorf("model_path = %q", pp.ModelPath)
	}
	if pp.MinObservations != 3 || pp.ImpactThreshold != 3.0 || pp.HoldMargin != 1.25 || pp.Decay != 0.3 {
		t.Errorf("process_prediction defaults = %+v", pp)
	}
	if pp.PreemptTTL.Std() != 60*time.Second {
		t.Errorf("preempt_ttl = %v, want 60s", pp.PreemptTTL.Std())
	}
}

func TestApplyDefaultsThenValidateIsConsistent(t *testing.T) {
	c := Config{
		Datasource: Datasource{Type: Prometheus},
		IPMI:       IPMI{Local: true},
		Sensors:    []Sensor{{Name: "cpu", Query: "q", Weight: 1}},
		FanCurve:   []CurvePoint{{Temp: 30, Percent: 10}},

		Prediction:        Prediction{Enabled: true},
		Smoothing:         Smoothing{Enabled: true},
		ProcessPrediction: ProcessPrediction{Enabled: true},
	}
	c.Datasource.Prometheus.URL = "http://prom:9090"
	c.applyDefaults()
	if err := c.validate(); err != nil {
		t.Fatalf("defaults must produce a valid config, got %v", err)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "valid prometheus", mutate: func(*Config) {}},
		{
			name: "valid grafana",
			mutate: func(c *Config) {
				c.Datasource.Type = Grafana
				c.Datasource.Grafana.URL = "http://graf:3000"
				c.Datasource.Grafana.Token = "tok"
				c.Datasource.Grafana.DatasourceUID = "uid"
			},
		},
		{
			name:    "missing datasource type",
			mutate:  func(c *Config) { c.Datasource.Type = "" },
			wantErr: "datasource.type is required",
		},
		{
			name:    "unknown datasource type",
			mutate:  func(c *Config) { c.Datasource.Type = "influx" },
			wantErr: "unknown datasource.type",
		},
		{
			name:    "prometheus without url",
			mutate:  func(c *Config) { c.Datasource.Prometheus.URL = "" },
			wantErr: "datasource.prometheus.url is required",
		},
		{
			name: "grafana missing token",
			mutate: func(c *Config) {
				c.Datasource.Type = Grafana
				c.Datasource.Grafana.URL = "http://graf:3000"
				c.Datasource.Grafana.DatasourceUID = "uid"
			},
			wantErr: "datasource.grafana requires",
		},
		{
			name:    "remote ipmi without host",
			mutate:  func(c *Config) { c.IPMI.Local = false; c.IPMI.Host = "" },
			wantErr: "ipmi.host is required",
		},
		{
			name:   "remote ipmi with host",
			mutate: func(c *Config) { c.IPMI.Local = false; c.IPMI.Host = "bmc.local" },
		},
		{
			name:    "no sensors",
			mutate:  func(c *Config) { c.Sensors = nil },
			wantErr: "at least one entry under sensors",
		},
		{
			name:    "sensor without name",
			mutate:  func(c *Config) { c.Sensors[0].Name = "" },
			wantErr: "sensors[0]: name and query are required",
		},
		{
			name:    "sensor without query",
			mutate:  func(c *Config) { c.Sensors[0].Query = "" },
			wantErr: "sensors[0]: name and query are required",
		},
		{
			name:    "sensor zero weight",
			mutate:  func(c *Config) { c.Sensors[0].Weight = 0 },
			wantErr: "weight must be greater than 0",
		},
		{
			name:    "sensor negative weight",
			mutate:  func(c *Config) { c.Sensors[0].Weight = -1 },
			wantErr: "weight must be greater than 0",
		},
		{
			name:    "empty fan curve",
			mutate:  func(c *Config) { c.FanCurve = nil },
			wantErr: "at least one fan_curve breakpoint",
		},
		{
			name:    "fan curve percent above range",
			mutate:  func(c *Config) { c.FanCurve[1].Percent = 101 },
			wantErr: "out of range 0-100",
		},
		{
			name:    "fan curve percent below range",
			mutate:  func(c *Config) { c.FanCurve[0].Percent = -1 },
			wantErr: "out of range 0-100",
		},
		{
			name:    "fan curve not ascending",
			mutate:  func(c *Config) { c.FanCurve[1].Temp = 20 },
			wantErr: "must be greater than the previous breakpoint",
		},
		{
			name:    "fan curve duplicate temp",
			mutate:  func(c *Config) { c.FanCurve[1].Temp = c.FanCurve[0].Temp },
			wantErr: "must be greater than the previous breakpoint",
		},
		{
			name:    "min fan speed negative",
			mutate:  func(c *Config) { c.MinFanSpeed = -1 },
			wantErr: "min_fan_speed",
		},
		{
			name:    "min fan speed above 100",
			mutate:  func(c *Config) { c.MinFanSpeed = 101 },
			wantErr: "min_fan_speed",
		},
		{
			name:    "poll interval zero",
			mutate:  func(c *Config) { c.PollInterval = 0 },
			wantErr: "poll_interval must be greater than 0",
		},
		{
			name:    "poll interval negative",
			mutate:  func(c *Config) { c.PollInterval = -5 },
			wantErr: "poll_interval must be greater than 0",
		},
		{
			name:    "max fetch failures zero",
			mutate:  func(c *Config) { c.MaxFetchFailures = 0 },
			wantErr: "max_fetch_failures must be greater than 0",
		},
		{
			name:    "fail-safe speed zero is rejected",
			mutate:  func(c *Config) { c.FetchFailureFanSpeed = 0 },
			wantErr: "fetch_failure_fan_speed",
		},
		{
			name:    "fail-safe speed above 100",
			mutate:  func(c *Config) { c.FetchFailureFanSpeed = 101 },
			wantErr: "fetch_failure_fan_speed",
		},
		{
			name: "prediction lookback zero",
			mutate: func(c *Config) {
				c.Prediction = Prediction{Enabled: true, Quantile: 0.6}
			},
			wantErr: "prediction.lookback must be greater than 0",
		},
		{
			name: "prediction step exceeds lookback",
			mutate: func(c *Config) {
				c.Prediction = Prediction{
					Enabled:  true,
					Lookback: Duration(60 * time.Second),
					Step:     Duration(120 * time.Second),
					Quantile: 0.6,
				}
			},
			wantErr: "prediction.step",
		},
		{
			name: "prediction quantile above one",
			mutate: func(c *Config) {
				c.Prediction = Prediction{
					Enabled:  true,
					Lookback: Duration(60 * time.Second),
					Step:     Duration(10 * time.Second),
					Quantile: 1.5,
				}
			},
			wantErr: "prediction.quantile",
		},
		{
			name: "prediction negative trend horizon",
			mutate: func(c *Config) {
				c.Prediction = Prediction{
					Enabled:      true,
					Lookback:     Duration(60 * time.Second),
					Step:         Duration(10 * time.Second),
					Quantile:     0.6,
					TrendHorizon: Duration(-time.Second),
				}
			},
			wantErr: "prediction.trend_horizon",
		},
		{
			name: "prediction negative critical dwell",
			mutate: func(c *Config) {
				c.Prediction = Prediction{
					Enabled:       true,
					Lookback:      Duration(60 * time.Second),
					Step:          Duration(10 * time.Second),
					Quantile:      0.6,
					CriticalDwell: Duration(-time.Second),
				}
			},
			wantErr: "prediction.critical_dwell",
		},
		{
			name: "prediction disabled skips its checks",
			mutate: func(c *Config) {
				c.Prediction = Prediction{Quantile: 99}
			},
		},
		{
			name: "smoothing deadband out of range",
			mutate: func(c *Config) {
				c.Smoothing = Smoothing{Enabled: true, Deadband: 51, MaxStepUp: 10, MaxStepDown: 5, UrgentTemp: 80}
			},
			wantErr: "smoothing.deadband",
		},
		{
			name: "smoothing max step up zero",
			mutate: func(c *Config) {
				c.Smoothing = Smoothing{Enabled: true, Deadband: 3, MaxStepUp: 0, MaxStepDown: 5, UrgentTemp: 80}
			},
			wantErr: "smoothing.max_step_up",
		},
		{
			name: "smoothing max step down zero",
			mutate: func(c *Config) {
				c.Smoothing = Smoothing{Enabled: true, Deadband: 3, MaxStepUp: 10, MaxStepDown: 0, UrgentTemp: 80}
			},
			wantErr: "smoothing.max_step_down",
		},
		{
			name: "smoothing urgent temp zero",
			mutate: func(c *Config) {
				c.Smoothing = Smoothing{Enabled: true, Deadband: 3, MaxStepUp: 10, MaxStepDown: 5}
			},
			wantErr: "smoothing.urgent_temp must be greater than 0",
		},
		{
			name: "smoothing urgent temp above critical",
			mutate: func(c *Config) {
				c.Smoothing = Smoothing{Enabled: true, Deadband: 3, MaxStepUp: 10, MaxStepDown: 5, UrgentTemp: 95}
			},
			wantErr: "must not exceed temp_critical",
		},
		{
			name: "smoothing urgent temp equal to critical",
			mutate: func(c *Config) {
				c.Smoothing = Smoothing{Enabled: true, Deadband: 3, MaxStepUp: 10, MaxStepDown: 5, UrgentTemp: 90}
			},
		},
		{
			name: "process prediction missing paths",
			mutate: func(c *Config) {
				c.ProcessPrediction = ProcessPrediction{Enabled: true}
			},
			wantErr: "process_prediction requires proc_path and model_path",
		},
		{
			name: "process prediction min observations zero",
			mutate: func(c *Config) {
				pp := validProcessPrediction()
				pp.MinObservations = 0
				c.ProcessPrediction = pp
			},
			wantErr: "min_observations",
		},
		{
			name: "process prediction impact threshold zero",
			mutate: func(c *Config) {
				pp := validProcessPrediction()
				pp.ImpactThreshold = 0
				c.ProcessPrediction = pp
			},
			wantErr: "impact_threshold",
		},
		{
			name: "process prediction preempt ttl zero",
			mutate: func(c *Config) {
				pp := validProcessPrediction()
				pp.PreemptTTL = 0
				c.ProcessPrediction = pp
			},
			wantErr: "preempt_ttl",
		},
		{
			name: "process prediction hold margin below one",
			mutate: func(c *Config) {
				pp := validProcessPrediction()
				pp.HoldMargin = 0.5
				c.ProcessPrediction = pp
			},
			wantErr: "hold_margin",
		},
		{
			name: "process prediction decay above one",
			mutate: func(c *Config) {
				pp := validProcessPrediction()
				pp.Decay = 1.5
				c.ProcessPrediction = pp
			},
			wantErr: "decay",
		},
		{
			name: "process prediction valid",
			mutate: func(c *Config) {
				c.ProcessPrediction = validProcessPrediction()
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.Datasource.Prometheus.URL = "http://prom:9090"
			tc.mutate(c)

			err := c.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func validProcessPrediction() ProcessPrediction {
	return ProcessPrediction{
		Enabled:         true,
		ProcPath:        "/proc",
		ModelPath:       "/var/lib/dellipmifanctl/procmodel.json",
		MinObservations: 3,
		ImpactThreshold: 3,
		PreemptTTL:      Duration(60 * time.Second),
		HoldMargin:      1.25,
		Decay:           0.3,
	}
}
