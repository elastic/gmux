# gmux

gmux is a Go library for simultaneously serving net/http and gRPC requests on a single port.

gmux is derived from [cmux](https://github.com/soheilhy/cmux), enabling mTLS at the expense of
reduced flexibility. Whereas cmux operates at the transport layer and enables multiplexing
arbitrary application layer protocols, gmux operates at the application layer (on top of HTTP
with or without TLS), to enable better integration with net/http and crypto/tls.

## Example

```go
s := &http.Server{
	Addr:    ":8080",
	Handler: myHandler,
}

// Configure the server with gmux. The returned net.Listener will receive gRPC connections,
// while all other requests will be handled by s.Handler.
grpcListener, err := gmux.Configure(s)
if err != nil {
	log.Fatal(err)
}

grpcServer := grpc.NewServer()
go grpcServer.Serve(grpcListener)
log.Fatal(s.ListenAndServeTLS("cert.pem", "key.pem"))
```

## HTTP/2 configuration

gmux configures HTTP/2 on the `http.Server` itself: it adds `h2` to `TLSConfig.NextProtos` and
installs the `TLSNextProto["h2"]` handler it uses to inspect connections. Do not also call
`golang.org/x/net/http2.ConfigureServer` on the same server. On Go 1.27 and later, doing so hands
HTTP/2 to `net/http`'s built-in implementation, which bypasses gmux, and gRPC requests will be
served by the HTTP handler instead.

To tune HTTP/2 for the connections handled by `s.Handler`, set `s.HTTP2` (`*http.HTTP2Config`)
and `s.IdleTimeout`. `gmux.ConfigureServer`, which took a `*golang.org/x/net/http2.Server`, has
been replaced by `gmux.Configure`; move any settings from the `http2.Server` to `s.HTTP2`.

gmux requires Go 1.27 or later. Earlier Go versions can continue to use gmux v0.3.x.

## License

This software is licensed under the [Apache License 2.0](./LICENSE).

