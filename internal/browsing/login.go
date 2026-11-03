package browsing

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// LoginOrigin is the browser origin to which a credential profile is bound.
// Password entry requires HTTPS except for loopback development sites.
func LoginOrigin(address string) (string, error) {
	invalid := errors.New("login requires an HTTPS URL, or loopback HTTP URL, without embedded credentials or a fragment")
	if len(address) > 4096 || !utf8.ValidString(address) || strings.TrimSpace(address) != address || strings.Contains(address, "#") {
		return "", invalid
	}
	u, err := url.Parse(address)
	if err != nil || u.User != nil || u.Hostname() == "" || u.Opaque != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", invalid
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	if u.Scheme == "http" && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return "", invalid
	}
	if ip != nil {
		host = ip.String()
	} else {
		if len(host) > 253 {
			return "", invalid
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", invalid
			}
			for _, ch := range label {
				if ch != '-' && (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') {
					return "", invalid
				}
			}
		}
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return "", invalid
	}
	if port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return "", invalid
		}
		port = strconv.Itoa(value)
	}
	if u.Scheme == "https" && port == "443" || u.Scheme == "http" && port == "80" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return u.Scheme + "://" + host, nil
}

func ValidLoginProfile(profile string) bool {
	if len(profile) == 0 || len(profile) > 32 || profile[0] < 'a' || profile[0] > 'z' {
		return false
	}
	for _, ch := range profile {
		if ch != '-' && ch != '_' && (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') {
			return false
		}
	}
	return true
}

// LoginSecretNames addresses reusable credentials, never a verification code.
// Names are shared in Commander; this binding is not a secret-store access policy.
func LoginSecretNames(address, profile string) (username, password string, err error) {
	origin, err := LoginOrigin(address)
	if err != nil {
		return "", "", err
	}
	if !ValidLoginProfile(profile) {
		return "", "", errors.New("login credential_profile must be a non-secret lowercase alias of at most 32 characters")
	}
	digest := sha256.Sum256([]byte(origin + "\x00" + profile))
	base := "browser-login-" + profile + "-" + hex.EncodeToString(digest[:])
	return base + "-username", base + "-password", nil
}
