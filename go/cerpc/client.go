package cerpc

import (
	"bytes"
	"context"
	"encoding/json"
	"io/ioutil"
	"net/http"

	"github.com/Jille/rpcz"
	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func InternalDoClientRequest(ctx context.Context, url string, req, resp proto.Message, options []ClientOption) error {
	return rpcz.UnaryClientInterceptor(ctx, url, req, resp, nil, func(ctx context.Context, _ string, _, reply interface{}, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		err := doClientRequestAfterRpcz(ctx, url, req, resp, options)
		reply = resp
		return err
	})
}

func doClientRequestAfterRpcz(ctx context.Context, url string, req, resp proto.Message, options []ClientOption) error {
	b, err := proto.Marshal(req)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to marshal request protobuf: %v", err)
	}
	hr, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to create HTTP request: %v", err)
	}
	hr.Header.Set("Content-Type", "application/protobuf")
	hr.Header.Set("Accept", "application/protobuf")
	co := clientOptions{
		httpClient: http.DefaultClient,
		headers:    hr.Header,
	}
	for _, o := range options {
		o(&co)
	}
	response, err := co.httpClient.Do(hr)
	if err != nil {
		if st := status.FromContextError(err); st.Code() != codes.Unknown {
			return st.Err()
		}
		return status.Errorf(codes.Unavailable, "failed to send HTTP request: %v", err)
	}
	respBytes, err := ioutil.ReadAll(response.Body)
	if err != nil {
		if st := status.FromContextError(err); st.Code() != codes.Unknown {
			return st.Err()
		}
		return status.Errorf(codes.Unavailable, "failed to read HTTP response: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		if st := status.FromContextError(err); st.Code() != codes.Unknown {
			return st.Err()
		}
		return status.Errorf(codes.Unavailable, "failed to read HTTP response: %v", err)
	}
	if response.StatusCode == 200 {
		if err := proto.Unmarshal(respBytes, resp); err != nil {
			return status.Errorf(codes.Internal, "HTTP 200 response contained invalid protobuf data: %v", err)
		}
		return nil
	}
	if response.Header.Get("Content-Type") != "application/json" {
		return status.Errorf(codes.Internal, "HTTP %d error response was not JSON: %s", response.StatusCode, respBytes)
	}
	var e struct {
		Code codes.Code `json:"code"`
		Msg  string     `json:"msg"`
	}
	if err := json.Unmarshal(respBytes, &e); err != nil {
		return status.Errorf(codes.Internal, "HTTP %d error response was not valid JSON: %s", response.StatusCode, respBytes)
	}
	if e.Code == codes.OK {
		return status.Errorf(codes.Internal, "HTTP %d error response contained code 0 (OK): %s", response.StatusCode, respBytes)
	}
	return status.Error(e.Code, e.Msg)
}

type clientOptions struct {
	httpClient      *http.Client
	headers         http.Header
	websocketDialer websocket.Dialer
}

// ClientOption allows you to change the default settings of the generated New*CeRPCClient().
type ClientOption func(*clientOptions)

// WithHTTPClient lets the Client use this http.Client rather than http.DefaultClient.
func WithHTTPClient(h *http.Client) ClientOption {
	return func(c *clientOptions) {
		c.httpClient = h
	}
}

// WithWebsocketDialer lets the Client use this websocket.Dialer rather than the default.
func WithWebsocketDialer(w websocket.Dialer) ClientOption {
	return func(c *clientOptions) {
		c.websocketDialer = w
	}
}

// WithHTTPHeaders lets the Client send these extra headers.
func WithHTTPHeaders(h http.Header) ClientOption {
	return func(c *clientOptions) {
		for k, v := range h {
			c.headers[k] = v
		}
	}
}
