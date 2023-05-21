package cerpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"path"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Jille/rpcz"
	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
		Error: func(w http.ResponseWriter, r *http.Request, status int, reason error) {
			fail(w, status, codes.Unknown, reason.Error())
		},
	}

	startsWithHttpRe = regexp.MustCompile(`^http`)
)

func InternalUpgrade(w http.ResponseWriter, r *http.Request, handler func(grpc.ServerStream) error) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to convert HTTP request to websocket connection: %v", err)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer ws.Close()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Panic in websocket: %v", r)
			debug.PrintStack()
			ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4001, fmt.Sprintf("%s: panic: %s", codes.Internal.String(), r)), websocketControlDeadline())
		}
	}()
	if p, err := net.ResolveTCPAddr("tcp", r.RemoteAddr); err == nil {
		ctx = peer.NewContext(ctx, &peer.Peer{Addr: p})
	}
	wss := &websocketServerStream{
		websocketStream: websocketStream{
			ws:       ws,
			ctx:      ctx,
			cancel:   cancel,
			incoming: make(chan []byte),
			readErr:  nil,
		},
	}
	go wss.backgroundReader()
	if err := rpcz.StreamServerInterceptor(nil, wss, &grpc.StreamServerInfo{
		FullMethod: r.URL.Path,
	}, func(srv interface{}, stream grpc.ServerStream) error {
		return handler(stream)
	}); err != nil {
		log.Printf("Streaming RPC to %s failed: %v", r.URL.Path, err)
		st, ok := status.FromError(err)
		if !ok {
			st = status.FromContextError(err)
		}
		// TODO: Change protocol to support returning larger errors, as control messages are limited to 125 bytes.
		if err := ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4001, fmt.Sprintf("%s: %s", st.Code().String(), st.Message())), websocketControlDeadline()); err != nil {
			log.Printf("cerpc stream: Failed to send CloseMessage with error (%v): %v", st.Err(), err)
		}
	} else {
		if err := ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "OK"), websocketControlDeadline()); err != nil {
			log.Printf("cerpc stream: Failed to send CloseMessage with success: %v", err)
		}
	}
	// Cancel the context so that any lingering goroutines calling Recv() will get an error rather than getting a partial read from wss.incoming.
	cancel()
	// Wait for the channel to close (discarding incoming messages). This will effectively happen when the client sent us a CloseMessage (in response to ours).
	for range wss.incoming {
	}
}

func InternalDoClientStream(ctx context.Context, urlString string, options []ClientOption) (grpc.ClientStream, error) {
	return rpcz.StreamClientInterceptor(ctx, &grpc.StreamDesc{
		StreamName: path.Base(urlString),
	}, nil, urlString, func(ctx context.Context, _ *grpc.StreamDesc, _ *grpc.ClientConn, _ string, _ ...grpc.CallOption) (grpc.ClientStream, error) {
		return doClientStreamAfterRpcz(ctx, urlString, options)
	})
}

func doClientStreamAfterRpcz(ctx context.Context, urlString string, options []ClientOption) (grpc.ClientStream, error) {
	co := clientOptions{
		headers: http.Header{},
	}
	for _, o := range options {
		o(&co)
	}

	ws, response, err := co.websocketDialer.DialContext(ctx, startsWithHttpRe.ReplaceAllString(urlString, "ws"), co.headers)
	if err != nil {
		if response == nil {
			return nil, err
		}
		if response.StatusCode == 200 {
			return nil, status.Error(codes.Internal, "HTTP 200 response when trying to initiate websocket")
		}
		respBytes, err := ioutil.ReadAll(response.Body)
		if err != nil {
			if st := status.FromContextError(err); st.Code() != codes.Unknown {
				return nil, st.Err()
			}
			return nil, status.Errorf(codes.Unavailable, "failed to read HTTP response: %v", err)
		}
		if err := response.Body.Close(); err != nil {
			if st := status.FromContextError(err); st.Code() != codes.Unknown {
				return nil, st.Err()
			}
			return nil, status.Errorf(codes.Unavailable, "failed to read HTTP response: %v", err)
		}
		if response.Header.Get("Content-Type") != "application/json" {
			return nil, status.Errorf(codes.Internal, "HTTP %d error response was not JSON: %s", response.StatusCode, respBytes)
		}
		var e struct {
			Code codes.Code `json:"code"`
			Msg  string     `json:"msg"`
		}
		if err := json.Unmarshal(respBytes, &e); err != nil {
			return nil, status.Errorf(codes.Internal, "HTTP %d error response was not valid JSON: %s", response.StatusCode, respBytes)
		}
		if e.Code == codes.OK {
			return nil, status.Errorf(codes.Internal, "HTTP %d error response contained code 0 (OK): %s", response.StatusCode, respBytes)
		}
		return nil, status.Error(e.Code, e.Msg)
	}
	done := make(chan struct{})
	wss := &websocketClientStream{
		websocketStream: websocketStream{
			ws:       ws,
			ctx:      ctx,
			cancel:   func() { close(done) },
			incoming: make(chan []byte),
			readErr:  nil,
		},
	}
	go wss.backgroundReader()
	go wss.contextWatcher(done)
	return wss, nil
}

type websocketStream struct {
	ws       *websocket.Conn
	ctx      context.Context
	cancel   func()
	incoming chan []byte
	readErr  error
}

func (w *websocketStream) Context() context.Context {
	return w.ctx
}

func (w *websocketStream) SendMsg(m interface{}) error {
	pm, ok := m.(proto.Message)
	if !ok {
		return status.Error(codes.Internal, "websocketStream.SendMsg() called with a non-protobuf")
	}
	b, err := proto.Marshal(pm)
	if err != nil {
		return status.Errorf(codes.Internal, "websocketStream.SendMsg(): protobuf encode failed: %v", err)
	}
	w.ws.SetWriteDeadline(time.Now().Add(time.Minute))
	if err := w.ws.WriteMessage(websocket.BinaryMessage, b); err != nil {
		if err == websocket.ErrCloseSent {
			if err := w.ctx.Err(); err != nil {
				return err
			}
		}
		return status.Errorf(codes.Internal, "websocketStream.SendMsg(): WriteMessage(): %v", err)
	}
	w.ws.SetWriteDeadline(time.Time{})
	return nil
}

func (w *websocketStream) backgroundReader() {
	var closeSendCalled bool
	for {
		t, b, err := w.ws.ReadMessage()
		if err != nil {
			if !closeSendCalled {
				if ce, ok := err.(*websocket.CloseError); ok {
					sp := strings.SplitN(ce.Text, ": ", 2)
					if ce.Code == 1000 {
						w.readErr = nil
					} else if ce.Code != 4001 || len(sp) != 2 {
						w.readErr = status.Errorf(codes.Internal, "websocket closed unexpectedly: %d: %s", ce.Code, ce.Text)
					} else {
						w.readErr = status.Error(codeFromString(sp[0]), sp[1])
					}
				} else {
					w.readErr = status.Errorf(codes.Unavailable, "websocketStream.RecvMsg(): ReadMessage(): %v", err)
				}
				close(w.incoming)
			}
			w.cancel()
			return
		}
		if closeSendCalled {
			// Client promised not to send anything else, but broke their promise.
			continue
		}
		switch t {
		case websocket.BinaryMessage:
			w.incoming <- b
		case websocket.TextMessage:
			// Client called CloseSend()
			w.readErr = io.EOF
			close(w.incoming)
			closeSendCalled = true
		}
	}
}

func (w *websocketStream) RecvMsg(m interface{}) error {
	pm, ok := m.(proto.Message)
	if !ok {
		return status.Error(codes.Internal, "websocketStream.RecvMsg() called with a non-protobuf")
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}
	select {
	case <-w.ctx.Done():
		return w.ctx.Err()
	case b, ok := <-w.incoming:
		if !ok {
			return w.readErr
		}
		if err := proto.Unmarshal(b, pm); err != nil {
			return status.Errorf(codes.Internal, "websocketStream.RecvMsg(): protobuf decode failed: %v", err)
		}
		return nil
	}
}

type websocketServerStream struct {
	websocketStream
}

func (w *websocketServerStream) SetHeader(metadata.MD) error {
	return status.Error(codes.Unimplemented, "websocketServerStream.SetHeader() is not implemented")
}

func (w *websocketServerStream) SendHeader(metadata.MD) error {
	return status.Error(codes.Unimplemented, "websocketServerStream.SendHeader() is not implemented")
}

func (w *websocketServerStream) SetTrailer(metadata.MD) {
}

var _ grpc.ServerStream = (*websocketServerStream)(nil)

type websocketClientStream struct {
	websocketStream
}

func (w *websocketClientStream) Header() (metadata.MD, error) {
	return nil, status.Error(codes.Unimplemented, "websocketClientStream.Header() is not implemented")
}

func (w *websocketClientStream) Trailer() metadata.MD {
	return nil
}

func (w *websocketClientStream) CloseSend() error {
	w.ws.SetWriteDeadline(time.Now().Add(time.Minute))
	if err := w.ws.WriteMessage(websocket.TextMessage, []byte("closeSend")); err != nil {
		w.ws.Close()
		return status.Errorf(codes.Internal, "websocketStream.CloseSend(): WriteMessage(): %v", err)
	}
	w.ws.SetWriteDeadline(time.Time{})
	return nil
}

// contextWatcher closes the websocket when the context is done.
// It runs only for client streams (because for server streams we own the context).
func (w *websocketClientStream) contextWatcher(done chan struct{}) {
	select {
	case <-w.ctx.Done():
		st := status.FromContextError(w.ctx.Err())
		w.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4001, fmt.Sprintf("%s: %s", st.Code().String(), st.Message())), websocketControlDeadline())
	case <-done:
		// The backgroundReader() called cancel() (and then died). This connection is gone, nothing left for us to do.
		return
	}
}

var _ grpc.ClientStream = (*websocketClientStream)(nil)

func codeFromString(c string) codes.Code {
	ret := codes.Unknown
	_ = ret.UnmarshalJSON([]byte(`"` + c + `"`))
	return ret
}

func websocketControlDeadline() time.Time {
	return time.Now().Add(time.Minute)
}
