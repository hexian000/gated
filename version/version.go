package version

import (
	"fmt"
	"time"
)

const (
	Major    = 0
	Minor    = 0
	Revision = 0

	Tag      = ""
	Homepage = "https://github.com/hexian000/gated"
)

func init() {
	fmt.Printf("gated %s\n  %s\n", Version(), Homepage)
}

func Version() string {
	if Tag != "" {
		return Tag
	}
	return fmt.Sprintf("v%d.%d.%d", Major, Minor, Revision)
}

func WebBanner(hostname string) string {
	return fmt.Sprintf(
		"tlswrapper@%s %s\n  %s\n\nserver time: %v\n\n",
		hostname, Version(), Homepage,
		time.Now().Format(time.RFC3339),
	)
}
