package core

// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Pipe adapter to connect code expecting an io.Reader
// with code expecting an io.Writer.

import (
	"errors"
	"io"
	"sync"
)

// onceError is an object that will only store an error once.
type onceError struct {
	sync.Mutex // guards following
	err        error
}

func (a *onceError) Store(err error) {
	a.Lock()
	defer a.Unlock()
	if a.err != nil {
		return
	}
	a.err = err
}
func (a *onceError) Load() error {
	a.Lock()
	defer a.Unlock()
	return a.err
}

// ErrClosedPipe is the error used for read or write operations on a closed pipe.
var ErrClosedPipe = errors.New("io: read/write on closed pipe")

// A pipe is the shared pipe structure underlying PipeReader and PipeWriter.
type pipe struct {
	wrMu sync.Mutex // Serializes Write operations
	wrCh chan []byte
	rdCh chan int

	once sync.Once // Protects closing done
	done chan struct{}
	rerr onceError
	werr onceError

	bufs [][]byte
	buf  []byte
	bw   []byte
}

func (p *pipe) Read(b []byte) (n int, err error) {
	select {
	case <-p.done:
		return 0, p.readCloseError()
	default:
	}

	if len(p.bw) > 0 {
		nr := copy(b, p.bw)
		if nr < len(p.bw) {
			p.bw = p.bw[nr:]
		} else {
			p.wrMu.Lock()
			FreeBytes(p.bufs[0])
			p.bufs = p.bufs[1:]
			p.wrMu.Unlock()
			p.buf = nil
			p.bw = nil
		}
		//p.rdCh <- nr
		return nr, nil
	}

	select {
	case bw := <-p.wrCh:
		nr := copy(b, bw)
		if nr < len(bw) {
			p.bw = bw[nr:]
			p.buf = bw
		} else {
			p.wrMu.Lock()
			FreeBytes(p.bufs[0])
			p.bufs = p.bufs[1:]
			p.wrMu.Unlock()
		}
		//p.rdCh <- nr
		return nr, nil
	case <-p.done:
		return 0, p.readCloseError()
	}
}

func (p *pipe) readCloseError() error {
	rerr := p.rerr.Load()
	if werr := p.werr.Load(); rerr == nil && werr != nil {
		return werr
	}
	return ErrClosedPipe
}

func (p *pipe) CloseRead(err error) error {
	if err == nil {
		err = ErrClosedPipe
	}
	p.rerr.Store(err)
	p.once.Do(func() { close(p.done) })
	return nil
}

func (p *pipe) Write(b []byte) (n int, err error) {
	select {
	case <-p.done:
		return 0, p.writeCloseError()
	default:
		p.wrMu.Lock()
		defer p.wrMu.Unlock()
	}

	if len(p.wrCh) > 0 {
		return 0, nil
	}

	buf := NewBytes(len(b))
	p.bufs = append(p.bufs, buf)
	copy(buf[:len(b)], b)

	//for once := true; once || len(b) > 0; once = false {
	//	select {
	//	case p.wrCh <- b:
	//		nw := <-p.rdCh
	//		b = b[nw:]
	//		n += nw
	//	case <-p.done:
	//		return n, p.writeCloseError()
	//	}
	//}
	select {
	case p.wrCh <- buf[:len(b)]:
	case <-p.done:
		return 0, p.writeCloseError()
	}
	return len(b), nil
}

func (p *pipe) writeCloseError() error {
	werr := p.werr.Load()
	if rerr := p.rerr.Load(); werr == nil && rerr != nil {
		return rerr
	}
	return ErrClosedPipe
}

func (p *pipe) CloseWrite(err error) error {
	if err == nil {
		err = io.EOF
	}
	p.werr.Store(err)
	p.once.Do(func() { close(p.done) })
	return nil
}

// A PipeReader is the read half of a pipe.
type PipeReader struct {
	p *pipe
}

// Read implements the standard Read interface:
// it reads data from the pipe, blocking until a writer
// arrives or the write end is closed.
// If the write end is closed with an error, that error is
// returned as err; otherwise err is EOF.
func (r *PipeReader) Read(data []byte) (n int, err error) {
	return r.p.Read(data)
}

// Close closes the reader; subsequent writes to the
// write half of the pipe will return the error ErrClosedPipe.
func (r *PipeReader) Close() error {
	return r.CloseWithError(nil)
}

// CloseWithError closes the reader; subsequent writes
// to the write half of the pipe will return the error err.
//
// CloseWithError never overwrites the previous error if it exists
// and always returns nil.
func (r *PipeReader) CloseWithError(err error) error {
	return r.p.CloseRead(err)
}

// A PipeWriter is the write half of a pipe.
type PipeWriter struct {
	p *pipe
}

// Write implements the standard Write interface:
// it writes data to the pipe, blocking until one or more readers
// have consumed all the data or the read end is closed.
// If the read end is closed with an error, that err is
// returned as err; otherwise err is ErrClosedPipe.
func (w *PipeWriter) Write(data []byte) (n int, err error) {
	return w.p.Write(data)
}

// Close closes the writer; subsequent reads from the
// read half of the pipe will return no bytes and EOF.
func (w *PipeWriter) Close() error {
	return w.CloseWithError(nil)
}

// CloseWithError closes the writer; subsequent reads from the
// read half of the pipe will return no bytes and the error err,
// or EOF if err is nil.
//
// CloseWithError never overwrites the previous error if it exists
// and always returns nil.
func (w *PipeWriter) CloseWithError(err error) error {
	return w.p.CloseWrite(err)
}

// Pipe creates a synchronous in-memory pipe.
// It can be used to connect code expecting an io.Reader
// with code expecting an io.Writer.
//
// Reads and Writes on the pipe are matched one to one
// except when multiple Reads are needed to consume a single Write.
// That is, each Write to the PipeWriter blocks until it has satisfied
// one or more Reads from the PipeReader that fully consume
// the written data.
// The data is copied directly from the Write to the corresponding
// Read (or Reads); there is no internal buffering.
//
// It is safe to call Read and Write in parallel with each other or with Close.
// Parallel calls to Read and parallel calls to Write are also safe:
// the individual calls will be gated sequentially.
func Pipe() (*PipeReader, *PipeWriter) {
	p := &pipe{
		wrCh: make(chan []byte, 2),
		rdCh: make(chan int),
		done: make(chan struct{}),
	}
	return &PipeReader{p}, &PipeWriter{p}
}