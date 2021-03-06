package nats

import (
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/jirenius/timerqueue"
	nats "github.com/nats-io/nats.go"
	"github.com/resgateio/resgate/logger"
	"github.com/resgateio/resgate/server/mq"
)

const (
	natsChannelSize = 256
)

// Client holds a client connection to a nats server.
type Client struct {
	RequestTimeout time.Duration
	URL            string
	Creds          *string
	Logger         logger.Logger

	mq           *nats.Conn
	mqCh         chan *nats.Msg
	mqReqs       map[*nats.Subscription]*responseCont
	tq           *timerqueue.Queue
	mu           sync.Mutex
	closeHandler func(error)
	stopped      chan struct{}
}

// Subscription implements the mq.Unsubscriber interface.
type Subscription struct {
	c   *Client
	sub *nats.Subscription
}

type responseCont struct {
	isReq bool
	f     mq.Response
	t     *time.Timer
}

// Logf writes a formatted log message
func (c *Client) Logf(format string, v ...interface{}) {
	c.Logger.Log(fmt.Sprintf(format, v...))
}

// Debugf writes a formatted debug message
func (c *Client) Debugf(format string, v ...interface{}) {
	if c.Logger.IsDebug() {
		c.Logger.Debug(fmt.Sprintf(format, v...))
	}
}

// Tracef writes a formatted trace message
func (c *Client) Tracef(format string, v ...interface{}) {
	if c.Logger.IsTrace() {
		c.Logger.Trace(fmt.Sprintf(format, v...))
	}
}

// Connect creates a connection to the nats server.
func (c *Client) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.Logf("Connecting to NATS at %s", c.URL)

	// Create connection options
	opts := []nats.Option{nats.NoReconnect(), nats.ClosedHandler(c.onClose)}
	if c.Creds != nil {
		opts = append(opts, nats.UserCredentials(*c.Creds))
	}

	// No reconnects as all resources are instantly stale anyhow
	nc, err := nats.Connect(c.URL, opts...)
	if err != nil {
		return err
	}

	c.mq = nc
	c.mqCh = make(chan *nats.Msg, natsChannelSize)
	c.mqReqs = make(map[*nats.Subscription]*responseCont)
	c.tq = timerqueue.New(c.onTimeout, c.RequestTimeout)
	c.stopped = make(chan struct{})

	go c.listener(c.mqCh, c.stopped)

	return nil
}

// IsClosed tests if the client connection has been closed.
func (c *Client) IsClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mq == nil {
		return true
	}

	return c.mq.IsClosed()
}

// Close closes the client connection.
func (c *Client) Close() {
	c.mu.Lock()

	if c.mq == nil {
		c.mu.Unlock()
		return
	}

	if !c.mq.IsClosed() {
		c.Debugf("Closing NATS connection...")
		c.mq.Close()
		c.Debugf("NATS connection closed")
	}

	c.Debugf("Stopping NATS listener...")
	close(c.mqCh)
	c.mqCh = nil

	c.mq = nil
	// Set mqReqs to empty map to avoid possible nil reference error in listener
	c.mqReqs = make(map[*nats.Subscription]*responseCont)

	c.tq.Clear()
	c.tq = nil

	stopped := c.stopped
	c.stopped = nil

	c.mu.Unlock()

	<-stopped
	c.Debugf("NATS listener stopped")
}

// SetClosedHandler sets the handler when the connection is closed
func (c *Client) SetClosedHandler(cb func(error)) {
	c.closeHandler = cb
}

func (c *Client) onClose(conn *nats.Conn) {
	if c.closeHandler != nil {
		err := conn.LastError()
		c.closeHandler(fmt.Errorf("lost NATS connection: %s", err))
	}
}

// SendRequest sends a request to the MQ.
func (c *Client) SendRequest(subj string, payload []byte, cb mq.Response) {
	inbox := nats.NewInbox()

	c.mu.Lock()
	defer c.mu.Unlock()

	sub, err := c.mq.ChanSubscribe(inbox, c.mqCh)
	if err != nil {
		cb("", nil, err)
		return
	}

	c.Tracef("<== (%s) %s: %s", inboxSubstr(inbox), subj, payload)

	err = c.mq.PublishRequest(subj, inbox, payload)
	if err != nil {
		sub.Unsubscribe()
		cb("", nil, err)
		return
	}

	c.tq.Add(sub)
	c.mqReqs[sub] = &responseCont{isReq: true, f: cb}
}

// Subscribe to all events on a resource namespace.
// The namespace has the format "event."+resource
func (c *Client) Subscribe(namespace string, cb mq.Response) (mq.Unsubscriber, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	sub, err := c.mq.ChanSubscribe(namespace+".*", c.mqCh)
	if err != nil {
		return nil, err
	}

	c.Tracef("S=> %s", sub.Subject)

	c.mqReqs[sub] = &responseCont{f: cb}

	us := &Subscription{c: c, sub: sub}
	return us, nil
}

// Unsubscribe removes the subscription.
func (s *Subscription) Unsubscribe() error {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()

	s.c.Tracef("U=> %s", s.sub.Subject)

	delete(s.c.mqReqs, s.sub)
	return s.sub.Unsubscribe()
}

func (c *Client) listener(ch chan *nats.Msg, stopped chan struct{}) {
	for msg := range ch {
		c.mu.Lock()
		rc, ok := c.mqReqs[msg.Sub]
		if ok && rc.isReq {
			// Is the first character a-z or A-Z?
			// Then it is a meta response
			if len(msg.Data) > 0 && (msg.Data[0]|32)-'a' < 26 {
				c.parseMeta(msg, rc)
				c.mu.Unlock()
				c.Tracef("==> (%s): %s", inboxSubstr(msg.Subject), msg.Data)
				continue
			}

			delete(c.mqReqs, msg.Sub)
			c.tq.Remove(msg.Sub)
			if rc.t != nil {
				rc.t.Stop()
			}
			msg.Sub.Unsubscribe()
		}
		c.mu.Unlock()

		if ok {
			if rc.isReq {
				c.Tracef("==> (%s): %s", inboxSubstr(msg.Subject), msg.Data)
			} else {
				c.Tracef("=>> %s: %s", msg.Subject, msg.Data)
			}
			rc.f(msg.Subject, msg.Data, nil)
		}
	}

	close(stopped)
}

func (c *Client) parseMeta(msg *nats.Msg, rc *responseCont) {
	tag := reflect.StructTag(msg.Data)

	// timeout tag
	if v, ok := tag.Lookup("timeout"); ok {
		timeout, err := strconv.Atoi(v)
		if err == nil {
			var removed bool
			if rc.t == nil {
				removed = c.tq.Remove(msg.Sub)
			} else {
				removed = rc.t.Stop()
			}
			if removed {
				rc.t = time.AfterFunc(time.Duration(timeout)*time.Millisecond, func() {
					c.onTimeout(msg.Sub)
				})
			}
		}
	}
}

func (c *Client) onTimeout(v interface{}) {
	sub := v.(*nats.Subscription)

	c.mu.Lock()
	rc, ok := c.mqReqs[sub]
	delete(c.mqReqs, sub)
	c.mu.Unlock()

	if !ok {
		return
	}

	if rc.t != nil {
		rc.t.Stop()
	}
	sub.Unsubscribe()

	c.Tracef("x=> (%s) Request timeout", inboxSubstr(sub.Subject))
	rc.f("", nil, mq.ErrRequestTimeout)
}

func inboxSubstr(s string) string {
	l := len(s)
	if l <= 6 {
		return s
	}
	return s[l-6:]
}
