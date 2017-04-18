// Copyright (c) Liam Stanley <me@liamstanley.io>. All rights reserved. Use
// of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package girc

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// Messages are delimited with CR and LF line endings, we're using the last
// one to split the stream. Both are removed during parsing of the message.
const delim byte = '\n'

var endline = []byte("\r\n")

// ircConn represents an IRC network protocol connection, it consists of an
// Encoder and Decoder to manage i/o.
type ircConn struct {
	io   *bufio.ReadWriter
	sock net.Conn

	mu sync.RWMutex
	// lastWrite is used ot keep track of when we last wrote to the server.
	lastWrite time.Time
	// writeDelay is used to keep track of rate limiting of events sent to
	// the server.
	writeDelay time.Duration
	// connected is true if we're actively connected to a server.
	connected bool
	// connTime is the time at which the client has connected to a server.
	connTime *time.Time
	// lastPing is the last time that we pinged the server.
	lastPing time.Time
	// lastPong is the last successful time that we pinged the server and
	// received a successful pong back.
	lastPong  time.Time
	pingDelay time.Duration
}

// newConn sets up and returns a new connection to the server. This includes
// setting up things like proxies, ssl/tls, and other misc. things.
func newConn(conf Config, addr string) (*ircConn, error) {
	if err := conf.isValid(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %s", err)
	}

	var conn net.Conn
	var err error

	dialer := &net.Dialer{Timeout: 5 * time.Second}

	if conf.Bind != "" {
		var local *net.TCPAddr
		local, err = net.ResolveTCPAddr("tcp", conf.Bind+":0")
		if err != nil {
			return nil, fmt.Errorf("unable to resolve bind address %s: %s", conf.Bind, err)
		}

		dialer.LocalAddr = local
	}

	if conf.Proxy != "" {
		var proxyURI *url.URL
		var proxyDialer proxy.Dialer

		proxyURI, err = url.Parse(conf.Proxy)
		if err != nil {
			return nil, fmt.Errorf("unable to use proxy %q: %s", conf.Proxy, err)
		}

		proxyDialer, err = proxy.FromURL(proxyURI, dialer)
		if err != nil {
			return nil, fmt.Errorf("unable to use proxy %q: %s", conf.Proxy, err)
		}

		conn, err = proxyDialer.Dial("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("unable to connect to proxy %q: %s", conf.Proxy, err)
		}
	} else {
		conn, err = dialer.Dial("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("unable to connect to %q: %s", addr, err)
		}
	}

	if conf.SSL {
		var tlsConn net.Conn
		tlsConn, err = tlsHandshake(conn, conf.TLSConfig, conf.Server, true)
		if err != nil {
			return nil, err
		}

		conn = tlsConn
	}

	ctime := time.Now()

	c := &ircConn{
		sock:      conn,
		connTime:  &ctime,
		connected: true,
	}
	c.newReadWriter()

	return c, nil
}

func (c *ircConn) decode() (event *Event, err error) {
	line, err := c.io.ReadString(delim)
	if err != nil {
		return nil, err
	}

	event = ParseEvent(line)
	if event == nil {
		return nil, fmt.Errorf("unable to parse incoming event: %s", event)
	}

	return event, nil
}

func (c *ircConn) encode(event *Event) error {
	if _, err := c.io.Write(event.Bytes()); err != nil {
		return err
	}
	if _, err := c.io.Write(endline); err != nil {
		return err
	}

	return c.io.Flush()
}

func (c *ircConn) newReadWriter() {
	c.io = bufio.NewReadWriter(bufio.NewReader(c.sock), bufio.NewWriter(c.sock))
}

func tlsHandshake(conn net.Conn, conf *tls.Config, server string, validate bool) (net.Conn, error) {
	if conf == nil {
		conf = &tls.Config{ServerName: server, InsecureSkipVerify: !validate}
	}

	tlsConn := tls.Client(conn, conf)
	return net.Conn(tlsConn), nil
}

// Close closes the underlying socket.
func (c *ircConn) Close() error {
	return c.sock.Close()
}

// Connect attempts to connect to the given IRC server
func (c *Client) Connect() error {
	// We want to be the only one handling connects/disconnects right now.
	c.cmux.Lock()

	// Reset the state.
	c.state = newState()

	// Validate info, and actually make the connection.
	c.debug.Printf("connecting to %s...", c.Server())
	conn, err := newConn(c.Config, c.Server())
	if err != nil {
		c.cmux.Unlock()
		return err
	}

	c.conn = conn
	c.cmux.Unlock()

	// Start read loop to process messages from the server.
	errs := make(chan error, 4)
	done := make(chan struct{}, 4)
	defer close(errs)
	defer close(done)

	go c.execLoop(done)
	go c.readLoop(errs, done)
	go c.pingLoop(errs, done)
	go c.sendLoop(errs, done)

	// Send a virtual event allowing hooks for successful socket connection.
	c.RunHandlers(&Event{Command: INITIALIZED, Trailing: c.Server()})

	// Passwords first.
	if c.Config.Password != "" {
		c.write(&Event{Command: PASS, Params: []string{c.Config.Password}})
	}

	// Then nickname.
	c.write(&Event{Command: NICK, Params: []string{c.Config.Nick}})

	// Then username and realname.
	if c.Config.Name == "" {
		c.Config.Name = c.Config.User
	}

	c.write(&Event{Command: USER, Params: []string{c.Config.User, "+iw", "*"}, Trailing: c.Config.Name})

	// List the IRCv3 capabilities, specifically with the max protocol we
	// support.
	c.listCAP()

	return <-errs
}

// readLoop sets a timeout of 300 seconds, and then attempts to read from the
// IRC server. If there is an error, it calls Reconnect.
func (c *Client) readLoop(errs chan error, done chan struct{}) {
	var event *Event
	var err error

	for {
		select {
		case <-done:
			return
		default:
			// c.conn.sock.SetDeadline(time.Now().Add(300 * time.Second))
			event, err = c.conn.decode()
			if err != nil {
				// Attempt a reconnect (if applicable). If it fails, send
				// the error to c.Config.HandleError to be dealt with, if
				// the handler exists.
				errs <- err
			}

			c.rx <- event
		}
	}
}

// Send sends an event to the server. Use Client.RunHandlers() if you are
// simply looking to trigger handlers with an event.
func (c *Client) Send(event *Event) {
	if !c.Config.AllowFlood {
		<-time.After(c.conn.rate(event.Len()))
	}

	c.write(event)
}

// write is the lower level function to write an event. It does not have a
// write-delay when sending events.
func (c *Client) write(event *Event) {
	c.tx <- event
}

// rate allows limiting events based on how frequent the event is being sent,
// as well as how many characters each event has.
func (c *ircConn) rate(chars int) time.Duration {
	_time := time.Second + ((time.Duration(chars) * time.Second) / 100)

	c.mu.Lock()
	if c.writeDelay += _time - time.Now().Sub(c.lastWrite); c.writeDelay < 0 {
		c.writeDelay = 0
	}
	c.mu.Unlock()

	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.writeDelay > (8 * time.Second) {
		return _time
	}

	return 0
}

func (c *Client) sendLoop(errs chan error, done chan struct{}) {
	var err error

	for {
		select {
		case <-done:
			return
		case event := <-c.tx:
			// Log the event.
			if !event.Sensitive {
				c.debug.Print("> ", StripRaw(event.String()))
			}
			if c.Config.Out != nil {
				if pretty, ok := event.Pretty(); ok {
					fmt.Fprintln(c.Config.Out, StripRaw(pretty))
				}
			}

			c.conn.mu.Lock()
			c.conn.lastWrite = time.Now()
			c.conn.mu.Unlock()

			// Write the raw line.
			_, err = c.conn.io.Write(event.Bytes())
			if err == nil {
				// And the \r\n.
				_, err = c.conn.io.Write(endline)
				if err == nil {
					// Lastly, flush everything to the socket.
					err = c.conn.io.Flush()
				}
			}

			if err != nil {
				errs <- err
				return
			}
		}
	}
}

// flushTx empties c.tx.
func (c *Client) flushTx() {
	for {
		select {
		case <-c.tx:
		default:
			return
		}
	}
}

// ErrTimedOut is returned when we attempt to ping the server, and time out
// before receiving a PONG back.
var ErrTimedOut = errors.New("timed out during ping to server")

func (c *Client) pingLoop(errs chan error, done chan struct{}) {
	c.conn.mu.Lock()
	c.conn.lastPing = time.Now()
	c.conn.lastPong = time.Now()
	c.conn.mu.Unlock()

	// Delay for 30 seconds during connect to wait for the client to register
	// and what not.
	time.Sleep(20 * time.Second)

	tick := time.NewTicker(c.Config.PingDelay)
	defer tick.Stop()

	for {
		select {
		case <-done:
			return
		case <-tick.C:
			c.conn.mu.RLock()
			defer c.conn.mu.RUnlock()
			if time.Since(c.conn.lastPong) > c.Config.PingDelay+(60*time.Second) {
				// It's 60 seconds over what out ping delay is, connection
				// has probably dropped.
				errs <- ErrTimedOut
				return
			}

			c.conn.mu.Lock()
			c.conn.lastPing = time.Now()
			c.conn.mu.Unlock()
			c.Commands.Ping(fmt.Sprintf("%d", time.Now().UnixNano()))
		}
	}
}
