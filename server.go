package httpow

import (
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

// Server traps http.Server, exposes additional fuctionality
type Server struct {
	// IdleTimeout tells how long connection can be idle between requests.
	// Value of http.Server.ReadTimeout taken from the passed server instance
	// will be used as a single request read timeout.
	IdleTimeout time.Duration

	// NewAsActive controls whether new connection can be idle before issuing
	// a request. By default new connections will have IdleTimeout allowing for
	// idle period before issuing a request. However, if this flag is set, new
	// connections will be given ReadTimeout as the request was already initiated.
	NewAsActive bool

	server   *http.Server // wrapped http server
	listener net.Listener

	reqTimeout time.Duration // request timeout

	lock sync.Mutex

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
func (s *Server) Serve(l net.Listener) error {
	s.reqTimeout = s.server.ReadTimeout

	oldConnState := s.server.ConnState
	newConnState := func(c net.Conn, state http.ConnState) {
		s.updateConnState(c, state)
		// Pass to original handler
		if oldConnState != nil {
			oldConnState(c, state)
		}
	}

	s.server.ConnState = newConnState

	if s.IdleTimeout != 0 {
		// Disable read timeout managment by http.Server
		s.server.ReadTimeout = 0

		// Wrap with custom listener
		l = &rtListener{
			Listener:    l,
			newAsActive: s.NewAsActive,
			callback:    newConnState,
		}
	}

	err := s.server.Serve(l)
	if err == errListenerClosed {
		err = nil
	}
	return err
}

// Close server
func (s *Server) Close() {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.closing = true

	s.listener.Close()
	s.server.SetKeepAlivesEnabled(false)

	// Set a 100ms deadline for all inactive connections.
	// If during this period state changes to active, request will be processed
	// with regular request timeout, otherwise connection will be closed.
	deadline := time.Now().Add(100 * time.Millisecond)
	for c, active := range s.conns {
		if !active {
			c.SetReadDeadline(deadline)
		}
	}
}

func (s *Server) getTimeout(state http.ConnState) (timeout time.Duration) {
	switch state {
	case http.StateNew:
		if s.NewAsActive {
			timeout = s.reqTimeout
		} else {
			timeout = s.IdleTimeout
		}

	case http.StateIdle:
		timeout = s.IdleTimeout

	case StateData:
		timeout = s.reqTimeout
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
	case StateData:
		s.conns[c] = true
	}

	if state == http.StateIdle {
		if c, ok := c.(*rtConn); ok {
			c.idle()
		}
	}

	// Update timeout if not closing or new request
	if !s.closing || state == StateData {
		if t := s.getTimeout(state); t != 0 {
			c.SetReadDeadline(time.Now().Add(t))
		}
	}
}

type rtListener struct {
	net.Listener

	newAsActive bool // set new connections as active
	callback    func(c net.Conn, s http.ConnState)

	mx     sync.Mutex
	closed bool
}

// This error will be proagated to Serve when we delibaretely close the listener
var errListenerClosed = errors.New("listener closed")

func (l *rtListener) Accept() (c net.Conn, err error) {
	c, err = l.Listener.Accept()
	if c != nil {
		c = &rtConn{c, l.newAsActive, l.callback}
	}
	if err != nil {
		l.mx.Lock()
		if l.closed {
			err = errListenerClosed
		}
		l.mx.Unlock()
	}
	return
}

func (l *rtListener) Close() (err error) {
	l.mx.Lock()
	l.closed = true
	l.mx.Unlock()
	return l.Listener.Close()
}

const (
	// StateData tells when initial request data (header) was received.
	// This varies from http.StateActive, as the latter is issued after headers
	// are parsed.
	StateData http.ConnState = 100 + iota
)

// rtConn is a net.Conn that sets read deadlines for idle and active state.
// It automatically detects requests as first bytes are read after idle state.
type rtConn struct {
	net.Conn

	active   bool // are we currently processing a request?
	callback func(c net.Conn, s http.ConnState)
}

func (c *rtConn) Read(b []byte) (n int, err error) {
	n, err = c.Conn.Read(b)
	if n > 0 && !c.active {
		c.callback(c, StateData)
	}
	return
}

func (c *rtConn) idle() {
	c.active = false
}
