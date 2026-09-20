package ipmi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xsaveopt/dell-ipmitemps/internal/config"
)

type recorder struct {
	calls [][]string
	names []string
	out   []byte
	err   error
}

func (r *recorder) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.names = append(r.names, name)
	r.calls = append(r.calls, append([]string(nil), args...))
	return r.out, r.err
}

func (r *recorder) last() []string {
	if len(r.calls) == 0 {
		return nil
	}
	return r.calls[len(r.calls)-1]
}

func newTest(cfg config.IPMI, rec *recorder) *Controller {
	return &Controller{cfg: cfg, exec: rec.run}
}

var localCfg = config.IPMI{Local: true}

var remoteCfg = config.IPMI{Host: "bmc.local", User: "admin", Pass: "hunter2"}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestNewWiresTheRealRunner(t *testing.T) {
	if New(localCfg).exec == nil {
		t.Fatal("New must install a command runner")
	}
}

func TestLocalCommands(t *testing.T) {
	cases := []struct {
		name string
		call func(*Controller) error
		want []string
	}{
		{
			name: "ping",
			call: func(c *Controller) error { return c.Ping(context.Background()) },
			want: []string{"mc", "info"},
		},
		{
			name: "manual",
			call: func(c *Controller) error { return c.SetManual(context.Background()) },
			want: []string{"raw", "0x30", "0x30", "0x01", "0x00"},
		},
		{
			name: "auto",
			call: func(c *Controller) error { return c.SetAuto(context.Background()) },
			want: []string{"raw", "0x30", "0x30", "0x01", "0x01"},
		},
		{
			name: "speed",
			call: func(c *Controller) error { return c.SetSpeed(context.Background(), 40) },
			want: []string{"raw", "0x30", "0x30", "0x02", "0xff", "0x28"},
		},
		{
			name: "disable pcie cooling",
			call: func(c *Controller) error { return c.DisablePCIeCooling(context.Background()) },
			want: []string{
				"raw", "0x30", "0xce", "0x00", "0x16", "0x05",
				"0x00", "0x00", "0x00", "0x05", "0x00", "0x01", "0x00", "0x00",
			},
		},
		{
			name: "enable pcie cooling",
			call: func(c *Controller) error { return c.EnablePCIeCooling(context.Background()) },
			want: []string{
				"raw", "0x30", "0xce", "0x00", "0x16", "0x05",
				"0x00", "0x00", "0x00", "0x05", "0x00", "0x00", "0x00", "0x00",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			if err := tc.call(newTest(localCfg, rec)); err != nil {
				t.Fatalf("call: %v", err)
			}
			if len(rec.names) != 1 || rec.names[0] != "ipmitool" {
				t.Fatalf("binary = %v, want a single ipmitool invocation", rec.names)
			}
			if !equal(rec.last(), tc.want) {
				t.Errorf("args = %v, want %v", rec.last(), tc.want)
			}
		})
	}
}

func TestEnableAndDisablePCIeCoolingDifferOnlyInTheFlagByte(t *testing.T) {
	off := &recorder{}
	on := &recorder{}
	if err := newTest(localCfg, off).DisablePCIeCooling(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := newTest(localCfg, on).EnablePCIeCooling(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, b := off.last(), on.last()
	if len(a) != len(b) {
		t.Fatalf("arg counts differ: %v vs %v", a, b)
	}
	diff := 0
	for i := range a {
		if a[i] != b[i] {
			diff++
		}
	}
	if diff != 1 {
		t.Errorf("%d bytes differ, want exactly one", diff)
	}
}

func TestSetSpeedHexEncoding(t *testing.T) {
	cases := []struct {
		pct  int
		want string
	}{
		{0, "0x00"},
		{1, "0x01"},
		{10, "0x0a"},
		{15, "0x0f"},
		{16, "0x10"},
		{50, "0x32"},
		{99, "0x63"},
		{100, "0x64"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			rec := &recorder{}
			if err := newTest(localCfg, rec).SetSpeed(context.Background(), tc.pct); err != nil {
				t.Fatal(err)
			}
			args := rec.last()
			if got := args[len(args)-1]; got != tc.want {
				t.Errorf("SetSpeed(%d) encoded %q, want %q", tc.pct, got, tc.want)
			}
		})
	}
}

func TestRemoteAddsLanplusPrefix(t *testing.T) {
	rec := &recorder{}
	if err := newTest(remoteCfg, rec).SetManual(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-I", "lanplus", "-H", "bmc.local", "-U", "admin", "-P", "hunter2",
		"raw", "0x30", "0x30", "0x01", "0x00",
	}
	if !equal(rec.last(), want) {
		t.Errorf("args = %v, want %v", rec.last(), want)
	}
}

func TestLocalOmitsLanplusPrefix(t *testing.T) {
	rec := &recorder{}
	if err := newTest(localCfg, rec).Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, a := range rec.last() {
		if a == "lanplus" || a == "-H" {
			t.Fatalf("local mode must not pass remote flags, got %v", rec.last())
		}
	}
}

func TestRemotePrefixDoesNotLeakBetweenCalls(t *testing.T) {
	rec := &recorder{}
	c := newTest(remoteCfg, rec)
	if err := c.SetManual(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.SetSpeed(context.Background(), 30); err != nil {
		t.Fatal(err)
	}
	second := rec.calls[1]
	want := []string{
		"-I", "lanplus", "-H", "bmc.local", "-U", "admin", "-P", "hunter2",
		"raw", "0x30", "0x30", "0x02", "0xff", "0x1e",
	}
	if !equal(second, want) {
		t.Errorf("second call args = %v, want %v", second, want)
	}
}

func TestErrorNeverLeaksThePassword(t *testing.T) {
	rec := &recorder{out: []byte("  Unable to establish IPMI v2 session  \n"), err: errors.New("exit status 1")}
	err := newTest(remoteCfg, rec).SetSpeed(context.Background(), 60)
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if strings.Contains(msg, "hunter2") {
		t.Fatalf("error leaked the BMC password: %q", msg)
	}
	if strings.Contains(msg, "lanplus") || strings.Contains(msg, "bmc.local") {
		t.Errorf("error = %q, want only the bare ipmitool command echoed", msg)
	}
	if !strings.Contains(msg, "raw 0x30 0x30 0x02 0xff 0x3c") {
		t.Errorf("error = %q, want it to name the failed command", msg)
	}
	if !strings.Contains(msg, "Unable to establish IPMI v2 session") {
		t.Errorf("error = %q, want the tool output included", msg)
	}
	if strings.Contains(msg, "session  \n") {
		t.Errorf("error = %q, want the tool output trimmed", msg)
	}
}

func TestErrorWrapsTheUnderlyingFailure(t *testing.T) {
	boom := errors.New("exit status 1")
	rec := &recorder{err: boom}
	err := newTest(localCfg, rec).Ping(context.Background())
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to wrap %v", err, boom)
	}
}

func TestEveryCommandPropagatesFailure(t *testing.T) {
	calls := map[string]func(*Controller) error{
		"Ping":               func(c *Controller) error { return c.Ping(context.Background()) },
		"SetManual":          func(c *Controller) error { return c.SetManual(context.Background()) },
		"SetAuto":            func(c *Controller) error { return c.SetAuto(context.Background()) },
		"SetSpeed":           func(c *Controller) error { return c.SetSpeed(context.Background(), 50) },
		"DisablePCIeCooling": func(c *Controller) error { return c.DisablePCIeCooling(context.Background()) },
		"EnablePCIeCooling":  func(c *Controller) error { return c.EnablePCIeCooling(context.Background()) },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{err: errors.New("no such device")}
			if err := call(newTest(localCfg, rec)); err == nil {
				t.Error("want the runner failure surfaced")
			}
		})
	}
}

func TestContextIsPassedThrough(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "marker")

	var seen context.Context
	c := &Controller{cfg: localCfg, exec: func(got context.Context, _ string, _ ...string) ([]byte, error) {
		seen = got
		return nil, nil
	}}
	if err := c.SetAuto(ctx); err != nil {
		t.Fatal(err)
	}
	if seen == nil || seen.Value(key{}) != "marker" {
		t.Error("the caller's context must reach the command runner")
	}
}
