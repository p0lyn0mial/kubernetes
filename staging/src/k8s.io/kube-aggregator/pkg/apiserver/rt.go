package apiserver

import (
	"io"
	"net/http"
)

type proxyResponseModifier struct {
	shutDownInProgressCh <-chan struct{}
}

func (prm *proxyResponseModifier) ModifyResponse(r *http.Response) error {
	if r.StatusCode == 200 {
		bh := newBodyHijacker(r.Body, prm.shutDownInProgressCh)
		r.Body = bh
	}
	return nil
}

type tuple struct {
	n int
	e error
	p []byte
}

type bodyHijacker struct {
	origin io.ReadCloser

	// TODO: better name
	shutDownInProgressCh <-chan struct{}

	readCh chan tuple

	done chan struct{}

	started bool
}

func newBodyHijacker(o io.ReadCloser, shutDownInProgressCh <-chan struct{}) *bodyHijacker {
	return &bodyHijacker{
		origin:               o,
		shutDownInProgressCh: shutDownInProgressCh,
		readCh:               make(chan tuple),
		done:                 make(chan struct{}),
	}
}

func (b *bodyHijacker) Read(p []byte) (int, error) {
	if !b.started {
		b.started = true
		go b.startReader()
	}

	select {
	case data := <-b.readCh:
		p = data.p
		return data.n, data.e
	case <-b.shutDownInProgressCh:
		return 0, io.EOF
	case <-b.done:
		return 0, io.EOF
	}

	return 0, io.EOF
}

func (b *bodyHijacker) Close() error {
	defer close(b.done)
	return b.origin.Close()
}

func (b *bodyHijacker) startReader() {
	buf := make([]byte, 32*1024)
	for {
		n, e := b.origin.Read(buf)

		t := tuple{n, e, make([]byte, 0)}
		copy(t.p, buf)

		select {
		case b.readCh <- t:
			if e != nil {
				return
			}
		case <-b.done:
			return
		}
	}
}
