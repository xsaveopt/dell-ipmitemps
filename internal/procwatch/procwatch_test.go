package procwatch

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sratabix/dell-ipmitemps/internal/config"
)

func curvePoints() []config.CurvePoint {
	return []config.CurvePoint{
		{Temp: 30, Percent: 10},
		{Temp: 50, Percent: 30},
		{Temp: 65, Percent: 55},
		{Temp: 75, Percent: 80},
		{Temp: 82, Percent: 100},
	}
}

func testWatcher(t *testing.T) *Watcher {
	t.Helper()
	opts := Options{
		ProcPath:        filepath.Join(t.TempDir(), "proc"),
		ModelPath:       filepath.Join(t.TempDir(), "model.json"),
		MinObservations: 2,
		ImpactThreshold: 3,
		PreemptTTL:      60 * time.Second,
		HoldMargin:      1.25,
		Decay:           0.5,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(opts, curvePoints(), 10, log)
}

func TestScan(t *testing.T) {
	dir := t.TempDir()
	mkproc := func(pid, comm string) {
		d := filepath.Join(dir, pid)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if comm != "" {
			if err := os.WriteFile(filepath.Join(d, "comm"), []byte(comm+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mkproc("123", "stress-ng")
	mkproc("456", "")        // no comm file -> skipped
	mkproc("self", "ignore") // non-numeric -> skipped

	pids, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pids) != 1 || pids[123] != "stress-ng" {
		t.Fatalf("got %v, want {123: stress-ng}", pids)
	}
}

const (
	baseline = 40.0
	peak     = 70.0
)

// runOccurrence simulates a process spawning and ramping to peak. When recovers
// is true the temperature falls back to baseline within the window (transient);
// otherwise it stays elevated through the close (sustained).
func (w *Watcher) runOccurrence(start time.Time, recovers bool) {
	w.update(start, baseline, map[int]string{1: "hot"})
	w.update(start.Add(5*time.Second), peak, map[int]string{1: "hot"})
	final := peak
	if recovers {
		w.update(start.Add(10*time.Second), baseline, map[int]string{1: "hot"})
		final = baseline
	}
	w.update(start.Add(61*time.Second), final, map[int]string{}) // finalize, proc gone
}

func TestTransientLearnsAndHolds(t *testing.T) {
	w := testWatcher(t)
	base := time.Unix(1000, 0)
	w.update(base, 40, map[int]string{}) // establish baseline PID set

	w.runOccurrence(base.Add(10*time.Second), true)
	w.runOccurrence(base.Add(200*time.Second), true)

	s := w.model["hot"]
	if s == nil || s.Observations != 2 {
		t.Fatalf("model = %+v, want 2 observations", s)
	}
	if !s.Transient() {
		t.Fatalf("signature should be transient, score = %v", s.TransientScore)
	}

	// Third spawn: now trusted -> should open a hold.
	t3 := base.Add(400 * time.Second)
	w.update(t3, 40, map[int]string{1: "hot"})

	speed, ok := w.Hold(40)
	if !ok || speed != 20 {
		t.Fatalf("Hold(40) = (%d, %v), want (20, true)", speed, ok)
	}

	// Temperature blows past the learned envelope (40 + 30*1.25 = 77.5) -> hold breaks.
	if _, ok := w.Hold(90); ok {
		t.Fatal("Hold should break once temperature exceeds the learned envelope")
	}
}

func TestSustainedLearnsAndRaisesFloor(t *testing.T) {
	w := testWatcher(t)
	base := time.Unix(1000, 0)
	w.update(base, 40, map[int]string{})

	// Never recovers within the window -> sustained signature.
	w.runOccurrence(base.Add(10*time.Second), false)
	w.runOccurrence(base.Add(200*time.Second), false)

	s := w.model["hot"]
	if s == nil || s.Observations != 2 {
		t.Fatalf("model = %+v, want 2 observations", s)
	}
	if s.Transient() {
		t.Fatalf("signature should be sustained, score = %v", s.TransientScore)
	}

	t3 := base.Add(400 * time.Second)
	w.update(t3, 40, map[int]string{1: "hot"})

	// Floor = curve speed at baseline + learned peak rise (40 + 30 = 70 -> 67%).
	if floor := w.Floor(); floor != 67 {
		t.Fatalf("Floor() = %d, want 67", floor)
	}
	if _, ok := w.Hold(40); ok {
		t.Fatal("sustained signature should not produce a hold")
	}
}

func TestModelPersists(t *testing.T) {
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.json")
	opts := Options{
		ProcPath:        filepath.Join(dir, "proc"),
		ModelPath:       modelPath,
		MinObservations: 2,
		ImpactThreshold: 3,
		PreemptTTL:      60 * time.Second,
		HoldMargin:      1.25,
		Decay:           0.5,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	w := New(opts, curvePoints(), 10, log)
	base := time.Unix(1000, 0)
	w.update(base, 40, map[int]string{})
	w.runOccurrence(base.Add(10*time.Second), true)

	if _, err := os.Stat(modelPath); err != nil {
		t.Fatalf("model file not written: %v", err)
	}

	reloaded := New(opts, curvePoints(), 10, log)
	if s := reloaded.model["hot"]; s == nil || s.Observations != 1 {
		t.Fatalf("reloaded model = %+v, want 1 observation", reloaded.model["hot"])
	}
}
