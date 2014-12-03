package httpow

import (
	"net"
	"net/http"
	"time"
)

// Server traps http.Server, exposes additional fuctionality
type Server struct {
	// IdleTimeout tells how long connection can be idle between requests.
	// Value of http.Server.ReadTimeout taken from the passed server instance
	// will be used as a single request read timeout.
	IdleTimeout time.Duration

	server   *http.Server // wrapped http server
	listener net.Listener
}

// NewServer wraps http.Server, which should be already configured.
func NewServer(server *http.Server) *Server {
	return &Server{server: server}
}

// Serve behaves as http.Server.Serve on the wrapped server instance
func (s *Server) Serve(l net.Listener) error {
	if s.IdleTimeout != 0 {
		// Wrap with custom listener
		l = &rtListener{l, s.server.ReadTimeout}

		// Set server.ReadTimeout to IdleTimeout, as http.Server uses this
		// after going into idle state, when awaiting new request.
		s.server.ReadTimeout = s.IdleTimeout

		oldConnState := s.server.ConnState
		s.server.ConnState = func(c net.Conn, s http.ConnState) {
			switch s {
			case http.StateIdle:
				if c, ok := c.(*rtConn); ok {
					c.idle()
				}
			}
			// Pass to custom handler
			if oldConnState != nil {
				oldConnState(c, s)
			}
		}
	}

	return s.server.Serve(l)
}

type rtListener struct {
	net.Listener
	timeout time.Duration // timeout for processing a single request
}

func (l *rtListener) Accept() (c net.Conn, err error) {
	c, err = l.Listener.Accept()
	return &rtConn{Conn: c, timeout: l.timeout}, err
}

// conn is a net.Conn that resets read deadline at the beginning of every request
type rtConn struct {
	net.Conn

	timeout time.Duration // timeout for processing a single request
	active  bool          // are we currently processing a request?
}

func (c *rtConn) Read(b []byte) (n int, err error) {
	n, err = c.Conn.Read(b)
	if n > 0 && !c.active {
		// First byte in the request, set the deadline
		c.active = true
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	}
	return
}

// mark connection as idle, so next byte will be considered as a new request
func (c *rtConn) idle() {
	c.active = false
}
