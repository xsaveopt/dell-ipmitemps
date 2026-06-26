package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Type string

const (
	Prometheus Type = "prometheus"
	Grafana    Type = "grafana"
)

type Config struct {
	Datasource Datasource   `yaml:"datasource"`
	IPMI       IPMI         `yaml:"ipmi"`
	Sensors    []Sensor     `yaml:"sensors"`
	FanCurve   []CurvePoint `yaml:"fan_curve"`

	DisablePCIeCoolingResponse bool `yaml:"disable_pcie_cooling_response"`

	TempCritical         float64 `yaml:"temp_critical"`
	MinFanSpeed          int     `yaml:"min_fan_speed"`
	PollInterval         int     `yaml:"poll_interval"`
	MaxFetchFailures     int     `yaml:"max_fetch_failures"`
	FetchFailureFanSpeed int     `yaml:"fetch_failure_fan_speed"`

	Prediction        Prediction        `yaml:"prediction"`
	ProcessPrediction ProcessPrediction `yaml:"process_prediction"`
}

type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

type Prediction struct {
	Enabled       bool     `yaml:"enabled"`
	Lookback      Duration `yaml:"lookback"`
	Step          Duration `yaml:"step"`
	Quantile      float64  `yaml:"quantile"`
	TrendHorizon  Duration `yaml:"trend_horizon"`
	CriticalDwell Duration `yaml:"critical_dwell"`
}

type ProcessPrediction struct {
	Enabled         bool     `yaml:"enabled"`
	ProcPath        string   `yaml:"proc_path"`
	ModelPath       string   `yaml:"model_path"`
	MinObservations int      `yaml:"min_observations"`
	ImpactThreshold float64  `yaml:"impact_threshold"`
	PreemptTTL      Duration `yaml:"preempt_ttl"`
	HoldMargin      float64  `yaml:"hold_margin"`
	Decay           float64  `yaml:"decay"`
}

type Datasource struct {
	Type       Type `yaml:"type"`
	Prometheus struct {
		URL string `yaml:"url"`
	} `yaml:"prometheus"`
	Grafana struct {
		URL           string `yaml:"url"`
		Token         string `yaml:"token"`
		DatasourceUID string `yaml:"datasource_uid"`
	} `yaml:"grafana"`
}

type IPMI struct {
	Local bool   `yaml:"local"`
	Host  string `yaml:"host"`
	User  string `yaml:"user"`
	Pass  string `yaml:"pass"`
}

type Sensor struct {
	Name   string  `yaml:"name"`
	Query  string  `yaml:"query"`
	Weight float64 `yaml:"weight"`
}

type CurvePoint struct {
	Temp    float64 `yaml:"temp"`
	Percent int     `yaml:"percent"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("config not found: %s — copy config.yaml.example there and edit it", path)
		}
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.PollInterval == 0 {
		c.PollInterval = 30
	}
	if c.MaxFetchFailures == 0 {
		c.MaxFetchFailures = 3
	}
	if c.FetchFailureFanSpeed == 0 {
		c.FetchFailureFanSpeed = 100
	}
	if c.TempCritical == 0 {
		c.TempCritical = 90
	}
	if c.IPMI.User == "" {
		c.IPMI.User = "root"
	}

	if c.Prediction.Enabled {
		if c.Prediction.Lookback == 0 {
			c.Prediction.Lookback = Duration(120 * time.Second)
		}
		if c.Prediction.Step == 0 {
			c.Prediction.Step = Duration(10 * time.Second)
		}
		if c.Prediction.Quantile == 0 {
			c.Prediction.Quantile = 0.9
		}
		if c.Prediction.TrendHorizon == 0 {
			c.Prediction.TrendHorizon = Duration(30 * time.Second)
		}
		if c.Prediction.CriticalDwell == 0 {
			c.Prediction.CriticalDwell = Duration(15 * time.Second)
		}
	}

	if c.ProcessPrediction.Enabled {
		if c.ProcessPrediction.ProcPath == "" {
			c.ProcessPrediction.ProcPath = "/proc"
		}
		if c.ProcessPrediction.ModelPath == "" {
			c.ProcessPrediction.ModelPath = "/var/lib/dellipmifanctl/procmodel.json"
		}
		if c.ProcessPrediction.MinObservations == 0 {
			c.ProcessPrediction.MinObservations = 3
		}
		if c.ProcessPrediction.ImpactThreshold == 0 {
			c.ProcessPrediction.ImpactThreshold = 3.0
		}
		if c.ProcessPrediction.PreemptTTL == 0 {
			c.ProcessPrediction.PreemptTTL = Duration(60 * time.Second)
		}
		if c.ProcessPrediction.HoldMargin == 0 {
			c.ProcessPrediction.HoldMargin = 1.25
		}
		if c.ProcessPrediction.Decay == 0 {
			c.ProcessPrediction.Decay = 0.3
		}
	}
}

func (c *Config) validate() error {
	switch c.Datasource.Type {
	case Prometheus:
		if c.Datasource.Prometheus.URL == "" {
			return errors.New("datasource.prometheus.url is required")
		}
	case Grafana:
		g := c.Datasource.Grafana
		if g.URL == "" || g.Token == "" || g.DatasourceUID == "" {
			return errors.New("datasource.grafana requires url, token, and datasource_uid")
		}
	case "":
		return errors.New("datasource.type is required (prometheus or grafana)")
	default:
		return fmt.Errorf("unknown datasource.type: %q", c.Datasource.Type)
	}

	if !c.IPMI.Local && c.IPMI.Host == "" {
		return errors.New("ipmi.host is required when ipmi.local is false")
	}

	if len(c.Sensors) == 0 {
		return errors.New("at least one entry under sensors is required")
	}
	for i, s := range c.Sensors {
		if s.Name == "" || s.Query == "" {
			return fmt.Errorf("sensors[%d]: name and query are required", i)
		}
		if s.Weight <= 0 {
			return fmt.Errorf("sensors[%d] %q: weight must be greater than 0", i, s.Name)
		}
	}

	if len(c.FanCurve) == 0 {
		return errors.New("at least one fan_curve breakpoint is required")
	}
	for i, p := range c.FanCurve {
		if p.Percent < 0 || p.Percent > 100 {
			return fmt.Errorf("fan_curve[%d]: percent %d out of range 0-100", i, p.Percent)
		}
		if i > 0 && p.Temp <= c.FanCurve[i-1].Temp {
			return fmt.Errorf("fan_curve[%d]: temp %g must be greater than the previous breakpoint", i, p.Temp)
		}
	}

	if c.MinFanSpeed < 0 || c.MinFanSpeed > 100 {
		return fmt.Errorf("min_fan_speed %d out of range 0-100", c.MinFanSpeed)
	}
	if c.PollInterval <= 0 {
		return fmt.Errorf("poll_interval must be greater than 0")
	}
	if c.MaxFetchFailures <= 0 {
		return fmt.Errorf("max_fetch_failures must be greater than 0")
	}
	if c.FetchFailureFanSpeed < 1 || c.FetchFailureFanSpeed > 100 {
		return fmt.Errorf("fetch_failure_fan_speed %d out of range 1-100", c.FetchFailureFanSpeed)
	}

	if c.Prediction.Enabled {
		p := c.Prediction
		if p.Lookback <= 0 {
			return errors.New("prediction.lookback must be greater than 0")
		}
		if p.Step <= 0 || p.Step > p.Lookback {
			return errors.New("prediction.step must be greater than 0 and not exceed prediction.lookback")
		}
		if p.Quantile <= 0 || p.Quantile > 1 {
			return fmt.Errorf("prediction.quantile %g out of range (0,1]", p.Quantile)
		}
		if p.TrendHorizon < 0 {
			return errors.New("prediction.trend_horizon must not be negative")
		}
		if p.CriticalDwell < 0 {
			return errors.New("prediction.critical_dwell must not be negative")
		}
	}

	if c.ProcessPrediction.Enabled {
		pp := c.ProcessPrediction
		if pp.ProcPath == "" || pp.ModelPath == "" {
			return errors.New("process_prediction requires proc_path and model_path")
		}
		if pp.MinObservations < 1 {
			return errors.New("process_prediction.min_observations must be at least 1")
		}
		if pp.ImpactThreshold <= 0 {
			return errors.New("process_prediction.impact_threshold must be greater than 0")
		}
		if pp.PreemptTTL <= 0 {
			return errors.New("process_prediction.preempt_ttl must be greater than 0")
		}
		if pp.HoldMargin < 1 {
			return errors.New("process_prediction.hold_margin must be at least 1")
		}
		if pp.Decay <= 0 || pp.Decay > 1 {
			return fmt.Errorf("process_prediction.decay %g out of range (0,1]", pp.Decay)
		}
	}
	return nil
}
