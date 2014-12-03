package httpow

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net"
	"net/http"
	"testing"
	"time"
)

func checkTimeout(c net.Conn, timeout time.Duration) error {
	// Measure read timeout
	start := time.Now()
	c.SetReadDeadline(start.Add(timeout + time.Second))
	data, err := ioutil.ReadAll(c)
	duration := time.Since(start)

	if err != nil {
		return err
	} else if len(data) > 0 {
		return fmt.Errorf("received unexpected data: %s", data)
	}

	if math.Abs(float64(duration-timeout)) > float64(100*time.Millisecond) {
		return fmt.Errorf("timeout duration out of range: %s", duration)
	}

	return nil
}

func checkRequestTimeout(c net.Conn, timeout time.Duration) error {
	c.SetWriteDeadline(time.Now().Add(timeout + time.Second))

	_, err := c.Write([]byte("GET /index.html HTTP/1.1"))
	if err != nil {
		return err
	}

	return checkTimeout(c, timeout)
}

func httpGet(c net.Conn) (data []byte, err error) {
	// Send request
	_, err = c.Write([]byte("GET /index.html HTTP/1.1\n\n"))
	if err != nil {
		return
	}

	rawresp := &bytes.Buffer{}

	// Get response, store all data in rawresp
	resp, err := http.ReadResponse(bufio.NewReader(io.TeeReader(c, rawresp)), nil)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if _, err = ioutil.ReadAll(resp.Body); err != nil {
		return
	}

	data = rawresp.Bytes()
	if len(data) == 0 {
		err = fmt.Errorf("No data from server")
	}

	return
}

func TestNewConnectionIdleTimeout(t *testing.T) {
	t.Parallel()

	idleTimeout := 2 * time.Second

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	go func() {
		s := NewServer(&http.Server{ReadTimeout: idleTimeout / 100})
		s.IdleTimeout = idleTimeout
		s.Serve(l)
	}()

	c, err := net.Dial(l.Addr().Network(), l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err = checkTimeout(c, idleTimeout); err != nil {
		t.Fatal(err)
	}
}

func TestNewConnectionRequestTimeout(t *testing.T) {
	t.Parallel()

	requestTimeout := 2 * time.Second

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	go func() {
		s := NewServer(&http.Server{ReadTimeout: requestTimeout})
		s.IdleTimeout = requestTimeout * 2
		s.Serve(l)
	}()

	c, err := net.Dial(l.Addr().Network(), l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Measure disconnection time...
	start := time.Now()

	// Force read/write to return in case server is broken
	c.SetDeadline(start.Add(requestTimeout + time.Second))

	if err = checkRequestTimeout(c, requestTimeout); err != nil {
		t.Fatal(err)
	}
}

func TestIdleTimeoutAfterRequest(t *testing.T) {
	t.Parallel()

	readTimeout := time.Second
	idleTimeout := 2 * readTimeout

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	go func() {
		s := NewServer(&http.Server{ReadTimeout: readTimeout})
		s.IdleTimeout = idleTimeout
		s.Serve(l)
	}()

	c, err := net.Dial(l.Addr().Network(), l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Sleep through idle time
	time.Sleep(idleTimeout - idleTimeout/10)

	c.SetDeadline(time.Now().Add(idleTimeout + readTimeout + time.Second))

	data, err := httpGet(c)
	t.Log("Server response:", string(data))
	if err != nil {
		t.Fatal(err)
	}

	if err = checkTimeout(c, idleTimeout); err != nil {
		t.Fatal(err)
	}
}

func TestSecondRequestTimeout(t *testing.T) {
	t.Parallel()

	readTimeout := time.Second
	idleTimeout := 2 * readTimeout

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	go func() {
		s := NewServer(&http.Server{ReadTimeout: readTimeout})
		s.IdleTimeout = idleTimeout
		s.Serve(l)
	}()

	c, err := net.Dial(l.Addr().Network(), l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Sleep through idle time
	time.Sleep(idleTimeout - idleTimeout/10)

	c.SetDeadline(time.Now().Add(idleTimeout + readTimeout + time.Second))

	data, err := httpGet(c)
	t.Log("Server response:", string(data))
	if err != nil {
		t.Fatal(err)
	}

	// Sleep through idle time
	time.Sleep(idleTimeout - idleTimeout/10)

	if err = checkRequestTimeout(c, readTimeout); err != nil {
		t.Fatal(err)
	}
}

func TestNoNewIdle(t *testing.T) {
	t.Parallel()

	readTimeout := time.Second

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	go func() {
		s := NewServer(&http.Server{ReadTimeout: readTimeout})
		s.IdleTimeout = readTimeout * 2
		s.NoNewIdle = true
		s.Serve(l)
	}()

	c, err := net.Dial(l.Addr().Network(), l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err = checkTimeout(c, readTimeout); err != nil {
		t.Fatal(err)
	}
}
