package proxy

import (
	"io"
	"net"
)

type BufConn struct {
	net.Conn
	io.Reader
}

var _ = net.Conn(&BufConn{})

func (c *BufConn) Read(p []byte) (n int, err error) {
	return c.Reader.Read(p)
}

func (c *BufConn) Write(b []byte) (n int, err error) {
	return c.Conn.Write(b)
}
