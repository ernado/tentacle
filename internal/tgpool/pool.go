package tgpool

import (
	"context"
	"sync"

	"github.com/go-faster/errors"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

func New() *Pool {
	return &Pool{}
}

var _ tg.Invoker = (*Pool)(nil)

// Pool of telegram API's.
type Pool struct {
	clients []tg.Invoker
	index   int
	mux     sync.Mutex
}

func (p *Pool) Add(client tg.Invoker) {
	p.mux.Lock()
	defer p.mux.Unlock()

	p.clients = append(p.clients, client)
}

func (p *Pool) next() tg.Invoker {
	p.mux.Lock()
	defer p.mux.Unlock()

	if len(p.clients) == 0 {
		return nil
	}

	p.index = (p.index + 1) % len(p.clients)

	return p.clients[p.index]
}

func (p *Pool) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	client := p.next()
	if client == nil {
		return errors.New("no clients")
	}
	return client.Invoke(ctx, input, output)
}
