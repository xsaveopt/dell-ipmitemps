package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"

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

	TempCritical     float64 `yaml:"temp_critical"`
	MinFanSpeed      int     `yaml:"min_fan_speed"`
	PollInterval     int     `yaml:"poll_interval"`
	MaxFetchFailures int     `yaml:"max_fetch_failures"`
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
	if c.TempCritical == 0 {
		c.TempCritical = 90
	}
	if c.IPMI.User == "" {
		c.IPMI.User = "root"
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
	return nil
}
