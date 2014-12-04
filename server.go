package httpow

import (
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

	// NoNewIdle controls whether new connection can be idle before issuing
	// a request. By default new connections will have IdleTimeout allowing for
	// idle period before issuing a request. However, if this flag is set, new
	// connections will be given ReadTimeout as the request was already initiated.
	NoNewIdle bool

	server   *http.Server // wrapped http server
	listener net.Listener

	lock sync.Mutex

	// conns is a map of connections which indicates whether connection is active.
	// Connection is active when is processeing a request (after headers parsing).
	conns map[net.Conn]bool
}

// NewServer wraps http.Server, which should be already configured.
func NewServer(server *http.Server) *Server {
	return &Server{server: server, conns: make(map[net.Conn]bool)}
}

// Serve behaves as http.Server.Serve on the wrapped server instance
func (s *Server) Serve(l net.Listener) error {
	if s.IdleTimeout != 0 {
		reqTimeout := s.server.ReadTimeout

		// Disable read timeout managment by http.Server
		s.server.ReadTimeout = 0

		// Wrap with custom listener
		l = &rtListener{l, reqTimeout, s.IdleTimeout, s.NoNewIdle}
	}

	// Add ConnState callback, but make sure the one provided by user is called as well
	oldConnState := s.server.ConnState
	s.server.ConnState = func(c net.Conn, state http.ConnState) {
		s.updateConnMap(c, state)

		if state == http.StateIdle {
			if c, ok := c.(*rtConn); ok {
				c.setIdle()
			}
		}

		// Pass to original handler
		if oldConnState != nil {
			oldConnState(c, state)
		}
	}

	return s.server.Serve(l)
}

func (s *Server) updateConnMap(c net.Conn, state http.ConnState) {
	s.lock.Lock()
	defer s.lock.Unlock()

	switch state {
	case http.StateNew, http.StateIdle:
		s.conns[c] = false
	case http.StateClosed, http.StateHijacked:
		delete(s.conns, c)
	case http.StateActive:
		s.conns[c] = true
	}
}

type rtListener struct {
	net.Listener

	reqTimeout  time.Duration // timeout for processing a single request
	idleTimeout time.Duration // idle timeout
	newAsReq    bool          // set new connections to reqTimeout
}

func (l *rtListener) Accept() (c net.Conn, err error) {
	c, err = l.Listener.Accept()
	if c != nil {
		c = newRtConn(c, l.reqTimeout, l.idleTimeout, l.newAsReq)
	}
	return
}

// rtConn is a net.Conn that sets read deadlines for idle and active state.
// It automatically detects requests as first bytes are read after idle state.
type rtConn struct {
	net.Conn

	reqTimeout  time.Duration // timeout for processing a single request
	idleTimeout time.Duration // idle timeout
	active      bool          // are we currently processing a request?
}

func newRtConn(c net.Conn, rto time.Duration, ito time.Duration, active bool) *rtConn {
	rc := &rtConn{c, rto, ito, active}
	if active {
		rc.setActive()
	} else {
		rc.setIdle()
	}
	return rc
}

func (c *rtConn) Read(b []byte) (n int, err error) {
	n, err = c.Conn.Read(b)
	if n > 0 && !c.active {
		c.setActive()
	}
	return
}

func (c *rtConn) setActive() {
	c.active = true
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.reqTimeout))
}

func (c *rtConn) setIdle() {
	c.active = false
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.idleTimeout))
}
