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

package gmux_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	pb "google.golang.org/grpc/examples/helloworld/helloworld"

	"github.com/elastic/gmux"
)

func TestServerHTTPRequest(t *testing.T) {
	test := func(t *testing.T, forceAttemptHTTP2 bool, expectedProtoMajor int) {
		var protoMajor int
		var handledHTTP bool
		srv, _ := startConfiguredServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			protoMajor = r.ProtoMajor
			handledHTTP = true
		}))

		client := srv.Client()
		clientTransport := client.Transport.(*http.Transport)
		clientTransport.ForceAttemptHTTP2 = forceAttemptHTTP2

		resp, err := client.Get(srv.URL)
		require.NoError(t, err)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		assert.True(t, handledHTTP)
		assert.Equal(t, expectedProtoMajor, protoMajor)
	}
	t.Run("http/1", func(t *testing.T) { test(t, false, 1) })
	t.Run("http/2", func(t *testing.T) { test(t, true, 2) })
}

func TestServerHTTPRequestTLS(t *testing.T) {
	var rTLS *tls.ConnectionState
	srv, _ := startConfiguredServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rTLS = r.TLS
	}))

	client := srv.Client()
	resp, err := client.Get(srv.URL)
	require.NoError(t, err)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	assert.NotNil(t, rTLS)
}

func TestServerGRPCListener(t *testing.T) {
	var handledHTTP bool
	srv, grpcListener := startConfiguredServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handledHTTP = true
	}))
	assert.NotNil(t, grpcListener)

	var g errgroup.Group
	defer func() {
		assert.NoError(t, g.Wait())
	}()

	grpcServer := grpc.NewServer()
	pb.RegisterGreeterServer(grpcServer, &greeterServer{})
	g.Go(func() error { return grpcServer.Serve(grpcListener) })
	defer grpcServer.Stop()

	grpcClient := getGRPCClient(t, srv)
	greeterClient := pb.NewGreeterClient(grpcClient)
	reply, err := greeterClient.SayHello(context.Background(), &pb.HelloRequest{Name: "world"})
	assert.NoError(t, err)
	assert.Equal(t, "Hello, world!", reply.Message)
	assert.NoError(t, grpcClient.Close())

	require.False(t, handledHTTP)
}

func TestShutdownListenerClosed(t *testing.T) {
	srv, grpcListener := startConfiguredServer(t, nil)

	srv.Config.Shutdown(context.Background())
	conn, err := grpcListener.Accept()
	assert.EqualError(t, err, "listener closed")
	assert.Nil(t, conn)
}

func TestShutdownGRPCServerBeforeHTTPServer(t *testing.T) {
	// Although it is recommended to shutdown HTTP server before GRPC server,
	// make sure it does not panic when they are closed in reversed order.

	srv, grpcListener := startConfiguredServer(t, nil)
	assert.NotNil(t, grpcListener)

	var g errgroup.Group

	grpcServer := grpc.NewServer()
	pb.RegisterGreeterServer(grpcServer, &greeterServer{})
	g.Go(func() error { return grpcServer.Serve(grpcListener) })

	grpcClient := getGRPCClient(t, srv)
	greeterClient := pb.NewGreeterClient(grpcClient)
	_, err := greeterClient.SayHello(context.Background(), &pb.HelloRequest{Name: "world"})
	assert.NoError(t, err)
	assert.NoError(t, grpcClient.Close())

	// Stop grpc server first.
	grpcServer.GracefulStop()

	grpcClient = getGRPCClient(t, srv)
	greeterClient = pb.NewGreeterClient(grpcClient)
	_, err = greeterClient.SayHello(context.Background(), &pb.HelloRequest{Name: "world"})
	assert.EqualError(t, err, "rpc error: code = Unavailable desc = error reading from server: EOF")
	assert.NoError(t, grpcClient.Close())

	// Then stop http server.
	err = srv.Config.Shutdown(context.Background())
	assert.NoError(t, err)

	assert.NoError(t, g.Wait())
}

func TestShutdownGracefulHTTP2(t *testing.T) {
	// Shutdown of the HTTP server must send GOAWAY to HTTP/2 connections
	// handled by gmux, and wait for in-flight requests to complete.
	started := make(chan struct{})
	release := make(chan struct{})
	srv, _ := startConfiguredServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		io.WriteString(w, "done")
	}))

	type result struct {
		resp *http.Response
		body string
		err  error
	}
	results := make(chan result, 1)
	go func() {
		resp, err := srv.Client().Get(srv.URL)
		if err != nil {
			results <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		results <- result{resp: resp, body: string(body), err: err}
	}()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for request to start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- srv.Config.Shutdown(ctx) }()

	// Shutdown must not complete while the request is in flight.
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned (%v) before in-flight request completed", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case res := <-results:
		require.NoError(t, res.err)
		assert.Equal(t, http.StatusOK, res.resp.StatusCode)
		assert.Equal(t, 2, res.resp.ProtoMajor, "expected request to be served over HTTP/2")
		assert.Equal(t, "done", res.body)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for response")
	}
	select {
	case err := <-shutdownDone:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for Shutdown")
	}
}

func TestGRPCInsecure(t *testing.T) {
	var handledHTTP bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handledHTTP = true
	})
	srv, grpcListener := newConfiguredServer(t, handler)
	srv.EnableHTTP2 = false
	srv.Start()

	var g errgroup.Group
	defer func() {
		assert.NoError(t, g.Wait())
	}()

	grpcServer := grpc.NewServer()
	pb.RegisterGreeterServer(grpcServer, &greeterServer{})
	g.Go(func() error { return grpcServer.Serve(grpcListener) })
	defer grpcServer.Stop()

	grpcClient, err := grpc.Dial(srv.Listener.Addr().String(), grpc.WithInsecure())
	require.NoError(t, err)
	greeterClient := pb.NewGreeterClient(grpcClient)
	reply, err := greeterClient.SayHello(context.Background(), &pb.HelloRequest{Name: "world"})
	require.NoError(t, err)
	assert.Equal(t, "Hello, world!", reply.Message)
	assert.NoError(t, grpcClient.Close())
	require.False(t, handledHTTP)

	// Non-gRPC requests go through to the configured net/http.Handler.
	resp, err := srv.Client().Get(srv.URL)
	require.NoError(t, err)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	require.True(t, handledHTTP)
}

func BenchmarkGRPCWithTLS(b *testing.B) {
	srv, muxedGRPCListener := startConfiguredServer(b, nil)
	directGRPCListener, err := tls.Listen("tcp", "localhost:0", srv.TLS)
	require.NoError(b, err)
	defer directGRPCListener.Close()

	var g errgroup.Group
	defer func() {
		assert.NoError(b, g.Wait())
	}()

	grpcServer := grpc.NewServer()
	pb.RegisterGreeterServer(grpcServer, &greeterServer{})
	g.Go(func() error { return grpcServer.Serve(muxedGRPCListener) })
	g.Go(func() error { return grpcServer.Serve(directGRPCListener) })
	defer grpcServer.Stop()

	clientTLSConfig := srv.Client().Transport.(*http.Transport).TLSClientConfig
	dial := func(b *testing.B, lis net.Listener, opts ...grpc.DialOption) *grpc.ClientConn {
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(clientTLSConfig)))
		client, err := grpc.Dial(lis.Addr().String(), opts...)
		if err != nil {
			b.Fatal(err)
		}
		return client
	}
	benchmarkGRPC(b, dial, directGRPCListener, srv.Listener)
}

func BenchmarkGRPCInsecure(b *testing.B) {
	srv, muxedGRPCListener := newConfiguredServer(b, nil)
	srv.EnableHTTP2 = false
	srv.Start()

	directGRPCListener, err := net.Listen("tcp", "localhost:0")
	require.NoError(b, err)
	defer directGRPCListener.Close()

	var g errgroup.Group
	defer func() {
		assert.NoError(b, g.Wait())
	}()

	grpcServer := grpc.NewServer()
	pb.RegisterGreeterServer(grpcServer, &greeterServer{})
	g.Go(func() error { return grpcServer.Serve(muxedGRPCListener) })
	g.Go(func() error { return grpcServer.Serve(directGRPCListener) })
	defer grpcServer.Stop()

	dial := func(b *testing.B, lis net.Listener, opts ...grpc.DialOption) *grpc.ClientConn {
		opts = append(opts, grpc.WithInsecure())
		client, err := grpc.Dial(lis.Addr().String(), opts...)
		if err != nil {
			b.Fatal(err)
		}
		return client
	}
	benchmarkGRPC(b, dial, directGRPCListener, srv.Listener)
}

func benchmarkGRPC(b *testing.B, dial dialFunc, directListener, muxedListener net.Listener) {
	b.Run("longrunning", func(b *testing.B) {
		directClient := dial(b, directListener)
		b.Cleanup(func() { directClient.Close() })
		muxedClient := dial(b, muxedListener)
		b.Cleanup(func() { muxedClient.Close() })
		bench := func(b *testing.B, grpcClient *grpc.ClientConn) {
			greeterClient := pb.NewGreeterClient(grpcClient)
			req := &pb.HelloRequest{Name: "world"}
			for i := 0; i < b.N; i++ {
				_, err := greeterClient.SayHello(context.Background(), req)
				if err != nil {
					b.Error(err)
				}
			}
		}
		b.Run("direct", func(b *testing.B) {
			bench(b, directClient)
		})
		b.Run("muxed", func(b *testing.B) {
			bench(b, muxedClient)
		})
	})
	b.Run("shortlived", func(b *testing.B) {
		bench := func(b *testing.B, lis net.Listener) {
			for i := 0; i < b.N; i++ {
				client := dial(b, lis)
				greeterClient := pb.NewGreeterClient(client)
				req := &pb.HelloRequest{Name: "world"}
				_, err := greeterClient.SayHello(context.Background(), req)
				if err != nil {
					b.Error(err)
				}
				client.Close()
			}
		}
		b.Run("direct", func(b *testing.B) {
			bench(b, directListener)
		})
		b.Run("muxed", func(b *testing.B) {
			bench(b, muxedListener)
		})
	})
}

type dialFunc func(b *testing.B, lis net.Listener, opts ...grpc.DialOption) *grpc.ClientConn

func startConfiguredServer(t testing.TB, handler http.Handler) (srv *httptest.Server, grpcListener net.Listener) {
	t.Helper()
	srv, grpcListener = newConfiguredServer(t, handler)
	srv.StartTLS()
	return srv, grpcListener
}

func newConfiguredServer(t testing.TB, handler http.Handler) (srv *httptest.Server, grpcListener net.Listener) {
	t.Helper()
	srv = httptest.NewUnstartedServer(handler)
	grpcListener, err := gmux.Configure(srv.Config)
	if err != nil {
		t.Fatal(err)
	}
	srv.EnableHTTP2 = true
	t.Cleanup(srv.Close)
	return srv, grpcListener
}

type greeterServer struct {
	pb.UnimplementedGreeterServer
}

func (s *greeterServer) SayHello(ctx context.Context, req *pb.HelloRequest) (*pb.HelloReply, error) {
	return &pb.HelloReply{Message: fmt.Sprintf("Hello, %s!", req.Name)}, nil
}

func getGRPCClient(t *testing.T, srv *httptest.Server) *grpc.ClientConn {
	srvURL, err := url.Parse(srv.URL)
	require.NoError(t, err)

	clientTLSConfig := srv.Client().Transport.(*http.Transport).TLSClientConfig
	grpcClient, err := grpc.Dial(srvURL.Host, grpc.WithTransportCredentials(credentials.NewTLS(clientTLSConfig)))
	require.NoError(t, err)
	return grpcClient
}
