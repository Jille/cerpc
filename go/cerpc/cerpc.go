// Package cerpc handles RPCs over HTTP.
package cerpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"

	"github.com/Jille/rpcz"
	"github.com/timewasted/go-accept-headers"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type Service interface {
	http.Handler
	Name() string
}

func prefersJSONOutput(r *http.Request) bool {
	ct, _ := accept.Negotiate(r.Header.Get("Accept"), "application/protobuf", "application/json")
	if ct == "" {
		ct, _ = accept.Negotiate(r.Header.Get("Content-Type"), "application/protobuf", "application/json")
	}
	return ct == "application/json"
}

func InternalHandleRequest(w http.ResponseWriter, r *http.Request, inMsg proto.Message, handler func(ctx context.Context) (proto.Message, error)) {
	defer panicHandler(w)
	if !InternalDecodeRequest(w, r, inMsg) {
		return
	}
	ctx := r.Context()
	ctx = context.WithValue(ctx, myContextKey, &cerpcRequest{w, r})

	// Check if the context was already cancelled before we even called the handler.
	if err := ctx.Err(); err != nil {
		// 499 is nginx's non-standard "Client Closed Request" response code.
		fail(w, 499, codes.Canceled, fmt.Sprintf("request was cancelled before RPC handler was called: %v", err))
		return
	}

	if p, err := net.ResolveTCPAddr("tcp", r.RemoteAddr); err == nil {
		ctx = peer.NewContext(ctx, &peer.Peer{Addr: p})
	}

	var outMsg proto.Message
	_, err := rpcz.UnaryServerInterceptor(ctx, inMsg, &grpc.UnaryServerInfo{
		FullMethod: r.URL.Path,
	}, func(ctx context.Context, req interface{}) (interface{}, error) {
		var err error
		outMsg, err = handler(ctx)
		return outMsg, err
	})
	if err != nil {
		WriteError(w, err)
		return
	}
	encoder := proto.Marshal
	contentType := "application/protobuf"
	if prefersJSONOutput(r) {
		encoder = protojson.Marshal
		contentType = "application/json"
	}
	resp, err := encoder(outMsg)
	if err != nil {
		fail(w, http.StatusInternalServerError, codes.Internal, fmt.Sprintf("failed to encode response as %s: %v", contentType, err))
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(resp)))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(resp); err != nil {
		// Client disappeared after we started writing. Nothing we can do now.
		// Nginx uses 499 Client Closed Request for this.
		return
	}
}

func InternalDecodeRequest(w http.ResponseWriter, r *http.Request, inMsg proto.Message) bool {
	if r.Method != "POST" {
		fail(w, http.StatusMethodNotAllowed, codes.InvalidArgument, "RPCs must be done as POST requests")
		return false
	}

	buf, err := ioutil.ReadAll(r.Body)
	if err != nil {
		r.Body.Close()
		fail(w, http.StatusBadRequest, codes.InvalidArgument, fmt.Sprintf("failed to read request body: %v", err))
		return false
	}
	r.Body.Close()

	ct, _ := accept.Negotiate(r.Header.Get("Content-Type"), "application/protobuf", "application/json")
	switch ct {
	case "application/json":
		if err := protojson.Unmarshal(buf, inMsg); err != nil {
			fail(w, http.StatusBadRequest, codes.InvalidArgument, fmt.Sprintf("failed to decode request json-protobuf: %v", err))
			return false
		}
		return true
	case "application/protobuf":
		if err := proto.Unmarshal(buf, inMsg); err != nil {
			fail(w, http.StatusBadRequest, codes.InvalidArgument, fmt.Sprintf("failed to decode request protobuf: %v", err))
			return false
		}
		return true
	default:
		fail(w, http.StatusBadRequest, codes.InvalidArgument, fmt.Sprintf("unexpected Content-Type: %q", r.Header.Get("Content-Type")))
		return false
	}
}

func panicHandler(w http.ResponseWriter) {
	if r := recover(); r != nil {
		log.Printf("Panic in HTTP request: %v", r)
		debug.PrintStack()
		fail(w, http.StatusInternalServerError, codes.Internal, fmt.Sprintf("panic: %v", r))
	}
}

func errorToJSON(c codes.Code, msg string) []byte {
	type errObj struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
	}
	e := errObj{c.String(), msg}
	b, err := json.Marshal(e)
	if err != nil {
		return []byte(`{"type": "internal", "msg": "JSON serialization of error failed"}`)
	}
	return b
}

func fail(w http.ResponseWriter, httpCode int, grpcCode codes.Code, msg string) {
	log.Printf("Failed HTTP RPC: httpCode=%d, grpcCode=%s, msg=%q", httpCode, grpcCode.String(), msg)
	// Error responses are always JSON
	body := errorToJSON(grpcCode, msg)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(httpCode)

	_, _ = w.Write(body)
}

func WriteError(w http.ResponseWriter, err error) {
	st, ok := status.FromError(err)
	if !ok {
		st = status.FromContextError(err)
	}
	httpCode := 500
	switch st.Code() {
	case codes.Unimplemented:
		httpCode = http.StatusNotImplemented
	case codes.Unavailable:
		httpCode = http.StatusServiceUnavailable
	case codes.Unauthenticated:
		httpCode = http.StatusUnauthorized
	default:
		// The rest doesn't really map cleanly/safely/exactly.
	}
	fail(w, httpCode, st.Code(), st.Message())
}

func InternalRejectUnknownRPC(w http.ResponseWriter, name string) {
	fail(w, http.StatusNotFound, codes.NotFound, name+": RPC not found")
}

type contextKey int

var myContextKey contextKey

type cerpcRequest struct {
	w http.ResponseWriter
	r *http.Request
}

// AddHeader sets a header to be returned by the current cerpc HTTP request.
func AddHeader(ctx context.Context, key, value string) bool {
	r, ok := ctx.Value(myContextKey).(*cerpcRequest)
	if !ok {
		return false
	}
	r.w.Header().Add(key, value)
	return true
}

// GetCookie gets a cookie from the current request. The current request is determined from the context.
func GetCookie(ctx context.Context, key string) (*http.Cookie, error) {
	return ctx.Value(myContextKey).(*cerpcRequest).r.Cookie(key)
}

// AttachRequest creates a new context that has the given ResponseWriter and Request attached. It allows GetCookie and AddHeader on the returned context.
// Most users don't need this function because it's automatically attached when using the cerpc generated http handler.
func AttachRequest(ctx context.Context, w http.ResponseWriter, r *http.Request) context.Context {
	return context.WithValue(ctx, myContextKey, &cerpcRequest{w, r})
}
