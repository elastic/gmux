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
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
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
		srv, _ := startConfiguredServer(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			protoMajor = r.ProtoMajor
			handledHTTP = true
		}))

		client := srv.Client()
		clientTransport := client.Transport.(*http.Transport)
		clientTransport.ForceAttemptHTTP2 = forceAttemptHTTP2

		resp, err := client.Get(srv.URL)
		require.NoError(t, err)
		io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()

		assert.True(t, handledHTTP)
		assert.Equal(t, expectedProtoMajor, protoMajor)
	}
	t.Run("http/1", func(t *testing.T) { test(t, false, 1) })
	t.Run("http/2", func(t *testing.T) { test(t, true, 2) })
}

func TestServerHTTPRequestTLS(t *testing.T) {
	var rTLS *tls.ConnectionState
	srv, _ := startConfiguredServer(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rTLS = r.TLS
	}))

	client := srv.Client()
	resp, err := client.Get(srv.URL)
	require.NoError(t, err)
	io.Copy(ioutil.Discard, resp.Body)
	resp.Body.Close()

	assert.NotNil(t, rTLS)
}

func TestServerHTTPRequestTimeout(t *testing.T) {
	srv, _ := startConfiguredServer(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))

	client := srv.Client()
	client.Timeout = 100 * time.Millisecond
	_, err := client.Get(srv.URL)
	assert.Error(t, err)

	client.CloseIdleConnections()
	err = srv.Config.Shutdown(context.Background())
	assert.NoError(t, err)
}

func TestServerGRPCListener(t *testing.T) {
	var handledHTTP bool
	srv, grpcListener := startConfiguredServer(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv, grpcListener := startConfiguredServer(t, nil, nil)

	srv.Config.Shutdown(context.Background())
	conn, err := grpcListener.Accept()
	assert.EqualError(t, err, "listener closed")
	assert.Nil(t, conn)
}

func TestShutdownHTTPAndGRPCServers(t *testing.T) {
	srv, grpcListener := startConfiguredServer(t, nil, nil)
	assert.NotNil(t, grpcListener)

	var g errgroup.Group
	defer func() {
		assert.EqualError(t, g.Wait(), grpc.ErrServerStopped.Error())
	}()

	grpcServer := grpc.NewServer()
	pb.RegisterGreeterServer(grpcServer, &greeterServer{})
	g.Go(func() error { return grpcServer.Serve(grpcListener) })

	grpcClient := getGRPCClient(t, srv)

	// Stop grpc server first.
	grpcServer.GracefulStop()

	greeterClient := pb.NewGreeterClient(grpcClient)
	_, err := greeterClient.SayHello(context.Background(), &pb.HelloRequest{Name: "world"})
	assert.EqualError(t, err, "rpc error: code = Unavailable desc = error reading from server: EOF")
	assert.NoError(t, grpcClient.Close())

	// Then stop http server.
	err = srv.Config.Shutdown(context.Background())
	assert.NoError(t, err)
}

func TestGRPCInsecure(t *testing.T) {
	var handledHTTP bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handledHTTP = true
	})
	srv, grpcListener := newConfiguredServer(t, nil, handler)
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
	io.Copy(ioutil.Discard, resp.Body)
	resp.Body.Close()
	require.True(t, handledHTTP)
}

func BenchmarkGRPCWithTLS(b *testing.B) {
	srv, muxedGRPCListener := startConfiguredServer(b, nil, nil)
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
	srv, muxedGRPCListener := newConfiguredServer(b, nil, nil)
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

func startConfiguredServer(t testing.TB, conf *http2.Server, handler http.Handler) (srv *httptest.Server, grpcListener net.Listener) {
	t.Helper()
	srv, grpcListener = newConfiguredServer(t, conf, handler)
	srv.StartTLS()
	return srv, grpcListener
}

func newConfiguredServer(t testing.TB, conf *http2.Server, handler http.Handler) (srv *httptest.Server, grpcListener net.Listener) {
	t.Helper()
	srv = httptest.NewUnstartedServer(handler)
	grpcListener, err := gmux.ConfigureServer(srv.Config, conf)
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
