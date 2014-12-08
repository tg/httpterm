// Package httpterm provides closable http.Server with extended read timeouts.
package httpterm

import (
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
	// StateHead tells when initial request data (header) was received.
	// This varies from http.StateActive, as the latter is issued after headers
	// are parsed.
	StateHead http.ConnState = 100 + iota
)

// ErrClosing indicating that operation is not allowed as server is closing
var ErrClosing = errors.New("server closing")

// Server traps http.Server, exposes additional fuctionality
type Server struct {
	// HeadReadTimeout defines timeout for reading request headers.
	HeadReadTimeout time.Duration

	// BodyReadTimeout defines timeout for reading request body.
	// This timeout is being applied just before calling the request handler.
	BodyReadTimeout time.Duration

	// IdleTimeout defines for how long connection can be idle between requests.
	IdleTimeout time.Duration

	// NewAsActive controls whether new connection can be idle before issuing
	// a request. By default, new connections will have IdleTimeout allowing for
	// idle period before issuing a request. However, if this flag is set, new
	// connections will be treated as immediately expecting the request, thus
	// HeadReadTimeout will be applied.
	NewAsActive bool

	// CloseOnSignal enables server shutdown on SIGTERM/SIGNINT.
	// Signal handler is registered in Serve() method.
	CloseOnSignal bool

	server   *http.Server // wrapped http server
	listener *rtListener

	lock    sync.Mutex
	closing bool

	// conns is a map of connections which indicates whether connection is active,
	// i.e. there a request being processed (including header handling)
	conns map[net.Conn]bool
}

// NewServer wraps http.Server, which should be already configured.
func NewServer(server *http.Server) *Server {
	return &Server{server: server, conns: make(map[net.Conn]bool)}
}

// Serve behaves as http.Server.Serve on the wrapped server instance
func (s *Server) Serve(l net.Listener) (pending <-chan bool, err error) {
	oldConnState := s.server.ConnState
	newConnState := func(c net.Conn, state http.ConnState) {
		s.updateConnState(c, state)
		// Pass to original handler
		if oldConnState != nil {
			oldConnState(c, state)
		}
	}

	s.server.ConnState = newConnState

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
	err = s.server.Serve(s.listener)

	// Clear error if server closing
	s.lock.Lock()
	if s.closing {
		err = nil
	}
	s.lock.Unlock()

	// Wait for pending requests
	waiter := make(chan bool)
	pending = waiter
	go func() {
		s.listener.wg.Wait()
		close(waiter)
	}()

	return
}

// ListenAndServe listens on the TCP network address from passed http.Server.Addr
// and calls Serve(). Returned channel can be used to wait for pending requests
// and will be closed once everything is handled.
func (s *Server) ListenAndServe() (pending <-chan bool, err error) {
	addr := s.server.Addr
	if addr == "" {
		addr = ":http"
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return s.Serve(l)
}

// Close server
func (s *Server) Close() error {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.closing {
		return ErrClosing
	}

	if err := s.listener.Close(); err != nil {
		return err
	}

	s.server.SetKeepAlivesEnabled(false)
	s.closing = true

	// Set a 100ms deadline for all inactive connections (new or idle).
	// If during this period state changes to active, request will be processed
	// with regular request timeout, otherwise connection will be closed.
	deadline := time.Now().Add(100 * time.Millisecond)
	for c, active := range s.conns {
		if !active {
			c.SetReadDeadline(deadline)
		}
	}

	return nil
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
