package config

import (
	"strings"
	"testing"
	"time"
)

func parseStartDelay(t *testing.T, value string) (*Config, error) {
	t.Helper()
	doc, template := validVMDocument(t, "")
	text := string(doc)
	if value != "" {
		text = strings.Replace(text, "    template: base\n", "    template: base\n    start_delay: \""+value+"\"\n", 1)
	}
	key := testKey(t)
	cfg, _, err := Parse([]byte(text), t.TempDir(), func(name string) (string, bool) {
		switch name {
		case "IMAGE":
			return template, true
		case "KEY":
			return key, true
		}
		return "", false
	})
	return cfg, err
}

func TestVMStartDelayAcceptsPositiveWholeUnits(t *testing.T) {
	tests := map[string]time.Duration{
		"30s": 30 * time.Second,
		"2m":  2 * time.Minute,
		"1h":  time.Hour,
	}
	for value, want := range tests {
		t.Run(value, func(t *testing.T) {
			cfg, err := parseStartDelay(t, value)
			if err != nil {
				t.Fatal(err)
			}
			vm, _ := cfg.LookupVM("vm")
			if vm.StartDelay != value || vm.StartDelayDuration != want {
				t.Fatalf("StartDelay = %q, duration = %v; want %q, %v", vm.StartDelay, vm.StartDelayDuration, value, want)
			}
			out, err := Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), "start_delay: "+value) {
				t.Fatalf("normalized YAML lacks start_delay:\n%s", out)
			}
		})
	}
}

func TestVMStartDelayDefaultsToNoWaitAndStaysOutOfOutput(t *testing.T) {
	cfg, err := parseStartDelay(t, "")
	if err != nil {
		t.Fatal(err)
	}
	vm, _ := cfg.LookupVM("vm")
	if vm.StartDelay != "" || vm.StartDelayDuration != 0 {
		t.Fatalf("unexpected default: %q %v", vm.StartDelay, vm.StartDelayDuration)
	}
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "start_delay") {
		t.Fatalf("normalized YAML shows an unset start_delay:\n%s", out)
	}
}

func TestVMStartDelayRejectsOtherFormats(t *testing.T) {
	for _, value := range []string{"0s", "-5s", "1.5s", "30", "1d", "500ms", "abc", "30 s", "99999999999999999999h"} {
		t.Run(value, func(t *testing.T) {
			_, err := parseStartDelay(t, value)
			if err == nil || !strings.Contains(err.Error(), "start_delay must be a positive integer followed by s, m, or h") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}
