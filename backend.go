// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package gmux

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"slices"
	"sync"

	"golang.org/x/net/http2"
)

// httpBackend serves the HTTP/2 connections that gmux does not route to gRPC,
// using a dedicated net/http.Server.
//
// Since Go 1.27, net/http serves HTTP/2 with an internal implementation, and
// golang.org/x/net/http2.ConfigureServer (with x/net v0.55.0 or later)
// registers that implementation on the http.Server. When it is registered,
// net/http serves "h2" connections itself and never invokes the TLSNextProto
// handler gmux relies on to inspect connections.
//
// gmux therefore configures ALPN and TLSNextProto on the caller's server by
// itself, and serves the HTTP/2 connections it does not route to gRPC through
// a separate http.Server fed by a channel-backed listener. net/http negotiates
// ALPN on any net.Conn that exposes a ConnectionState method, so these
// connections are served as HTTP/2 with the caller's HTTP/2 configuration,
// and the backend server's Shutdown sends GOAWAY to them.
type httpBackend struct {
	server   *http.Server
	listener *chanListener
}

func newHTTPBackend(srv *http.Server) (*httpBackend, error) {
	if _, ok := srv.TLSNextProto[http2.NextProtoTLS]; ok {
		return nil, errors.New(`gmux: srv already has an "h2" TLSNextProto handler; ` +
			`do not configure HTTP/2 (e.g. with golang.org/x/net/http2.ConfigureServer) before calling gmux.Configure`)
	}
	if srv.TLSConfig == nil {
		srv.TLSConfig = new(tls.Config)
	}
	if err := checkCipherSuites(srv.TLSConfig); err != nil {
		return nil, err
	}
	for _, proto := range []string{http2.NextProtoTLS, "http/1.1"} {
		if !slices.Contains(srv.TLSConfig.NextProtos, proto) {
			srv.TLSConfig.NextProtos = append(srv.TLSConfig.NextProtos, proto)
		}
	}

	var http2Config *http.HTTP2Config
	if srv.HTTP2 != nil {
		c := *srv.HTTP2
		http2Config = &c
	}

	backend := &http.Server{
		// Resolve srv.Handler at request time, as net/http itself does, so
		// that the backend follows any later changes to srv.Handler.
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handler := srv.Handler
			if handler == nil {
				handler = http.DefaultServeMux
			}
			handler.ServeHTTP(w, r)
		}),
		ReadTimeout:           srv.ReadTimeout,
		ReadHeaderTimeout:     srv.ReadHeaderTimeout,
		WriteTimeout:          srv.WriteTimeout,
		IdleTimeout:           srv.IdleTimeout,
		MaxHeaderBytes:        srv.MaxHeaderBytes,
		MaxHeaderValueCount:   srv.MaxHeaderValueCount,
		DisableClientPriority: srv.DisableClientPriority,
		ErrorLog:              srv.ErrorLog,
		HTTP2:                 http2Config,
		// Resolve srv.ConnState at call time. The probe connection is not
		// reported: it is internal to gmux, and its final state change is
		// delivered after Configure returns, when callers may still be
		// configuring srv.
		ConnState: func(c net.Conn, state http.ConnState) {
			if bc, ok := c.(*backendConn); ok && bc.probe {
				return
			}
			if f := srv.ConnState; f != nil {
				f(c, state)
			}
		},
		// Graft the per-connection context that net/http created for the
		// client connection on srv, so that handlers observe srv.BaseContext
		// and srv.ConnContext values, and http.ServerContextKey refers to srv.
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if bc, ok := c.(*backendConn); ok && bc.ctx != nil {
				return bc.ctx
			}
			return ctx
		},
		// TLSConfig is deliberately left nil: net/http enables HTTP/2 on Serve
		// when no TLSConfig is set, and the backend never performs handshakes.
	}
	lis := newChanListener()
	go backend.Serve(lis) // returns http.ErrServerClosed on shutdown
	return &httpBackend{server: backend, listener: lis}, nil
}

// checkCipherSuites mirrors the validation historically performed by
// golang.org/x/net/http2.ConfigureServer.
func checkCipherSuites(cfg *tls.Config) error {
	if cfg.CipherSuites == nil || cfg.MinVersion >= tls.VersionTLS13 {
		return nil
	}
	for _, cs := range cfg.CipherSuites {
		switch cs {
		case tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256:
			return nil
		}
	}
	return errors.New("gmux: TLSConfig.CipherSuites is missing an HTTP/2-required AES_128_GCM_SHA256 cipher " +
		"(need at least one of TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256 or TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256)")
}

// backendConn is a connection handed to the backend http.Server. It carries
// the TLS state of the client connection so that net/http negotiates HTTP/2
// for it, and the per-connection context created by the configured server.
type backendConn struct {
	net.Conn
	ctx       context.Context
	state     tls.ConnectionState
	probe     bool // set for the initial SETTINGS probe connection
	closeOnce sync.Once
	closed    chan struct{}
}

func (c *backendConn) ConnectionState() tls.ConnectionState {
	return c.state
}

func (c *backendConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// serveConn serves HTTP/2 on conn, blocking until the connection is closed.
//
// ctx is the per-connection base context supplied by net/http, and may be
// nil. closed is closed when conn is closed.
func (b *httpBackend) serveConn(ctx context.Context, conn net.Conn, closed <-chan struct{}) {
	var state tls.ConnectionState
	if cs, ok := conn.(connectionStater); ok {
		state = cs.ConnectionState()
	}
	b.serve(&backendConn{Conn: conn, ctx: ctx, state: state, closed: make(chan struct{})}, closed)
}

// probeTLSState is the connection state presented to the backend for the
// initial SETTINGS probe. net/http requires TLS 1.2 or later and a permitted
// cipher suite before serving HTTP/2 on a connection.
var probeTLSState = tls.ConnectionState{
	Version:            tls.VersionTLS13,
	HandshakeComplete:  true,
	CipherSuite:        tls.TLS_AES_128_GCM_SHA256,
	NegotiatedProtocol: http2.NextProtoTLS,
}

// probeServe serves HTTP/2 on a connection that has no TLS state, blocking
// until the connection is closed. It is used to introspect the initial
// SETTINGS frame emitted by the backend.
func (b *httpBackend) probeServe(conn net.Conn) {
	b.serve(&backendConn{Conn: conn, state: probeTLSState, probe: true, closed: make(chan struct{})}, nil)
}

// serve hands conn to the backend server and blocks until it is closed.
// closed, if non-nil, aborts the handoff if the client connection is closed first.
func (b *httpBackend) serve(conn *backendConn, closed <-chan struct{}) {
	select {
	case b.listener.conns <- conn:
	case <-b.listener.closed:
		conn.Close()
		return
	case <-closed:
		return
	}
	<-conn.closed
}

// shutdown sends GOAWAY to the connections served by the backend and waits
// for them to drain. The configured server's Shutdown waits for its own
// connections, which includes the client connections these are proxied from.
func (b *httpBackend) shutdown() {
	b.server.Shutdown(context.Background())
}
