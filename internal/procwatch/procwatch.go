package procwatch

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/xsaveopt/dell-ipmitemps/internal/config"
	"github.com/xsaveopt/dell-ipmitemps/internal/curve"
)

const (
	recoverDelta = 1.0
	maxModelKeys = 512
	staleAge     = 30 * 24 * time.Hour
)

type Options struct {
	ProcPath        string
	ModelPath       string
	MinObservations int
	ImpactThreshold float64
	PreemptTTL      time.Duration
	HoldMargin      float64
	Decay           float64
}

type Stat struct {
	PeakRise       float64 `json:"peak_rise"`
	DurationSec    float64 `json:"duration_sec"`
	TransientScore float64 `json:"transient_score"`
	Observations   int     `json:"observations"`
	LastSeen       int64   `json:"last_seen"`
}

func (s *Stat) Transient() bool { return s.TransientScore >= 0.5 }

type Model map[string]*Stat

type observation struct {
	name     string
	baseline float64
	start    time.Time
	deadline time.Time
	peak     float64
	returned bool
	returnAt time.Time
}

type hint struct {
	name      string
	expires   time.Time
	hold      bool
	floor     int
	holdSpeed int
	envelope  float64
}

type Watcher struct {
	opts     Options
	points   []config.CurvePoint
	minSpeed int
	log      *slog.Logger

	model    Model
	prevPIDs map[int]string
	pending  []*observation
	hints    []*hint

	scanErrLogged bool
}

func New(opts Options, points []config.CurvePoint, minSpeed int, log *slog.Logger) *Watcher {
	w := &Watcher{
		opts:     opts,
		points:   points,
		minSpeed: minSpeed,
		log:      log,
		model:    Model{},
	}
	w.loadModel()
	return w
}

func Scan(procPath string) (map[int]string, error) {
	entries, err := os.ReadDir(procPath)
	if err != nil {
		return nil, err
	}
	pids := make(map[int]string, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(procPath, e.Name(), "comm"))
		if err != nil {
			continue
		}
		name := string(data)
		for len(name) > 0 && (name[len(name)-1] == '\n' || name[len(name)-1] == '\r') {
			name = name[:len(name)-1]
		}
		if name == "" {
			continue
		}
		pids[pid] = name
	}
	return pids, nil
}

func (w *Watcher) Poll(now time.Time, currentTemp float64) {
	pids, err := Scan(w.opts.ProcPath)
	if err != nil {
		if !w.scanErrLogged {
			w.log.Warn("process scan failed; pre-emption inactive", "path", w.opts.ProcPath, "err", err)
			w.scanErrLogged = true
		}
		return
	}
	w.scanErrLogged = false
	w.update(now, currentTemp, pids)
}

func (w *Watcher) update(now time.Time, currentTemp float64, pids map[int]string) {
	w.expireHints(now)

	dirty := w.advanceObservations(now, currentTemp)

	if w.prevPIDs != nil {
		pendingNames := w.pendingNames()
		for pid, name := range pids {
			if _, seen := w.prevPIDs[pid]; seen {
				continue
			}
			w.onSpawn(now, currentTemp, name, pendingNames)
		}
	}

	w.prevPIDs = pids

	if dirty {
		w.saveModel()
	}
}

func (w *Watcher) advanceObservations(now time.Time, currentTemp float64) bool {
	dirty := false
	kept := w.pending[:0]
	for _, o := range w.pending {
		if currentTemp > o.peak {
			o.peak = currentTemp
		}
		if !o.returned && currentTemp <= o.baseline+recoverDelta {
			o.returned = true
			o.returnAt = now
		}
		if now.Before(o.deadline) {
			kept = append(kept, o)
			continue
		}
		w.learn(o)
		dirty = true
	}
	w.pending = kept
	return dirty
}

func (w *Watcher) learn(o *observation) {
	rise := o.peak - o.baseline
	if rise < 0 {
		rise = 0
	}
	transient := 0.0
	duration := o.deadline.Sub(o.start).Seconds()
	if o.returned {
		transient = 1.0
		duration = o.returnAt.Sub(o.start).Seconds()
	}

	s := w.model[o.name]
	if s == nil {
		s = &Stat{}
		w.model[o.name] = s
	}
	d := w.opts.Decay
	if s.Observations == 0 {
		s.PeakRise = rise
		s.DurationSec = duration
		s.TransientScore = transient
	} else {
		s.PeakRise = d*rise + (1-d)*s.PeakRise
		s.DurationSec = d*duration + (1-d)*s.DurationSec
		s.TransientScore = d*transient + (1-d)*s.TransientScore
	}
	s.Observations++
	s.LastSeen = time.Now().Unix()

	w.log.Info("learned process thermal signature",
		"process", o.name, "peak_rise_c", s.PeakRise,
		"transient", s.Transient(), "duration_s", s.DurationSec, "observations", s.Observations)
}

func (w *Watcher) onSpawn(now time.Time, currentTemp float64, name string, pendingNames map[string]bool) {
	if !pendingNames[name] {
		w.pending = append(w.pending, &observation{
			name:     name,
			baseline: currentTemp,
			start:    now,
			deadline: now.Add(w.opts.PreemptTTL),
			peak:     currentTemp,
		})
		pendingNames[name] = true
	}

	s := w.model[name]
	if s == nil || s.Observations < w.opts.MinObservations || s.PeakRise < w.opts.ImpactThreshold {
		return
	}

	ttl := w.opts.PreemptTTL
	if learned := time.Duration(s.DurationSec * float64(time.Second)); learned > 0 && learned < ttl {
		ttl = learned
	}

	if s.Transient() {
		w.hints = append(w.hints, &hint{
			name:      name,
			expires:   now.Add(ttl),
			hold:      true,
			holdSpeed: w.speedAt(currentTemp),
			envelope:  currentTemp + s.PeakRise*w.opts.HoldMargin,
		})
		w.log.Info("process pre-emption: holding speed for known transient",
			"process", name, "hold_speed", w.speedAt(currentTemp), "envelope_c", currentTemp+s.PeakRise*w.opts.HoldMargin)
		return
	}

	w.hints = append(w.hints, &hint{
		name:    name,
		expires: now.Add(ttl),
		floor:   w.speedAt(currentTemp + s.PeakRise),
	})
	w.log.Info("process pre-emption: raising floor for known sustained load",
		"process", name, "floor", w.speedAt(currentTemp+s.PeakRise))
}

func (w *Watcher) speedAt(temp float64) int {
	speed := curve.Speed(w.points, temp)
	if speed < w.minSpeed {
		speed = w.minSpeed
	}
	if speed > 100 {
		speed = 100
	}
	return speed
}

func (w *Watcher) Floor() int {
	max := 0
	for _, h := range w.hints {
		if h.hold {
			continue
		}
		if h.floor > max {
			max = h.floor
		}
	}
	return max
}

func (w *Watcher) Hold(currentTemp float64) (int, bool) {
	speed := 0
	ok := false
	kept := w.hints[:0]
	for _, h := range w.hints {
		if h.hold && currentTemp > h.envelope {
			w.log.Info("process pre-emption: hold broken, temperature exceeded learned envelope",
				"process", h.name, "temp_c", currentTemp, "envelope_c", h.envelope)
			continue
		}
		if h.hold {
			ok = true
			if h.holdSpeed > speed {
				speed = h.holdSpeed
			}
		}
		kept = append(kept, h)
	}
	w.hints = kept
	return speed, ok
}

func (w *Watcher) expireHints(now time.Time) {
	kept := w.hints[:0]
	for _, h := range w.hints {
		if now.Before(h.expires) {
			kept = append(kept, h)
		}
	}
	w.hints = kept
}

func (w *Watcher) pendingNames() map[string]bool {
	names := make(map[string]bool, len(w.pending))
	for _, o := range w.pending {
		names[o.name] = true
	}
	return names
}

func (w *Watcher) loadModel() {
	data, err := os.ReadFile(w.opts.ModelPath)
	if err != nil {
		return
	}
	var m Model
	if err := json.Unmarshal(data, &m); err != nil {
		w.log.Warn("process model is unreadable; starting fresh", "path", w.opts.ModelPath, "err", err)
		return
	}
	cutoff := time.Now().Add(-staleAge).Unix()
	for name, s := range m {
		if s == nil || s.LastSeen < cutoff {
			delete(m, name)
		}
	}
	w.model = m
	w.log.Info("loaded process model", "path", w.opts.ModelPath, "entries", len(m))
}

func (w *Watcher) saveModel() {
	w.pruneModel()

	if err := os.MkdirAll(filepath.Dir(w.opts.ModelPath), 0o755); err != nil {
		w.log.Warn("failed to create model directory", "path", w.opts.ModelPath, "err", err)
		return
	}
	data, err := json.Marshal(w.model)
	if err != nil {
		w.log.Warn("failed to encode process model", "err", err)
		return
	}
	tmp := w.opts.ModelPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		w.log.Warn("failed to write process model", "path", tmp, "err", err)
		return
	}
	if err := os.Rename(tmp, w.opts.ModelPath); err != nil {
		w.log.Warn("failed to replace process model", "path", w.opts.ModelPath, "err", err)
	}
}

func (w *Watcher) pruneModel() {
	if len(w.model) <= maxModelKeys {
		return
	}
	type kv struct {
		name string
		rise float64
	}
	all := make([]kv, 0, len(w.model))
	for name, s := range w.model {
		all = append(all, kv{name, s.PeakRise})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].rise > all[j].rise })
	for _, e := range all[maxModelKeys:] {
		delete(w.model, e.name)
	}
}
