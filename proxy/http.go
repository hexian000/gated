package proxy

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"
)

func Client(ctx context.Context, conn net.Conn, host string) (*BufConn, error) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
		defer func() {
			_ = conn.SetDeadline(time.Time{})
		}()
	}

	rd := bufio.NewReader(conn)
	req := &http.Request{
		Method:     http.MethodConnect,
		URL:        &url.URL{Host: host},
		Host:       host,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
	}
	req.Header.Set("Proxy-Connection", "keep-alive")
	if err := req.WriteProxy(conn); err != nil {
		return nil, err
	}
	resp, err := http.ReadResponse(rd, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status)
	}

	return &BufConn{
		conn,
		resp.Body,
	}, nil
}

func Hijack(w http.ResponseWriter) (net.Conn, error) {
	h, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("hijacking is not supported")
	}
	conn, rw, err := h.Hijack()
	if err != nil {
		return nil, err
	}
	err = rw.Flush()
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return &BufConn{conn, rw}, nil
}
