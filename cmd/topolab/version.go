package main

import (
	"runtime/debug"
	"strings"
)

// version is set by release builds with -X main.version=<semantic-version>.
var version string

func buildVersion() string {
	moduleVersion, moduleSum := "", ""
	localCheckout := false
	if info, ok := debug.ReadBuildInfo(); ok {
		moduleVersion, moduleSum = info.Main.Version, info.Main.Sum
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				localCheckout = true
				break
			}
		}
	}
	return resolveVersion(version, moduleVersion, moduleSum, localCheckout)
}

func resolveVersion(injected, moduleVersion, moduleSum string, localCheckout bool) string {
	if value := strings.TrimPrefix(strings.TrimSpace(injected), "v"); value != "" {
		return value
	}
	if value := strings.TrimPrefix(strings.TrimSpace(moduleVersion), "v"); !localCheckout && moduleSum != "" && value != "" && value != "(devel)" {
		return value
	}
	return "devel"
}
