package proxy

import (
	"io"
	"net"
)

type BufConn struct {
	net.Conn
	io.Reader
}

func (c *BufConn) Read(p []byte) (n int, err error) {
	return c.Reader.Read(p)
}

func (c *BufConn) Write(b []byte) (n int, err error) {
	return c.Conn.Write(b)
}
