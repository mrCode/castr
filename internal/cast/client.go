package cast

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// The four namespaces castr speaks. A receiver advertises others -- device
// authentication, multizone, per-app namespaces -- which castr does not need.
const (
	nsConnection = "urn:x-cast:com.google.cast.tp.connection"
	nsHeartbeat  = "urn:x-cast:com.google.cast.tp.heartbeat"
	nsReceiver   = "urn:x-cast:com.google.cast.receiver"
	nsMedia      = "urn:x-cast:com.google.cast.media"
)

const (
	// senderID is this end of every conversation. The protocol allows any
	// string; receivers echo it back as the destination.
	senderID = "sender-castr"

	// platformID is the receiver's own endpoint, as opposed to the transport
	// id of an app running on it.
	platformID = "receiver-0"

	// DefaultMediaReceiver plays a URL. It is Google's own app, present on
	// every Chromecast, and needs no developer registration -- which is what
	// makes casting from a program like this possible at all.
	DefaultMediaReceiver = "CC1AD845"

	// heartbeatInterval is what receivers expect. Miss a few and the receiver
	// drops the connection.
	heartbeatInterval = 5 * time.Second
)

// Conn is a connection to a receiver.
type Conn struct {
	// Observe, when set, is called with every message the receiver sends,
	// including the unsolicited status broadcasts that carry the reason a
	// stream stopped. Those are where a receiver explains itself; without
	// them a failure looks like the television going quiet.
	Observe func(namespace, payload string)

	conn      net.Conn
	requestID atomic.Int64

	writeMu sync.Mutex

	mu        sync.Mutex
	pending   map[int64]chan json.RawMessage
	watchers  map[int]func(namespace, payload string) bool
	nextWatch int
	closed    bool
	failure   error

	done chan struct{}
	wg   sync.WaitGroup
}

// Dial opens a connection and starts the heartbeat.
//
// TLS verification is disabled, deliberately. Receivers present a certificate
// issued by Google's device CA to a serial number, not to an IP address or a
// hostname, so no ordinary verification can succeed. The protocol's answer is
// its own device-authentication namespace, which every third-party sender --
// including Google's own Cast SDK for third parties -- skips. What this costs
// is worth stating plainly: castr cannot prove the box at that address is the
// television it discovered, so anything on the local network could impersonate
// it. Since what castr then sends is a URL and not the video itself, and the
// video is served over the same local network either way, this changes who can
// watch the stream, not whether it can be intercepted.
func Dial(ctx context.Context, addr string) (*Conn, error) {
	d := tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", addr, err)
	}

	c := &Conn{
		conn:     nc,
		pending:  map[int64]chan json.RawMessage{},
		watchers: map[int]func(string, string) bool{},
		done:     make(chan struct{}),
	}
	c.wg.Add(2)
	go c.readLoop()
	go c.heartbeat()

	if err := c.send(Message{
		Source: senderID, Destination: platformID,
		Namespace: nsConnection, Payload: `{"type":"CONNECT"}`,
	}); err != nil {
		c.Close()
		return nil, fmt.Errorf("connecting to %s: %w", addr, err)
	}
	return c, nil
}

// Close shuts the connection down. It is safe to call more than once.
func (c *Conn) Close() error {
	c.shutdown(nil)
	c.wg.Wait()
	return nil
}

// shutdown closes the connection once, recording why. Both the reader and
// Close arrive here, from different goroutines, possibly at the same moment;
// the flag under the mutex is what makes "once" true rather than likely.
func (c *Conn) shutdown(cause error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	if c.failure == nil {
		c.failure = cause
	}
	c.mu.Unlock()

	close(c.done)
	_ = c.conn.Close()
}

func (c *Conn) send(m Message) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := c.conn.Write(Encode(m))
	return err
}

// request sends a message carrying a request id and waits for the reply that
// quotes it back.
//
// Replies are matched by request id rather than by arrival order because a
// receiver interleaves them freely with unsolicited status broadcasts -- and
// with PONGs. Reading "the next message" would routinely return a heartbeat.
func (c *Conn) request(ctx context.Context, dest, namespace string, payload map[string]any) (json.RawMessage, error) {
	id := c.requestID.Add(1)
	payload["requestId"] = id

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	reply := make(chan json.RawMessage, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, c.err()
	}
	c.pending[id] = reply
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.send(Message{
		Source: senderID, Destination: dest,
		Namespace: namespace, Payload: string(body),
	}); err != nil {
		return nil, err
	}

	select {
	case r := <-reply:
		return r, nil
	case <-c.done:
		return nil, c.err()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// watch calls fn for every message until fn reports it is finished, and
// returns whatever fn passed to done.
//
// Some answers do not arrive as a reply to anything. A LOAD is answered by an
// interim MEDIA_STATUS quoting the request id, and only later by the message
// that says whether it worked -- so matching on the request id alone reports
// success for a stream the receiver is about to reject.
func (c *Conn) watch(ctx context.Context, fn func(namespace, payload string, done func(error)) bool) error {
	result := make(chan error, 1)
	finish := func(err error) {
		select {
		case result <- err:
		default:
		}
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return c.err()
	}
	c.nextWatch++
	id := c.nextWatch
	c.watchers[id] = func(ns, payload string) bool {
		return fn(ns, payload, finish)
	}
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.watchers, id)
		c.mu.Unlock()
	}()

	select {
	case err := <-result:
		return err
	case <-c.done:
		return c.err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Conn) err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failure != nil {
		return c.failure
	}
	return errors.New("cast: connection closed")
}

func (c *Conn) readLoop() {
	defer c.wg.Done()
	for {
		m, err := ReadMessage(c.conn)
		if err != nil {
			// Shutting down here is what wakes every pending request. Without
			// it a caller blocks until its own context expires and reports a
			// timeout for a connection already known to be gone.
			cause := fmt.Errorf("cast: reading from the receiver: %w", err)
			if errors.Is(err, io.EOF) {
				cause = errors.New("cast: the receiver closed the connection")
			}
			c.shutdown(cause)
			return
		}

		if c.Observe != nil && m.Namespace != nsHeartbeat {
			c.Observe(m.Namespace, m.Payload)
		}
		if m.Namespace != nsHeartbeat {
			c.mu.Lock()
			for id, watch := range c.watchers {
				if watch(m.Namespace, m.Payload) {
					delete(c.watchers, id)
				}
			}
			c.mu.Unlock()
		}

		if m.Namespace == nsHeartbeat {
			if payloadType(m.Payload) == "PING" {
				_ = c.send(Message{
					Source: senderID, Destination: m.Source,
					Namespace: nsHeartbeat, Payload: `{"type":"PONG"}`,
				})
			}
			continue
		}

		var envelope struct {
			RequestID int64 `json:"requestId"`
		}
		_ = json.Unmarshal([]byte(m.Payload), &envelope)
		if envelope.RequestID == 0 {
			continue // an unsolicited status broadcast
		}

		c.mu.Lock()
		waiter, ok := c.pending[envelope.RequestID]
		c.mu.Unlock()
		if ok {
			select {
			case waiter <- json.RawMessage(m.Payload):
			default:
			}
		}
	}
}

func (c *Conn) heartbeat() {
	defer c.wg.Done()
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			_ = c.send(Message{
				Source: senderID, Destination: platformID,
				Namespace: nsHeartbeat, Payload: `{"type":"PING"}`,
			})
		}
	}
}

func payloadType(s string) string {
	var p struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(s), &p)
	return p.Type
}
