package version

import (
	"fmt"
	"time"
)

var (
	tag      = ""
	homepage = "https://github.com/hexian000/gated"
)

func init() {
	fmt.Printf("gated %s\n  %s\n", Version(), homepage)
}

func Version() string {
	if tag != "" {
		return tag
	}
	return "development"
}

func WebBanner(hostname string) string {
	return fmt.Sprintf(
		"gated@%s %s\n  %s\n\nserver time: %v\n\n",
		hostname, Version(), homepage,
		time.Now().Format(time.RFC3339),
	)
}
