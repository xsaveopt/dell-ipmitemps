package main

import "testing"

func TestConfigPathFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want string
	}{
		{name: "unset falls back to the default", env: "", want: defaultConfigPath},
		{name: "set overrides the default", env: "/etc/example/config.yaml", want: "/etc/example/config.yaml"},
		{name: "relative path is kept as given", env: "config.yaml", want: "config.yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DELLIPMIFANCTL_CONFIG", tc.env)
			if got := configPathFromEnv(); got != tc.want {
				t.Errorf("configPathFromEnv() = %q, want %q", got, tc.want)
			}
		})
	}
}
