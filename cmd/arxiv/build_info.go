package main

import (
	"strings"
	"time"
)

var (
	buildDate   = "dev"
	buildCommit = ""
)

func buildDateLabel() string {
	value := strings.TrimSpace(buildDate)
	if value == "" {
		return "dev"
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.UTC().Format("2006-01-02 15:04 UTC")
	}
	return value
}

func buildCommitLabel() string {
	value := strings.TrimSpace(buildCommit)
	if value == "" || value == "unknown" {
		return ""
	}
	if len(value) > 12 {
		return value[:12]
	}
	return value
}
