//go:build linux

package daemon

import (
	"net"
	"os"
)

func Notify(state string) (bool, error) {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return false, nil
	}

	conn, err := net.Dial("unixgram", addr)
	if err != nil {
		return false, err
	}
	defer conn.Close()

	if _, err = conn.Write([]byte(state)); err != nil {
		return false, err
	}
	return true, nil
}
