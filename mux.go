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
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"golang.org/x/sync/errgroup"
)

const (
	http2FrameHeaderLength = 9

	grpcContentType = "application/grpc"
)

// mux supports multiplexing plain-old HTTP/2 and gRPC traffic
// on a single listener.
type mux struct {
	http2Server *http2.Server
	grpcConns   chan<- net.Conn
}

// ConfigureServer configures srv to identify gRPC connections and send them
// to the returned net.Listener, suitable for passing to grpc.Server.Serve,
// while all other HTTP requests will be handled by srv.
//
// ConfigureServer works with or without TLS enabled.
//
// When TLS is enabled, ConfigureServer relies on ALPN. ConfigureServer
// internally calls http2.ConfigureServer(srv, conf) to configure HTTP/2 support,
// and defines an alternative srv.TLSNextProto "h2" handler. When using TLS, the
// gRPC listener returns secure connections; the gRPC server must not also be
// configured to wrap the connection with TLS.
//
// When TLS is not enabled, ConfigureServer relies on h2c prior knowledge,
// wrapping srv.Handler. It is therefore necessary to set srv.Handler before
// calling ConfigureServer.
//
// The returned listener will be closed when srv.Shutdown is called. The
// returned listener's Addr() method does not correspond to the configured
// HTTP server's listener(s) in any way, and cannot be relied upon for forming
// a connection URL.
func ConfigureServer(srv *http.Server, conf *http2.Server) (grpcListener net.Listener, _ error) {
	if err := http2.ConfigureServer(srv, conf); err != nil {
		return nil, err
	}
	if conf == nil {
		conf = new(http2.Server)
	}
	glis := newChanListener()
	mux := &mux{http2Server: conf, grpcConns: glis.conns}
	srv.Handler = mux.withGRPCInsecure(srv.Handler)
	srv.TLSNextProto[http2.NextProtoTLS] = func(srv *http.Server, conn *tls.Conn, h http.Handler) {
		err := mux.handleH2(srv, conn, h)
		if err != nil && srv.ErrorLog != nil {
			srv.ErrorLog.Printf("handleH2 (%s) returned an error: %s", conn.RemoteAddr(), err)
		}
	}
	srv.RegisterOnShutdown(func() { glis.Close() })
	return glis, nil
}

// withGRPCInsecure wraps next such that h2c (HTTP/2 Cleartext) gRPC requests
// are hijacked and sent to the gRPC listener, and all other HTTP requests are
// handled by next.
//
// See https://httpwg.org/specs/rfc7540.html#rfc.section.3.4
func (m *mux) withGRPCInsecure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil && r.Method == "PRI" && len(r.Header) == 0 && r.URL.Path == "*" && r.Proto == "HTTP/2.0" {
			hijacker, ok := w.(http.Hijacker)
			if ok {
				conn, rw, err := hijacker.Hijack()
				if err != nil {
					panic(fmt.Sprintf("Hijack failed: %v", err))
				}
				defer conn.Close()

				// We just identify that we're dealing with a
				// prior-knowledge connection, and pass it straight
				// through to the gRPC server.
				preface := "PRI * HTTP/2.0\r\n\r\n"
				r := io.MultiReader(strings.NewReader(preface), rw, conn)
				pc, closed := newProxyConn(conn, r, conn)
				m.handleGRPC(nil, pc, closed, nil)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (m *mux) handleH2(srv *http.Server, conn net.Conn, handler http.Handler) error {
	var clientReadBuf bytes.Buffer
	connHandler, err := m.getConnHandler(conn, &clientReadBuf)
	if err != nil {
		return err
	}

	// FIXME(axw) less arcane pipe r/w names
	var g errgroup.Group
	rp2s, wp2s := io.Pipe()
	rs2p, ws2p := io.Pipe()
	g.Go(func() (res error) { // read frames from conn, write to pipe
		_, err := io.Copy(wp2s, &clientReadBuf)
		if err != nil {
			wp2s.CloseWithError(err)
			return err
		}
		_, err = io.Copy(wp2s, conn)
		wp2s.CloseWithError(err)
		return err
	})
	g.Go(func() (res error) { // read frames from pipe, write to conn
		defer func() { rs2p.CloseWithError(res) }()
		var framesBuf bytes.Buffer
		framer := http2.NewFramer(conn, io.TeeReader(rs2p, &framesBuf))
		framer.SetReuseFrames()
		// Wait for the server to send an ACK to the client's SETTINGS,
		// filtering it out and forwarding everything else.
		var haveFirstSettingsACK bool
		for !haveFirstSettingsACK {
			f, err := framer.ReadFrame()
			if err != nil {
				return err
			}
			switch f := f.(type) {
			case *http2.SettingsFrame:
				if !haveFirstSettingsACK && f.IsAck() {
					// Ignore first ACK, as the client's
					// SETTINGS has already been ACKed.
					haveFirstSettingsACK = true
					framesBuf.Truncate(framesBuf.Len() - int(f.Length) - http2FrameHeaderLength)
					break
				}
			}
		}
		if _, err := io.Copy(conn, &framesBuf); err != nil {
			return err
		}
		_, err = io.Copy(conn, rs2p)
		return err
	})
	g.Go(func() (res error) {
		proxyConn, closed := newProxyConn(conn, rp2s, ws2p)
		err := connHandler(srv, proxyConn, closed, handler)
		ws2p.CloseWithError(err)
		rp2s.CloseWithError(err)
		return err
	})
	return g.Wait()
}

// getConnHandler handles a new client connection, writing a SETTINGS
// request to the client, followed by reading the HTTP/2 client preface,
// and then finally looking for a Content-Type header to determine which
// connection handler to return.
//
// All data read from the client will be written to buf, which will be
// replayed to the backend HTTP/2 server.
func (m *mux) getConnHandler(conn net.Conn, buf *bytes.Buffer) (connHandlerFunc, error) {
	rbuf := io.TeeReader(conn, buf)
	framer := http2.NewFramer(conn, rbuf)
	framer.SetReuseFrames()

	// Client expects SETTINGS first, so send empty initial settings.
	// The real server will send a new one with the real settings.
	//
	// When replaying frames to the real server, we'll need to suppress
	// the ACK for this frame, which the server won't know about.
	if err := framer.WriteSettings(); err != nil {
		return nil, err
	}

	// Read client preface. We don't bother verifying it here, as it will
	// be verified later by the real http2.Server.
	var preface [len(http2.ClientPreface)]byte
	if _, err := io.ReadFull(rbuf, preface[:]); err != nil {
		return nil, err
	}

	contentType, err := m.getContentType(framer, buf)
	if err != nil {
		return nil, err
	}
	connHandler := m.handleHTTP
	if contentType == grpcContentType {
		connHandler = m.handleGRPC
	}
	return connHandler, nil
}

var decoderPool = sync.Pool{
	New: func() interface{} {
		out := &decoder{}
		out.d = hpack.NewDecoder(4096, func(hf hpack.HeaderField) {
			if hf.Name == "content-type" {
				out.contentType = hf.Value
			}
		})
		return out
	},
}

type decoder struct {
	d           *hpack.Decoder
	contentType string
}

func (m *mux) getContentType(framer *http2.Framer, framesBuf *bytes.Buffer) (contentType string, _ error) {
	// Code based on https://github.com/soheilhy/cmux
	//
	// Copyright 2016 The CMux Authors. All rights reserved.
	dec := decoderPool.Get().(*decoder)

	// Read frames until we have the content-type header, or we know there isn't one.
	var haveFirstSettings bool
	var haveFirstSettingsACK bool
	var haveEndHeaders bool
	for (dec.contentType == "" && !haveEndHeaders) || !haveFirstSettings || !haveFirstSettingsACK {
		f, err := framer.ReadFrame()
		if err != nil {
			return "", err
		}

		switch f := f.(type) {
		case *http2.SettingsFrame:
			switch {
			case !haveFirstSettingsACK && f.IsAck():
				haveFirstSettingsACK = true
				// We accept the ACK, and omit it from the frames
				// written to the real server.
				framesBuf.Truncate(framesBuf.Len() - int(f.Length) - http2FrameHeaderLength)
			case !haveFirstSettings && !f.IsAck():
				haveFirstSettings = true
				// We ACK the client's first SETTINGS to unblock it,
				// and ignore the first ACK from the real server.
				if err := framer.WriteSettingsAck(); err != nil {
					return "", err
				}
			}
		case *http2.ContinuationFrame:
			if _, err := dec.d.Write(f.HeaderBlockFragment()); err != nil {
				return "", err
			}
			haveEndHeaders = f.FrameHeader.Flags&http2.FlagHeadersEndHeaders != 0
		case *http2.HeadersFrame:
			if _, err := dec.d.Write(f.HeaderBlockFragment()); err != nil {
				return "", err
			}
			haveEndHeaders = f.FrameHeader.Flags&http2.FlagHeadersEndHeaders != 0
		}
	}
	contentType = dec.contentType
	if dec.d.Close() == nil {
		dec.contentType = ""
		decoderPool.Put(dec)
	}
	return contentType, nil
}

type connHandlerFunc func(srv *http.Server, conn net.Conn, closed <-chan struct{}, handler http.Handler) error

func (m *mux) handleHTTP(srv *http.Server, conn net.Conn, closed <-chan struct{}, handler http.Handler) error {
	// This code is adapted from x/net/http2 to not assume tls.Conn.

	// The TLSNextProto interface predates contexts, so the net/http package passes
	// down its per-connection base context via an exported but unadvertised method
	// on the Handler. This is for internal net/http<=>http2 use only.
	var ctx context.Context
	type baseContexter interface {
		BaseContext() context.Context
	}
	if bc, ok := handler.(baseContexter); ok {
		ctx = bc.BaseContext()
	}
	m.http2Server.ServeConn(conn, &http2.ServeConnOpts{
		Context:    ctx,
		Handler:    handler,
		BaseConfig: srv,
	})
	return nil
}

func (m *mux) handleGRPC(_ *http.Server, conn net.Conn, closed <-chan struct{}, _ http.Handler) error {
	select {
	case m.grpcConns <- conn:
	case <-closed:
		// Connection closed before it could be handled.
		return nil
	}
	<-closed
	return nil
}
