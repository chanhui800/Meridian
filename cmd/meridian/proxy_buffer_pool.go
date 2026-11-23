package main

import "sync"

const proxyCopyBufferSize = 32 << 10

// Reuse only the fixed-size response copy buffer. Every active response owns
// its buffer until ReverseProxy finishes, including cancellation paths.
type proxyCopyBufferPool struct {
	pool sync.Pool
}

func (p *proxyCopyBufferPool) Get() []byte {
	if buffer := p.pool.Get(); buffer != nil {
		return buffer.(*[proxyCopyBufferSize]byte)[:]
	}
	return new([proxyCopyBufferSize]byte)[:]
}

func (p *proxyCopyBufferPool) Put(buffer []byte) {
	if cap(buffer) != proxyCopyBufferSize {
		return
	}
	// An array pointer avoids allocating a boxed slice header for each Put.
	p.pool.Put((*[proxyCopyBufferSize]byte)(buffer[:proxyCopyBufferSize]))
}

var sharedProxyBufferPool = &proxyCopyBufferPool{}
