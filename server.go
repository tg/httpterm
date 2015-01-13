// Package httpterm provides closable http.Server with extended read timeouts.
package httpterm

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

const (
	// StateHead represents a connection that has read 1 or more bytes of
	// a request. Contrary to http.StateActive, the server.ConnState hook fires
	// before processing the data (headers). Connections transition from
	// StateHead to http.StateActive or http.Closed.
	StateHead http.ConnState = -1
)

// ErrClosing indicating that operation is not allowed as server is closing
var ErrClosing = errors.New("server closing")

// Server embeds http.Server and provides additional functionality.
// All the http.Server can be accessed directly and behaves as decribed in
// the original docs at http://golang.org/pkg/net/http/#Server.
//
// Assigning non-zero ReadTimeout is not advised, as it doesn't work well
// with better defined read timeout extensions provided by this class.
//
// ConnState function will be overwritten after call to Serve, but the original
// value will be preserved internally and called as expected.
// An additional value of StateHead will be passed to the function on top of
// the regular http.ConnState values.
type Server struct {
	http.Server

	// CloseOnSignal enables server shutdown on SIGTERM/SIGNINT.
	// Signal handler is registered in Serve() method.
	CloseOnSignal bool

	// HeadReadTimeout defines timeout for reading request headers.
	HeadReadTimeout time.Duration

	// BodyReadTimeout defines timeout for reading request body.
	// This timeout is being applied just before calling the request handler.
	BodyReadTimeout time.Duration

	// IdleTimeout defines for how long connection can be idle between requests.
	IdleTimeout time.Duration

	// NewAsActive prevents new connections from being idle before sending
	// first request. If set, new connections will have HeadReadTimeout applied.
	// If server is behind some proxy or a load balancer which maintains
	// a permanent connection, setting up this flag is not recommended.
	NewAsActive bool

	listener *rtListener

	lock    sync.Mutex
	closing bool

	// conns is a map of connections which indicates whether connection is active,
	// i.e. there a request being processed (including header handling)
	conns map[net.Conn]bool
}

// Serve behaves as http.Server.Serve.
// See: http://golang.org/pkg/net/http/#Server.Serve
//
// Along with an error, pending channel is returned which will be closed once
// all connections are closed or hijacked.
func (s *Server) Serve(l net.Listener) (pending <-chan bool, err error) {
	s.conns = make(map[net.Conn]bool)

	oldConnState := s.ConnState
	newConnState := func(c net.Conn, state http.ConnState) {
		s.updateConnState(c, state)
		// Pass to original handler
		if oldConnState != nil {
			oldConnState(c, state)
		}
	}

	s.ConnState = newConnState

	// Wrap with custom listener
	s.listener = &rtListener{
		Listener:    l,
		newAsActive: s.NewAsActive,
		callback:    func(c net.Conn) { newConnState(c, StateHead) },
	}

	// Register signal handling for shutdown if requested
	if s.CloseOnSignal {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-c
			s.Close()
		}()
	}

	// Serve loop
	err = s.Server.Serve(s.listener)

	// Clear error if server closing
	s.lock.Lock()
	if s.closing {
		err = nil
	}
	s.lock.Unlock()

	pending = s.monitorPending()
	return
}

// ListenAndServe behaves as http.Server.ListenAndServe.
// See: http://golang.org/pkg/net/http/#Server.ListenAndServe
//
// Along with an error, pending channel is returned which will be closed once
// all connections are closed or hijacked.
func (s *Server) ListenAndServe() (pending <-chan bool, err error) {
	pending = noPending

	addr := s.Addr
	if addr == "" {
		addr = ":http"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return
	}

	return s.Serve(ln)
}

// ListenAndServeTLS behaves as http.Server.ListenAndServeTLS.
// See: http://golang.org/pkg/net/http/#Server.ListenAndServeTLS
//
// Along with an error, pending channel is returned which will be closed once
// all connections are closed or hijacked.
func (s *Server) ListenAndServeTLS(certFile, keyFile string) (pending <-chan bool, err error) {
	pending = noPending

	config := &tls.Config{}
	if s.TLSConfig != nil {
		*config = *s.TLSConfig
	}
	if config.NextProtos == nil {
		config.NextProtos = []string{"http/1.1"}
	}

	config.Certificates = make([]tls.Certificate, 1)
	config.Certificates[0], err = tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return
	}

	addr := s.Addr
	if addr == "" {
		addr = ":https"
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return
	}

	return s.Serve(tls.NewListener(ln, config))
}

// The following timeout will be applied to idle connections on server shutdown
var waitOnClose = 100 * time.Millisecond

// Close shutdowns the server by closing the listener, disabling keep alives
// and applying a predefined timeout to all idle connections. Timeouts for all connections
// currently processing a request will be unafected. Call to Close will cause
// Serve to quit. Subsequent calls to Close will return ErrClosing.
func (s *Server) Close() error {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.closing {
		return ErrClosing
	}

	if err := s.listener.Close(); err != nil {
		return err
	}

	s.SetKeepAlivesEnabled(false)
	s.closing = true

	// Set a predefined deadline for all inactive connections (new or idle).
	// If during this period state changes to active, request will be processed
	// with regular request timeout, otherwise connection will be closed.
	deadline := time.Now().Add(waitOnClose)
	for c, active := range s.conns {
		if !active {
			c.SetReadDeadline(deadline)
		}
	}

	return nil
}

// Closed pending channel
var noPending <-chan bool = func() chan bool {
	ch := make(chan bool)
	close(ch)
	return ch
}()

func (s *Server) monitorPending() <-chan bool {
	ch := make(chan bool)
	go func() {
		s.listener.wg.Wait()
		close(ch)
	}()
	return ch
}

func (s *Server) getTimeout(state http.ConnState) (timeout time.Duration) {
	// Update state for new connection according to policy
	if state == http.StateNew {
		if s.NewAsActive {
			state = StateHead
		} else {
			state = http.StateIdle
		}
	}

	switch state {
	case http.StateIdle:
		timeout = s.IdleTimeout

	case StateHead:
		timeout = s.HeadReadTimeout

	case http.StateActive:
		timeout = s.BodyReadTimeout
	}

	return
}

func (s *Server) updateConnState(c net.Conn, state http.ConnState) {
	s.lock.Lock()
	defer s.lock.Unlock()

	// Update connection map
	switch state {
	case http.StateNew, http.StateIdle:
		s.conns[c] = false
	case http.StateClosed, http.StateHijacked:
		delete(s.conns, c)
		s.listener.wg.Done()
	case StateHead:
		s.conns[c] = true
	}

	if state == http.StateIdle {
		if c, ok := c.(*rtConn); ok {
			c.idle()
		}
	}

	// Update timeout if not closing or new request
	if !s.closing || state == StateHead || state == http.StateActive {
		if t := s.getTimeout(state); t != 0 {
			c.SetReadDeadline(time.Now().Add(t))
		}
	} else {
		c.SetReadDeadline(time.Now().Add(waitOnClose))
	}
}

type rtListener struct {
	net.Listener

	newAsActive bool             // set new connections as active
	callback    func(c net.Conn) // data callback

	wg sync.WaitGroup
}

func (l *rtListener) Accept() (c net.Conn, err error) {
	l.wg.Add(1)
	defer func() {
		if c == nil {
			l.wg.Done()
		}
	}()

	c, err = l.Listener.Accept()
	if c != nil {
		c = &rtConn{c, l.newAsActive, l.callback}
	}

	return
}

// rtConn is a net.Conn that sets read deadlines for idle and active state.
// It automatically detects requests as first bytes are read after idle state.
type rtConn struct {
	net.Conn

	active   bool             // are we currently processing a request?
	callback func(c net.Conn) // data callback
}

func (c *rtConn) Read(b []byte) (n int, err error) {
	n, err = c.Conn.Read(b)
	if n > 0 && !c.active {
		c.callback(c)
	}
	return
}

func (c *rtConn) idle() {
	c.active = false
}
