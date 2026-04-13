package api

import "net"

// Server exposes the scheduler API over a unix domain socket.
// The same http.Handler will serve both unix socket (CLI) and
// TCP (future Web UI).
//
// TODO: implement
type Server struct {
	listener net.Listener
	// TODO: add scheduler, store dependencies
}
