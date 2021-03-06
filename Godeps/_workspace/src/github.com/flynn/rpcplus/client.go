// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rpcplus

import (
	"bufio"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"reflect"
	"sync"
)

// ServerError represents an error that has been returned from
// the remote side of the RPC connection.
type ServerError string

func (e ServerError) Error() string {
	return string(e)
}

var ErrShutdown = errors.New("connection is shut down")

// Call represents an active RPC.
type Call struct {
	ServiceMethod string      // The name of the service and method to call.
	Args          interface{} // The argument to the function (*struct).
	Reply         interface{} // The reply from the function (*struct for single, chan * struct for streaming).
	Error         error       // After completion, the error status.
	Done          chan *Call  // Strobes when call is complete (nil for streaming RPCs)
	Stream        bool        // True for a streaming RPC call, false otherwise

	seq    uint64
	sent   chan struct{}
	client *Client
}

// CloseStream closes the associated stream
func (c *Call) CloseStream() error {
	if !c.Stream {
		return errors.New("rpc: cannot close non-stream request")
	}
	<-c.sent
	c.client.sending.Lock()
	defer c.client.sending.Unlock()

	c.client.mutex.Lock()
	if c.client.shutdown {
		c.client.mutex.Unlock()
		return ErrShutdown
	}
	c.client.mutex.Unlock()

	c.client.request.ServiceMethod = "CloseStream"
	c.client.request.Seq = c.seq
	return c.client.codec.WriteRequest(&c.client.request, struct{}{})
}

// Client represents an RPC Client.
// There may be multiple outstanding Calls associated
// with a single Client, and a Client may be used by
// multiple goroutines simultaneously.
type Client struct {
	mutex    sync.Mutex // protects pending, seq, request
	sending  sync.Mutex
	request  Request
	seq      uint64
	codec    ClientCodec
	pending  map[uint64]*Call
	closing  bool
	shutdown bool
}

// A ClientCodec implements writing of RPC requests and
// reading of RPC responses for the client side of an RPC session.
// The client calls WriteRequest to write a request to the connection
// and calls ReadResponseHeader and ReadResponseBody in pairs
// to read responses.  The client calls Close when finished with the
// connection. ReadResponseBody may be called with a nil
// argument to force the body of the response to be read and then
// discarded.
type ClientCodec interface {
	WriteRequest(*Request, interface{}) error
	ReadResponseHeader(*Response) error
	ReadResponseBody(interface{}) error

	Close() error
}

func (client *Client) send(call *Call) {
	client.sending.Lock()
	defer client.sending.Unlock()

	// Register this call.
	client.mutex.Lock()
	if client.shutdown {
		call.Error = ErrShutdown
		client.mutex.Unlock()
		call.done()
		return
	}
	seq := client.seq
	client.seq++
	client.pending[seq] = call
	client.mutex.Unlock()

	// Encode and send the request.
	client.request.Seq = seq
	client.request.ServiceMethod = call.ServiceMethod
	err := client.codec.WriteRequest(&client.request, call.Args)
	if call.Stream {
		call.seq = seq
		close(call.sent)
	}
	if err != nil {
		client.mutex.Lock()
		call = client.pending[seq]
		delete(client.pending, seq)
		client.mutex.Unlock()
		if call != nil {
			call.Error = err
			call.done()
		}
	}
}

func (client *Client) input() {
	var err error
	var response Response
	for err == nil {
		response = Response{}
		err = client.codec.ReadResponseHeader(&response)
		if err != nil {
			if err == io.EOF && !client.closing {
				err = io.ErrUnexpectedEOF
			}
			break
		}
		seq := response.Seq
		client.mutex.Lock()
		call := client.pending[seq]
		client.mutex.Unlock()

		switch {
		case call == nil:
			// We've got no pending call. That usually means that
			// WriteRequest partially failed, and call was already
			// removed; response is a server telling us about an
			// error reading request body. We should still attempt
			// to read error body, but there's no one to give it to.
			err = client.codec.ReadResponseBody(nil)
			if err != nil {
				err = errors.New("reading error body: " + err.Error())
			}
		case response.Error != "":
			// We've got an error response. Give this to the request;
			// any subsequent requests will get the ReadResponseBody
			// error if there is one.
			if !(call.Stream && response.Error == lastStreamResponseError) {
				call.Error = ServerError(response.Error)
			}
			err = client.codec.ReadResponseBody(nil)
			if err != nil {
				err = errors.New("reading error payload: " + err.Error())
			}
			client.done(seq)
		case call.Stream:
			// call.Reply is a chan *T2
			// we need to create a T2 and get a *T2 back
			value := reflect.New(reflect.TypeOf(call.Reply).Elem().Elem()).Interface()
			err = client.codec.ReadResponseBody(value)
			if err != nil {
				call.Error = errors.New("reading body " + err.Error())
			} else {
				// writing on the channel could block forever. For
				// instance, if a client calls 'close', this might block
				// forever.  the current suggestion is for the
				// client to drain the receiving channel in that case
				reflect.ValueOf(call.Reply).Send(reflect.ValueOf(value))
			}
		default:
			err = client.codec.ReadResponseBody(call.Reply)
			if err != nil {
				call.Error = errors.New("reading body " + err.Error())
			}
			client.done(seq)
		}
	}
	// Terminate pending calls.
	client.sending.Lock()
	client.mutex.Lock()
	client.shutdown = true
	closing := client.closing
	for _, call := range client.pending {
		call.Error = err
		call.done()
	}
	client.mutex.Unlock()
	client.sending.Unlock()
	if err != io.EOF && !closing {
		log.Println("rpc: client protocol error:", err)
	}
}

func (client *Client) done(seq uint64) {
	client.mutex.Lock()
	call := client.pending[seq]
	delete(client.pending, seq)
	client.mutex.Unlock()

	if call != nil {
		call.done()
	}
}

func (call *Call) done() {
	if call.Stream {
		// need to close the channel. Client won't be able to read any more.
		reflect.ValueOf(call.Reply).Close()
		return
	}

	select {
	case call.Done <- call:
		// ok
	default:
		// We don't want to block here.  It is the caller's responsibility to make
		// sure the channel has enough buffer space. See comment in Go().
		log.Println("rpc: discarding Call reply due to insufficient Done chan capacity")
	}
}

// NewClient returns a new Client to handle requests to the
// set of services at the other end of the connection.
// It adds a buffer to the write side of the connection so
// the header and payload are sent as a unit.
func NewClient(conn io.ReadWriteCloser) *Client {
	encBuf := bufio.NewWriter(conn)
	client := &gobClientCodec{conn, gob.NewDecoder(conn), gob.NewEncoder(encBuf), encBuf}
	return NewClientWithCodec(client)
}

// NewClientWithCodec is like NewClient but uses the specified
// codec to encode requests and decode responses.
func NewClientWithCodec(codec ClientCodec) *Client {
	client := &Client{
		codec:   codec,
		pending: make(map[uint64]*Call),
	}
	go client.input()
	return client
}

type gobClientCodec struct {
	rwc    io.ReadWriteCloser
	dec    *gob.Decoder
	enc    *gob.Encoder
	encBuf *bufio.Writer
}

func (c *gobClientCodec) WriteRequest(r *Request, body interface{}) (err error) {
	if err = c.enc.Encode(r); err != nil {
		return
	}
	if err = c.enc.Encode(body); err != nil {
		return
	}
	return c.encBuf.Flush()
}

func (c *gobClientCodec) ReadResponseHeader(r *Response) error {
	return c.dec.Decode(r)
}

func (c *gobClientCodec) ReadResponseBody(body interface{}) error {
	return c.dec.Decode(body)
}

func (c *gobClientCodec) Close() error {
	return c.rwc.Close()
}

// DialHTTP connects to an HTTP RPC server at the specified network address
// listening on the default HTTP RPC path.
func DialHTTP(network, address string) (*Client, error) {
	return DialHTTPPath(network, address, DefaultRPCPath, nil)
}

type DialFunc func(network, address string) (net.Conn, error)

// DialHTTPPath connects to an HTTP RPC server
// at the specified network address and path.
func DialHTTPPath(network, address, path string, dial DialFunc) (*Client, error) {
	if dial == nil {
		dial = net.Dial
	}
	var err error
	conn, err := dial(network, address)
	if err != nil {
		return nil, err
	}
	client, err := NewHTTPClient(conn, path, nil)
	if err != nil {
		conn.Close()
		return nil, &net.OpError{
			Op:   "dial-http",
			Net:  network + " " + address,
			Addr: nil,
			Err:  err,
		}
	}
	return client, nil
}

func NewHTTPClient(conn io.ReadWriteCloser, path string, header http.Header) (*Client, error) {
	if header == nil {
		header = make(http.Header)
	}
	header.Set("Accept", "application/vnd.flynn.rpc-hijack+gob")

	fmt.Fprintf(conn, "CONNECT %s HTTP/1.0\r\n", path)
	header.Write(conn)
	conn.Write([]byte("\r\n"))

	// Require successful HTTP response
	// before switching to RPC protocol.
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
	if err != nil || resp.Status != connected {
		if err == nil {
			err = errors.New("unexpected HTTP response: " + resp.Status)
		}
		return nil, err
	}
	return NewClient(conn), nil
}

// Dial connects to an RPC server at the specified network address.
func Dial(network, address string) (*Client, error) {
	conn, err := net.Dial(network, address)
	if err != nil {
		return nil, err
	}
	return NewClient(conn), nil
}

func (client *Client) Close() error {
	client.mutex.Lock()
	if client.shutdown || client.closing {
		client.mutex.Unlock()
		return ErrShutdown
	}
	client.closing = true
	client.mutex.Unlock()
	return client.codec.Close()
}

// Go invokes the function asynchronously.  It returns the Call structure representing
// the invocation.  The done channel will signal when the call is complete by returning
// the same Call object.  If done is nil, Go will allocate a new channel.
// If non-nil, done must be buffered or Go will deliberately crash.
func (client *Client) Go(serviceMethod string, args interface{}, reply interface{}, done chan *Call) *Call {
	call := new(Call)
	call.ServiceMethod = serviceMethod
	call.Args = args
	call.Reply = reply
	if done == nil {
		done = make(chan *Call, 10) // buffered.
	} else {
		// If caller passes done != nil, it must arrange that
		// done has enough buffer for the number of simultaneous
		// RPCs that will be using that channel.  If the channel
		// is totally unbuffered, it's best not to run at all.
		if cap(done) == 0 {
			log.Panic("rpc: done channel is unbuffered")
		}
	}
	call.Done = done
	client.send(call)
	return call
}

// Go invokes the streaming function asynchronously.  It returns the Call structure representing
// the invocation.
func (client *Client) StreamGo(serviceMethod string, args interface{}, replyStream interface{}) *Call {
	// first check the replyStream object is a stream of pointers to a data structure
	typ := reflect.TypeOf(replyStream)
	// FIXME: check the direction of the channel, maybe?
	if typ.Kind() != reflect.Chan || typ.Elem().Kind() != reflect.Ptr {
		log.Panic("rpc: replyStream is not a channel of pointers")
		return nil
	}

	call := new(Call)
	call.ServiceMethod = serviceMethod
	call.Args = args
	call.Reply = replyStream
	call.Stream = true
	call.sent = make(chan struct{})
	call.client = client
	client.send(call)
	return call
}

// Call invokes the named function, waits for it to complete, and returns its error status.
func (client *Client) Call(serviceMethod string, args interface{}, reply interface{}) error {
	call := <-client.Go(serviceMethod, args, reply, make(chan *Call, 1)).Done
	return call.Error
}
