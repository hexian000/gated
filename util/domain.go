package util

import (
	"strings"
)

func StripDomain(hostname, domain string) (string, bool) {
	n := len(hostname) - len(domain)
	if n > 0 && hostname[n-1] == '.' &&
		strings.EqualFold(hostname[n:], domain) {
		return hostname[:n-1], true
	}
	return "", false
}
