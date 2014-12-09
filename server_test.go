package httpterm

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"
)

func assertTimeout(elapsed, expected time.Duration) error {
	if math.Abs(float64(elapsed-expected)) > float64(100*time.Millisecond) {
		return fmt.Errorf("timeout duration out of range: %s (expected %s)", elapsed, expected)
	}
	return nil
}

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

	return assertTimeout(duration, timeout)
}

func checkTimeoutGET(c net.Conn, timeout time.Duration) error {
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
		err = fmt.Errorf("no data from server")
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
		s := Server{}
		s.IdleTimeout = idleTimeout
		s.HeadReadTimeout = idleTimeout / 100
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
		var s Server
		s.IdleTimeout = requestTimeout * 2
		s.HeadReadTimeout = requestTimeout
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

	if err = checkTimeoutGET(c, requestTimeout); err != nil {
		t.Fatal(err)
	}
}

func TestNewConnectionBodyTimeout(t *testing.T) {
	t.Parallel()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	bodyread := make(chan time.Duration)

	handler := http.NewServeMux()
	handler.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ioutil.ReadAll(r.Body)
		bodyread <- time.Since(start)
	})

	var s Server
	s.Handler = handler
	s.BodyReadTimeout = time.Second
	go func() {
		s.Serve(l)
	}()
	defer s.Close()

	c, err := net.Dial(l.Addr().Network(), l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.SetWriteDeadline(time.Now().Add(time.Second))

	// Send all headers, but incomplete data
	_, err = c.Write([]byte("POST /index.html HTTP/1.1\nContent-Length: 16\n\ndata"))
	if err != nil {
		t.Fatal(err)
	}

	elapsed := <-bodyread
	if err = assertTimeout(elapsed, s.BodyReadTimeout); err != nil {
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
		var s Server
		s.IdleTimeout = idleTimeout
		s.HeadReadTimeout = readTimeout
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
		var s Server
		s.IdleTimeout = idleTimeout
		s.HeadReadTimeout = readTimeout
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

	if err = checkTimeoutGET(c, readTimeout); err != nil {
		t.Fatal(err)
	}
}

func TestNewAsActive(t *testing.T) {
	t.Parallel()

	readTimeout := time.Second

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	go func() {
		var s Server
		s.IdleTimeout = readTimeout * 2
		s.HeadReadTimeout = readTimeout
		s.NewAsActive = true
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

func TestClose_empty(t *testing.T) {
	var s Server
	s.Addr = "127.0.0.1:0"

	done := make(chan bool)

	go func() {
		pending, err := s.ListenAndServe()
		if err != nil {
			t.Fatal(err)
		}
		<-pending
		done <- true
	}()

	time.Sleep(time.Second)
	s.Close()
	<-done

	err := s.Close()
	if err != ErrClosing {
		t.Fatal("Expected ErrClosing, got:", err)
	}
}

func TestClose_idle(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	done := make(chan bool)

	var s Server
	s.IdleTimeout = 5 * time.Second

	go func() {
		pending, err := s.Serve(l)
		if err != nil {
			t.Fatal(err)
		}
		<-pending
		done <- true
	}()

	c, err := net.Dial(l.Addr().Network(), l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s.Close()
	<-done
}

func TestClose_active(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	done := make(chan bool)

	var s Server
	s.ReadTimeout = time.Second
	s.IdleTimeout = 5 * time.Second

	go func() {
		pending, err := s.Serve(l)
		if err != nil {
			t.Fatal(err)
		}
		<-pending
		done <- true
	}()

	c, err := net.Dial(l.Addr().Network(), l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.Write([]byte("GET"))
	s.Close()

	<-done
}

func TestClose_activeAfterClose(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	done := make(chan bool)

	var s Server
	s.ReadTimeout = time.Second
	s.IdleTimeout = 5 * time.Second

	go func() {
		pending, err := s.Serve(l)
		if err != nil {
			t.Fatal(err)
		}
		<-pending
		done <- true
	}()

	c, err := net.Dial(l.Addr().Network(), l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s.Close()
	c.Write([]byte("GET"))

	<-done
}

func TestClose_signal(t *testing.T) {
	var s Server
	s.Addr = "127.0.0.1:0"
	s.CloseOnSignal = true

	done := make(chan bool)

	go func() {
		pending, err := s.ListenAndServe()
		if err != nil {
			t.Fatal(err)
		}
		<-pending
		done <- true
	}()

	time.Sleep(time.Second)
	syscall.Kill(syscall.Getpid(), syscall.SIGINT)

	<-done
}
