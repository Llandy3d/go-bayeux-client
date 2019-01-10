package bayeux

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/obeattie/ohmyglob"
	"gopkg.in/tomb.v2"
)

const (
	VERSION         = "1.0"
	MINIMUM_VERSION = "1.0"
)

// Client allows connecting to a Bayeux server and subscribing to channels.
type Client struct {
	mtx           sync.RWMutex
	url           string
	clientId      string
	tomb          *tomb.Tomb
	subscriptions map[string]interface{}
	messages      chan *Message
	connected     bool
	http          *http.Client
	interval      time.Duration
}

// Message is the type delivered to subscribers.
type Message struct {
	Channel   string          `json:"channel"`
	Data      json.RawMessage `json:"data,omitempty"`
	Id        string          `json:"id,omitempty"`
	ClientId  string          `json:"clientId,omitempty"`
	Extension interface{}     `json:"ext,omitempty"`
}

type subscription struct {
	glob ohmyglob.Glob
	out  chan<- *Message
}

type request struct {
	Channel                  string          `json:"channel"`
	Data                     json.RawMessage `json:"data,omitempty"`
	Id                       string          `json:"id,omitempty"`
	ClientId                 string          `json:"clientId,omitempty"`
	Extension                interface{}     `json:"ext,omitempty"`
	Version                  string          `json:"version,omitempty"`
	MinimumVersion           string          `json:"minimumVersion,omitempty"`
	SupportedConnectionTypes []string        `json:"supportedConnectionTypes,omitempty"`
	ConnectionType           string          `json:"connectionType,omitempty"`
	Subscription             string          `json:"subscription,omitempty"`
}

type advice struct {
	Reconnect string `json:"reconnect,omitempty"`
	Timeout   int64  `json:"timeout,omitempty"`
	Interval  int    `json:"interval,omitempty"`
}

type metaMessage struct {
	Message
	Version                  string   `json:"version,omitempty"`
	MinimumVersion           string   `json:"minimumVersion,omitempty"`
	SupportedConnectionTypes []string `json:"supportedConnectionTypes,omitempty"`
	ConnectionType           string   `json:"connectionType,omitempty"`
	Timestamp                string   `json:"timestamp,omitempty"`
	Successful               bool     `json:"successful"`
	Subscription             string   `json:"subscription,omitempty"`
	Error                    string   `json:"error,omitempty"`
	Advice                   *advice  `json:"advice,omitempty"`
}

// NewClient initialises a new Bayeux client. By default `http.DefaultClient`
// is used for HTTP connections.
func NewClient(url string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	return &Client{
		url:           url,
		http:          httpClient,
		messages:      make(chan *Message, 100),
		subscriptions: make(map[string]interface{}),
	}
}

// Connect performs a handshake with the server and will repeatedly initiate a
// long-polling connection until `Close` is called on the client.
func (c *Client) Connect() error {
	return c.ensureConnected()
}

// Close notifies the Bayeux server of the intent to disconnect and terminates
// the background polling loop.
func (c *Client) Close() error {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	c.tomb.Killf("Close")
	c.connected = false
	return c.disconnect()
}

func (c *Client) Unsubscribe(pattern string) error {
	rsp, err := c.send(&request{
		Channel:      "/meta/unsubscribe",
		ClientId:     c.clientId,
		Subscription: pattern,
	})
	if err != nil {
		return err
	}
	if !rsp.Successful {
		return errors.New(rsp.Error)
	}

	c.doForgetSubscription(pattern)

	return nil
}

// ForgetSubscription ensure to remove subscription object from
// the c.subscriptions slices. In back-side, the channel out
// inside it should have been closed before
// we search for 1st occurence of pattern
func (c *Client) doForgetSubscription(pattern string) {
	c.mtx.Lock()
	defer c.mtx.Unlock()

	c.subscriptions[pattern] = ""
}

// Subscribe is like `SubscribeExt` with a blank `ext` part.
func (c *Client) Subscribe(pattern string, out chan<- *Message) error {
	return c.SubscribeExt(pattern, out, nil)
}

// SubscribeExt creates a new subscription on the Bayeux server. Messages for
// the subscription will be delivered on the given channel `out`. If the client
// has not performed a handshake already, it will do so first.
func (c *Client) SubscribeExt(pattern string, out chan<- *Message, ext interface{}) error {
	if err := c.ensureConnected(); err != nil {
		return err
	}
	return c.subscribe(pattern, out, ext)
}

func (c *Client) ensureConnected() error {
	c.mtx.RLock()
	connected := c.connected
	c.mtx.RUnlock()

	if connected {
		return nil
	}

	c.mtx.Lock()
	defer c.mtx.Unlock()
	if c.connected {
		return nil
	}
	err := c.handshake()
	if err == nil {
		c.connected = err == nil
		c.tomb = &tomb.Tomb{}
		c.tomb.Go(c.worker)
	}
	return err
}

func (c *Client) worker() error {
	for {
		select {
		case msg := <-c.messages:
			for _, sub := range c.subscriptions {
				if s, subOpened := sub.(subscription); subOpened {
					if s.glob.MatchString(msg.Channel) {
						s.out <- msg
					}
				}
			}
		case <-c.tomb.Dying():
			return nil
		case <-time.After(c.interval):
			_, err := c.connect()
			if err != nil {
				log.Printf("[WRN] Bayeux connect failed: %s", err)
			}
		}
	}
}

func (c *Client) handshake() error {
	rsp, err := c.send(&request{
		Channel:                  "/meta/handshake",
		Version:                  VERSION,
		MinimumVersion:           MINIMUM_VERSION,
		SupportedConnectionTypes: []string{"long-polling"},
	})
	if err != nil {
		return err
	}
	if !rsp.Successful {
		return errors.New(rsp.Error)
	}
	c.clientId = rsp.ClientId
	return nil
}

func (c *Client) connect() (*metaMessage, error) {
	rsp, err := c.send(&request{
		Channel:        "/meta/connect",
		ClientId:       c.clientId,
		ConnectionType: "long-polling",
	})
	return rsp, err
}

func (c *Client) disconnect() error {
	rsp, err := c.send(&request{
		Channel:  "/meta/disconnect",
		ClientId: c.clientId,
	})
	if err != nil {
		return err
	}
	if !rsp.Successful {
		return errors.New(rsp.Error)
	}
	return nil
}

func (c *Client) subscribe(pattern string, out chan<- *Message, ext interface{}) error {
	glob, err := ohmyglob.Compile(pattern, nil)
	if err != nil {
		return fmt.Errorf("Invalid pattern: %s", err)
	}
	rsp, err := c.send(&request{
		Channel:      "/meta/subscribe",
		ClientId:     c.clientId,
		Subscription: pattern,
		Extension:    ext,
	})
	if err != nil {
		return err
	}
	if !rsp.Successful {
		return errors.New(rsp.Error)
	}

	c.mtx.Lock()
	defer c.mtx.Unlock()
	c.subscriptions[glob.String()] = subscription{
		glob: glob,
		out:  out,
	}

	return nil
}

func (c *Client) send(req *request) (*metaMessage, error) {
	data, err := json.Marshal([]*request{req})
	if err != nil {
		return nil, err
	}
	buffer := bytes.NewBuffer(data)
	rsp, err := c.http.Post(c.url, "application/json", buffer)
	if err != nil {
		return nil, err
	}

	if rsp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP Status %d", rsp.StatusCode)
	}

	data, err = ioutil.ReadAll(rsp.Body)
	if err != nil {
		return nil, err
	}
	defer rsp.Body.Close()

	var messages []metaMessage
	var reply *metaMessage
	if err = json.Unmarshal(data, &messages); err != nil {
		return nil, err
	}

	// 1. Check advice: Update interval
	// 2. Check advice: Reconnect "handshake" => reconnect
	// 3. Handle messages to just-created subscriptions

	for _, msg := range messages {
		if req.Channel == msg.Channel {
			reply = &msg
		} else {
			c.messages <- &msg.Message
		}
	}

	return reply, err
}
