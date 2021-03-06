package util

import (
	"bufio"
	"context"
	"fmt"
	"sync"
	"time"
)

type Canceller struct {
	mu sync.RWMutex

	contexts map[context.Context]context.CancelFunc
	closeCh  chan struct{}
}

func NewCanceller() *Canceller {
	return &Canceller{
		contexts: make(map[context.Context]context.CancelFunc),
		closeCh:  make(chan struct{}),
	}
}

func (c *Canceller) WithTimeout(timeout time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.closeCh:
		cancel()
	default:
		c.contexts[ctx] = cancel
	}
	return ctx
}

func (c *Canceller) Cancel(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cancel, ok := c.contexts[ctx]; ok {
		cancel()
		delete(c.contexts, ctx)
	}
}

func (c *Canceller) CancelAll() {
	close(c.closeCh)
	c.mu.Lock()
	defer c.mu.Unlock()
	for ctx, cancel := range c.contexts {
		cancel()
		delete(c.contexts, ctx)
	}
}

func (c *Canceller) CollectMetrics(w *bufio.Writer) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, _ = w.WriteString(fmt.Sprintln("Num Active Contexts:", len(c.contexts)))
}
