//go:build !unix

package hermes

func supportedPlatform() bool { return false }
