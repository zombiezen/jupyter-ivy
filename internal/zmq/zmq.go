// Copyright 2023 Ross Light
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//		 https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

// Package zmq provides access to the C 0MQ library.
package zmq

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// #cgo pkg-config: libzmq
// #include <stdlib.h>
// #include <errno.h>
// #include <zmq.h>
//
// static int myrecv(void *socket, void *buf, size_t len, int* more) {
//   int ret = zmq_recv(socket, buf, len, ZMQ_DONTWAIT);
//   size_t optionLen = sizeof(int);
//   zmq_getsockopt(socket, ZMQ_RCVMORE, more, &optionLen);
//   return ret;
// }
import "C"

// ZeroMQ is a 0MQ context, which manages a thread pool.
// Methods on a *ZeroMQ are safe to call from multiple goroutines simultaneously.
type ZeroMQ struct {
	ctx unsafe.Pointer
}

// Options holds optional parameters for [New].
type Options struct {
	// NumThreads specifies the number of I/O threads for the 0MQ pool.
	// Values less than 1 are treated identically to 1.
	NumThreads int
}

// New returns a new 0MQ context.
// If opts is nil, it is treated identically to the zero value.
func New(opts *Options) (*ZeroMQ, error) {
	ctx, err := C.zmq_ctx_new()
	if ctx == nil {
		return nil, fmt.Errorf("zeromq: init: %w", err)
	}

	if opts == nil {
		opts = new(Options)
	}
	C.zmq_ctx_set(ctx, C.ZMQ_IPV6, 1)
	C.zmq_ctx_set(ctx, C.ZMQ_BLOCKY, 0)
	if opts.NumThreads > 1 {
		C.zmq_ctx_set(ctx, C.ZMQ_IO_THREADS, C.int(opts.NumThreads))
	} else {
		C.zmq_ctx_set(ctx, C.ZMQ_IO_THREADS, 1)
	}

	return &ZeroMQ{ctx: ctx}, nil
}

// Close releases any resources associated with the 0MQ context,
// blocking until all sockets created in the context are closed.
func (z *ZeroMQ) Close() error {
	for {
		ret, err := C.zmq_ctx_term(z.ctx)
		if ret == 0 {
			return nil
		}
		if err != syscall.EINTR {
			return fmt.Errorf("zeromq: close: %w", err)
		}
	}
}

// A Socket represents a message queue.
// Methods on a *Socket are safe to call from multiple goroutines simultaneously.
type Socket struct {
	mu  sync.Mutex
	ptr unsafe.Pointer
}

// NewSocket returns a new socket with the given type.
func (z *ZeroMQ) NewSocket(t SocketType) (*Socket, error) {
	ptr, err := C.zmq_socket(z.ctx, C.int(t))
	if ptr == nil {
		return nil, fmt.Errorf("zeromq: new socket: %w", err)
	}
	return &Socket{ptr: ptr}, nil
}

func (z *ZeroMQ) newBind(typ SocketType, endpoints []string) (*Socket, error) {
	s, err := z.NewSocket(typ)
	if err != nil {
		return nil, err
	}
	for _, endpoint := range endpoints {
		if err := s.Bind(endpoint); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

// NewRep returns a [TypeRep] socket that binds to zero or more endpoints.
func (z *ZeroMQ) NewRep(endpoints ...string) (*Socket, error) {
	return z.newBind(TypeRep, endpoints)
}

// NewPub returns a [TypePub] socket that binds to zero or more endpoints.
func (z *ZeroMQ) NewPub(endpoints ...string) (*Socket, error) {
	return z.newBind(TypePub, endpoints)
}

// NewRouter returns a [TypeRouter] socket that binds to zero or more endpoints.
func (z *ZeroMQ) NewRouter(endpoints ...string) (*Socket, error) {
	return z.newBind(TypeRouter, endpoints)
}

// Close releases the resources associated with the socket.
func (s *Socket) Close() error {
	s.mu.Lock()
	ret, err := C.zmq_close(s.ptr)
	s.ptr = nil
	s.mu.Unlock()
	if ret != 0 {
		return fmt.Errorf("zeromq: close socket: %w", err)
	}
	return nil
}

// Bind binds the socket to a local endpoint
// and then accepts incoming connections on that endpoint.
func (s *Socket) Bind(endpoint string) error {
	cEndpoint := C.CString(endpoint)
	defer C.free(unsafe.Pointer(cEndpoint))
	s.mu.Lock()
	ret, err := C.zmq_bind(s.ptr, cEndpoint)
	s.mu.Unlock()
	if ret != 0 {
		return fmt.Errorf("zeromq: bind %q: %w", endpoint, err)
	}
	return nil
}

// Connect connects the socket to an endpoint
// and then accepts incoming connections on that endpoint.
func (s *Socket) Connect(endpoint string) error {
	cEndpoint := C.CString(endpoint)
	defer C.free(unsafe.Pointer(cEndpoint))
	s.mu.Lock()
	ret, err := C.zmq_connect(s.ptr, cEndpoint)
	s.mu.Unlock()
	if ret != 0 {
		return fmt.Errorf("zeromq: connect %q: %w", endpoint, err)
	}
	return nil
}

// Recv receives a message part from the socket
// and copies it into p.
// n is the number of bytes written to p.
// If p was not large enough to receive the entire message part,
// Recv will return n of len(p)
// and an error for which [IsTruncated] reports true.
// more is true when there are subsequent parts in a multi-part message
// that can be obtained through more calls to Recv.
//
// If the [context.Context] reports [context.Context.Done]
// before a message part has been received,
// then Recv returns an error that wraps the result of [context.Context.Err].
func (s *Socket) Recv(ctx context.Context, p []byte) (n int, more bool, err error) {
	var bufPtr unsafe.Pointer
	if len(p) > 0 {
		bufPtr = unsafe.Pointer(&p[0])
	}

	var cMore C.int
	err = retry(ctx, func() error {
		s.mu.Lock()
		ret, err := C.myrecv(s.ptr, bufPtr, C.size_t(len(p)), &cMore)
		s.mu.Unlock()
		if ret < 0 {
			return err
		}
		if int(ret) > len(p) {
			n = len(p)
			return truncatedError{int(ret) - len(p)}
		}
		n = int(ret)
		return nil
	})
	if err != nil {
		err = fmt.Errorf("zeromq: receive: %w", err)
	}
	return n, cMore != 0, err
}

// RecvMessage receives a message from the socket
// and copies it into buf.
// It returns slices that reference buf's backing array for each part.
// If buf was not large enough to receive the entire message,
// RecvMessage will return a slice with the number of parts in the message,
// but the parts at the end may be truncated or empty
// and an error for which [IsTruncated] reports true will be returned.
//
// If the [context.Context] reports [context.Context.Done]
// before a message has been received,
// then RecvMessage returns an error that wraps the result of [context.Context.Err].
func (s *Socket) RecvMessage(ctx context.Context, buf []byte) ([][]byte, error) {
	var firstError error
	var msg [][]byte
	for {
		n, more, err := s.Recv(ctx, buf)
		if err != nil && firstError == nil {
			firstError = err
		}
		msg = append(msg, buf[:n])
		buf = buf[n:]
		if !more {
			break
		}
	}
	return msg, firstError
}

// Send queues a message part on the socket.
// If more is true, then this message part is appended to a multi-part message
// that will be delivered atomically
// once the last message part is sent with more set to false.
// Once Send returns, the message has not necessarily been transmitted to the endpoint,
// but it is present in 0MQ's queue.
//
// If the [context.Context] reports [context.Context.Done]
// before a message part has been enqueued,
// then Recv returns an error that wraps the result of [context.Context.Err].
func (s *Socket) Send(ctx context.Context, p []byte, more bool) (n int, err error) {
	var bufPtr unsafe.Pointer
	if len(p) > 0 {
		bufPtr = unsafe.Pointer(&p[0])
	}
	flags := C.int(C.ZMQ_DONTWAIT)
	if more {
		flags |= C.ZMQ_SNDMORE
	}

	err = retry(ctx, func() error {
		s.mu.Lock()
		ret, err := C.zmq_send(s.ptr, bufPtr, C.size_t(len(p)), flags)
		s.mu.Unlock()
		if ret < 0 {
			return err
		}
		n = int(ret)
		return nil
	})
	if err != nil {
		err = fmt.Errorf("zeromq: send: %w", err)
	}
	return n, err
}

// SendMessage queues a multi-part message on the socket,
// returning the first error encountered.
// Once SendMessage returns, the message has not necessarily been transmitted to the endpoint,
// but it is present in 0MQ's queue.
func (s *Socket) SendMessage(ctx context.Context, msg [][]byte) error {
	for i, part := range msg {
		if _, err := s.Send(ctx, part, i < len(msg)-1); err != nil {
			return err
		}
	}
	return nil
}

func retry(ctx context.Context, f func() error) error {
	sleepTime := 10 * time.Microsecond
	const maxSleep = 10 * time.Millisecond
	var t *time.Timer
	for {
		err := f()
		switch err {
		case nil:
			return nil
		case syscall.EINTR:
			if err := ctx.Err(); err != nil {
				return err
			}
			// Retry without waiting.
			continue
		case syscall.EAGAIN:
			if t == nil {
				t = time.NewTimer(sleepTime)
			} else {
				t.Reset(sleepTime)
			}
			select {
			case <-t.C:
				sleepTime *= 2
				if sleepTime > maxSleep {
					sleepTime = maxSleep
				}
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			}
		default:
			return err
		}
	}
}

// IsTruncated reports whether the error indicates a truncated receive.
func IsTruncated(err error) bool {
	return errors.As(err, new(truncatedError))
}

type truncatedError struct {
	n int
}

func (e truncatedError) Error() string {
	return fmt.Sprintf("truncated %d bytes", e.n)
}

// SocketType is an enumeration of [0MQ socket types].
//
// [0MQ socket types]: http://api.zeromq.org/2-1:zmq-socket
type SocketType C.int

const (
	// TypePub is a publisher that distributes data.
	TypePub SocketType = C.ZMQ_PUB
	// TypeSub is a subscriber that subscribes to data distributed by a publisher.
	TypeSub SocketType = C.ZMQ_SUB
	// TypeReq is used by a client to send requests to and receive replies from a service.
	TypeReq SocketType = C.ZMQ_REQ
	// TypeRep is used by a service to receive requests from and send replies to a client.
	TypeRep SocketType = C.ZMQ_REP
	// TypeRouter is an advanced request/reply socket.
	// When receiving messages, a TypeRouter [Socket] shall prepend a message part
	// containing the identity of the originating peer to the message
	// before passing it to the application.
	// When sending messages, a TypeRouter [Socket] shall remove the first part of the message
	// and use it to determine the identity of the peer the message shall be routed to.
	// If the peer does not exist anymore the mssage shall be silently discarded.
	TypeRouter SocketType = C.ZMQ_ROUTER
)

// String returns the C API constant name (e.g. "ZMQ_PUB").
func (typ SocketType) String() string {
	switch typ {
	case TypePub:
		return "ZMQ_PUB"
	case TypeSub:
		return "ZMQ_SUB"
	case TypeReq:
		return "ZMQ_REQ"
	case TypeRep:
		return "ZMQ_REP"
	default:
		return fmt.Sprintf("SocketType(%d)", int(typ))
	}
}

var errClosed = errors.New("closed socket")
