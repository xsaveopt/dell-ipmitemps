package procwatch

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func writeModelFile(t *testing.T, w *Watcher, contents string) {
	t.Helper()
	if err := os.WriteFile(w.opts.ModelPath, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func reload(w *Watcher) *Watcher {
	return New(w.opts, w.points, w.minSpeed, w.log)
}

func TestLoadModelMissingFileStartsEmpty(t *testing.T) {
	w := testWatcher(t)
	if w.model == nil || len(w.model) != 0 {
		t.Fatalf("model = %v, want an empty model", w.model)
	}
}

func TestLoadModelDropsStaleEntries(t *testing.T) {
	w := testWatcher(t)
	now := time.Now()
	m := Model{
		"fresh": {PeakRise: 10, Observations: 3, LastSeen: now.Add(-24 * time.Hour).Unix()},
		"stale": {PeakRise: 10, Observations: 3, LastSeen: now.Add(-31 * 24 * time.Hour).Unix()},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	writeModelFile(t, w, string(data))

	r := reload(w)

	if r.model["fresh"] == nil {
		t.Error("want the recent entry kept")
	}
	if _, ok := r.model["stale"]; ok {
		t.Error("want the entry older than 30 days dropped")
	}
}

func TestLoadModelDropsNilEntries(t *testing.T) {
	w := testWatcher(t)
	fresh := strconv.FormatInt(time.Now().Unix(), 10)
	writeModelFile(t, w, `{"empty": null, "real": {"peak_rise": 5, "observations": 2, "last_seen": `+fresh+`}}`)

	r := reload(w)

	if _, ok := r.model["empty"]; ok {
		t.Error("want a null entry dropped")
	}
	if r.model["real"] == nil || r.model["real"].Observations != 2 {
		t.Errorf("model = %v, want the real entry kept", r.model)
	}
}

func TestLoadModelCorruptFileStartsFresh(t *testing.T) {
	w := testWatcher(t)
	writeModelFile(t, w, "{not json")

	r := reload(w)

	if r.model == nil || len(r.model) != 0 {
		t.Fatalf("model = %v, want an empty model after a corrupt file", r.model)
	}
	base := time.Unix(1000, 0)
	r.update(base, 40, map[int]string{})
	r.runOccurrence(base.Add(10*time.Second), true)
	if s := r.model["hot"]; s == nil || s.Observations != 1 {
		t.Errorf("model = %v, want learning to work after a corrupt file", r.model)
	}
}

func TestLoadModelNullFileStillLearns(t *testing.T) {
	w := testWatcher(t)
	writeModelFile(t, w, "null")

	r := reload(w)

	defer func() {
		if p := recover(); p != nil {
			t.Errorf("learning after a model file containing null panicked: %v", p)
		}
	}()
	base := time.Unix(1000, 0)
	r.update(base, 40, map[int]string{})
	r.runOccurrence(base.Add(10*time.Second), true)
	if s := r.model["hot"]; s == nil || s.Observations != 1 {
		t.Errorf("model = %v, want the observation learned", r.model)
	}
}

func TestPruneModelKeepsTheHighestImpactEntries(t *testing.T) {
	w := testWatcher(t)
	total := maxModelKeys + 8
	for i := range total {
		w.model["p"+strconv.Itoa(i)] = &Stat{PeakRise: float64(i), Observations: 1, LastSeen: time.Now().Unix()}
	}

	w.saveModel()

	if len(w.model) != maxModelKeys {
		t.Fatalf("model has %d entries, want %d", len(w.model), maxModelKeys)
	}
	for i := range 8 {
		if _, ok := w.model["p"+strconv.Itoa(i)]; ok {
			t.Errorf("p%d with the lowest rise should have been pruned", i)
		}
	}
	if w.model["p"+strconv.Itoa(total-1)] == nil {
		t.Error("the highest rise entry must survive pruning")
	}
	if n := len(reload(w).model); n != maxModelKeys {
		t.Errorf("persisted model has %d entries, want %d", n, maxModelKeys)
	}
}

func TestPruneModelLeavesSmallModelsAlone(t *testing.T) {
	w := testWatcher(t)
	for i := range maxModelKeys {
		w.model["p"+strconv.Itoa(i)] = &Stat{PeakRise: float64(i)}
	}

	w.pruneModel()

	if len(w.model) != maxModelKeys {
		t.Errorf("model has %d entries, want all %d kept", len(w.model), maxModelKeys)
	}
}

func TestPollScanFailureLeavesStateAndRecovers(t *testing.T) {
	w := testWatcher(t)

	w.Poll(time.Unix(1000, 0), 40)
	if !w.scanErrLogged {
		t.Error("want the scan failure recorded")
	}
	if w.prevPIDs != nil {
		t.Errorf("prevPIDs = %v, want nil after a failed scan", w.prevPIDs)
	}

	d := filepath.Join(w.opts.ProcPath, "42")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "comm"), []byte("worker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w.Poll(time.Unix(1010, 0), 40)
	if w.scanErrLogged {
		t.Error("want the scan error flag cleared once scanning works")
	}
	if w.prevPIDs[42] != "worker" {
		t.Errorf("prevPIDs = %v, want the process recorded", w.prevPIDs)
	}
	if len(w.pending) != 0 {
		t.Errorf("pending = %d, want processes present at the first good scan not treated as spawns", len(w.pending))
	}
}

func TestFirstScanDoesNotCountAsSpawns(t *testing.T) {
	w := testWatcher(t)
	w.model["hot"] = &Stat{PeakRise: 30, Observations: 5}

	w.update(time.Unix(1000, 0), 40, map[int]string{1: "hot"})

	if len(w.pending) != 0 || len(w.hints) != 0 {
		t.Errorf("pending=%d hints=%d, want nothing from the initial process set", len(w.pending), len(w.hints))
	}
}

func TestPendingNamesDedupe(t *testing.T) {
	w := testWatcher(t)
	base := time.Unix(1000, 0)
	w.update(base, 40, map[int]string{})

	w.update(base.Add(time.Second), 40, map[int]string{1: "hot", 2: "hot", 3: "cold"})
	if len(w.pending) != 2 {
		t.Fatalf("pending = %d, want one observation per name", len(w.pending))
	}

	w.update(base.Add(2*time.Second), 40, map[int]string{1: "hot", 2: "hot", 3: "cold", 4: "hot"})
	if len(w.pending) != 2 {
		t.Errorf("pending = %d, want no new observation while one is open for the name", len(w.pending))
	}
}

func TestSpawnNeedsMinObservations(t *testing.T) {
	w := testWatcher(t)
	w.model["hot"] = &Stat{PeakRise: 30, DurationSec: 60, Observations: 1}
	base := time.Unix(1000, 0)
	w.update(base, 40, map[int]string{})

	w.update(base.Add(time.Second), 40, map[int]string{1: "hot"})

	if len(w.hints) != 0 {
		t.Errorf("hints = %d, want none below min_observations", len(w.hints))
	}
}

func TestSpawnNeedsImpactThreshold(t *testing.T) {
	cases := []struct {
		name      string
		rise      float64
		wantHints int
	}{
		{name: "below", rise: 2.9, wantHints: 0},
		{name: "at", rise: 3, wantHints: 1},
		{name: "above", rise: 10, wantHints: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := testWatcher(t)
			w.model["hot"] = &Stat{PeakRise: tc.rise, DurationSec: 60, Observations: 5}
			base := time.Unix(1000, 0)
			w.update(base, 40, map[int]string{})

			w.update(base.Add(time.Second), 40, map[int]string{1: "hot"})

			if len(w.hints) != tc.wantHints {
				t.Errorf("hints = %d, want %d", len(w.hints), tc.wantHints)
			}
		})
	}
}

func TestHintsExpireAfterTheTTL(t *testing.T) {
	w := testWatcher(t)
	w.model["hot"] = &Stat{PeakRise: 30, DurationSec: 0, Observations: 5}
	base := time.Unix(1000, 0)
	w.update(base, 40, map[int]string{})
	spawnAt := base.Add(time.Second)
	w.update(spawnAt, 40, map[int]string{1: "hot"})

	w.update(spawnAt.Add(59*time.Second), 40, map[int]string{1: "hot"})
	if w.Floor() == 0 {
		t.Fatal("want the floor still active before the preempt TTL")
	}

	w.update(spawnAt.Add(60*time.Second), 40, map[int]string{1: "hot"})
	if f := w.Floor(); f != 0 {
		t.Errorf("Floor() = %d, want 0 once the preempt TTL passed", f)
	}
}

func TestHintTTLShortenedToLearnedDuration(t *testing.T) {
	w := testWatcher(t)
	w.model["hot"] = &Stat{PeakRise: 30, DurationSec: 10, Observations: 5}
	base := time.Unix(1000, 0)
	w.update(base, 40, map[int]string{})
	spawnAt := base.Add(time.Second)
	w.update(spawnAt, 40, map[int]string{1: "hot"})

	w.update(spawnAt.Add(9*time.Second), 40, map[int]string{1: "hot"})
	if w.Floor() == 0 {
		t.Fatal("want the floor active within the learned duration")
	}

	w.update(spawnAt.Add(10*time.Second), 40, map[int]string{1: "hot"})
	if f := w.Floor(); f != 0 {
		t.Errorf("Floor() = %d, want 0 after the learned duration", f)
	}
}

func TestHintTTLNotExtendedByLongerLearnedDuration(t *testing.T) {
	w := testWatcher(t)
	w.model["hot"] = &Stat{PeakRise: 30, DurationSec: 600, Observations: 5}
	base := time.Unix(1000, 0)
	w.update(base, 40, map[int]string{})
	spawnAt := base.Add(time.Second)
	w.update(spawnAt, 40, map[int]string{1: "hot"})

	w.update(spawnAt.Add(60*time.Second), 40, map[int]string{1: "hot"})
	if f := w.Floor(); f != 0 {
		t.Errorf("Floor() = %d, want the preempt TTL to cap the hint", f)
	}
}

func TestHoldKeepsNonHoldHints(t *testing.T) {
	w := testWatcher(t)
	w.hints = []*hint{
		{name: "burst", hold: true, holdSpeed: 20, envelope: 50, expires: time.Unix(2000, 0)},
		{name: "render", floor: 67, expires: time.Unix(2000, 0)},
	}

	if _, ok := w.Hold(60); ok {
		t.Error("want the hold broken above its envelope")
	}
	if f := w.Floor(); f != 67 {
		t.Errorf("Floor() = %d, want the floor hint kept when a hold breaks", f)
	}
}

func TestSpeedAtClampsToMinSpeed(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := New(Options{ModelPath: filepath.Join(t.TempDir(), "model.json")}, curvePoints(), 35, log)

	if got := w.speedAt(30); got != 35 {
		t.Errorf("speedAt(30) = %d, want the minimum of 35", got)
	}
	if got := w.speedAt(90); got != 100 {
		t.Errorf("speedAt(90) = %d, want 100", got)
	}
}
