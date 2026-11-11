package hermes

import "embed"

// The bridge is application-owned; upstream Hermes is loaded unchanged.
//
//go:embed *.py all:plugin
var workerFiles embed.FS
