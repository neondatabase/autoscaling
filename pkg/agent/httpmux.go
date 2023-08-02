package agent

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// HttpMuxServer provides a way to multiplex multiple http.ServeMux
// instances on a single HTTP server, so that requests are routed to
// the correct ServeMux based on a prefix at the beginning of the URL
// path.
//
// For example, if you call RegisterMux("foo", myMux), then all requests to
// "/foo/*" are routed to 'myMux' instance.
//
// (Why is this needed? You could register all the routes directly in
// one giant http.ServeMux, but http.ServeMux doesn't provide any way
// to unregister patterns.)
type HttpMuxServer struct {
	muxedServers map[string]*http.ServeMux
	lock         sync.Mutex // protects muxedServers
	logger       *zap.Logger
}

// Implements http.Handler
func (s *HttpMuxServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug(fmt.Sprintf("MUX: received request: %s", r.URL.Path))

	path, found := strings.CutPrefix(r.URL.Path, "/")
	if !found {
		w.WriteHeader(http.StatusBadRequest)
		s.logger.Warn(fmt.Sprintf("MUX: invalid request (path doesn't start with '/'): %s", r.URL.Path))
		_, _ = w.Write([]byte("missing /"))
		return
	}

	muxID, path, found := strings.Cut(path, "/")
	if !found {
		w.WriteHeader(http.StatusBadRequest)
		s.logger.Warn(fmt.Sprintf("MUX: invalid request (no mux ID in path): %s", r.URL.Path))
		_, _ = w.Write([]byte("request must start with mux ID"))
		return
	}

	s.lock.Lock()
	subServer := s.muxedServers[muxID]
	s.lock.Unlock()
	if subServer == nil {
		// muxed service not found
		w.WriteHeader(http.StatusNotAcceptable)
		s.logger.Warn(fmt.Sprintf("MUX: could not route request, mux ID not found): %s", r.URL.Path))
		_, _ = w.Write([]byte("mux ID not found"))
		return
	}

	// Change the path in the request, removing the muxID prefix.
	//
	// The documentation for ServeHTTP says that you should not
	// modify the Request, so make a copy.
	var newURL url.URL = *r.URL
	newURL.Path = "/" + path
	var newRequest http.Request = *r
	newRequest.URL = &newURL

	subServer.ServeHTTP(w, &newRequest)
}

func (s *HttpMuxServer) RegisterMux(muxID string, subServer *http.ServeMux) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.muxedServers[muxID] != nil {
		return fmt.Errorf("mux ID already in use")
	}
	s.muxedServers[muxID] = subServer
	return nil
}

func (s *HttpMuxServer) UnregisterMux(muxID string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.muxedServers[muxID] = nil
}

// Create a new HttpMuxServer, listening on the given port
func StartHttpMuxServer(
	logger *zap.Logger,
	port int,
) (*HttpMuxServer, error) {
	// Manually start the TCP listener so we can minimize errors in the background thread.
	addr := net.TCPAddr{IP: net.IPv4zero, Port: port}
	listener, err := net.ListenTCP("tcp", &addr)
	if err != nil {
		return nil, fmt.Errorf("Error binding to %v", addr)
	}

	muxServer := HttpMuxServer{
		muxedServers: map[string]*http.ServeMux{},
		lock:         sync.Mutex{},
		logger:       logger,
	}

	httpServer := &http.Server{
		Handler: &muxServer,
	}

	// Main thread running the server.
	go func() {
		err := httpServer.Serve(listener)

		// The Serve call should never return
		panic(fmt.Errorf("muxed http server exited unexpectedly: %w", err))
	}()
	return &muxServer, nil
}
