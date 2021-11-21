package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"sync"
)

type Forwarder struct {
	mu sync.RWMutex
	wg sync.WaitGroup

	closers map[io.Closer]struct{}
	errCh   chan error
}

func NewForwarder() *Forwarder {
	return &Forwarder{
		closers: make(map[io.Closer]struct{}),
		errCh:   make(chan error, 8),
	}
}

func (f *Forwarder) Errors() <-chan error {
	return f.errCh
}

func (f *Forwarder) Shutdown() {
	func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for c := range f.closers {
			_ = c.Close()
			delete(f.closers, c)
		}
	}()
	f.wg.Wait()
}

func (f *Forwarder) copy(dst net.Conn, src net.Conn) {
	defer f.wg.Done()
	defer func() {
		_ = src.Close()
		_ = dst.Close()
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.closers, src)
		delete(f.closers, dst)
	}()
	defer func() {
		if err := recover(); err != nil {
			select {
			case f.errCh <- fmt.Errorf("panic: %v", err):
			default:
			}
		}
	}()
	_, err := io.Copy(dst, src)
	if err != nil {
		select {
		case f.errCh <- err:
		default:
		}
	}
}

func (f *Forwarder) Forward(accepted net.Conn, dialed net.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closers[accepted] = struct{}{}
	f.closers[dialed] = struct{}{}
	f.wg.Add(2)
	go f.copy(accepted, dialed)
	go f.copy(dialed, accepted)
}

func (f *Forwarder) CollectMetrics(w *bufio.Writer) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	_, _ = w.WriteString(fmt.Sprintln("Num Forwarder Sockets:", len(f.closers)))
}
