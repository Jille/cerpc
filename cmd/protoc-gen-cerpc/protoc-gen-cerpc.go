// Binary protoc-gen-cerpc generates CERPC bindings that forward to gRPC methods.
package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/types/pluginpb"
)

func main() {
	protogen.Options{}.Run(func(gen *protogen.Plugin) error {
		gen.SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)
		for _, f := range gen.Files {
			if !f.Generate {
				continue
			}
			generateFile(gen, f)
		}
		return nil
	})
}

// generateFile generates a .cerpc.go file containing a HTTP handler that serves the RPCs.
func generateFile(gen *protogen.Plugin, file *protogen.File) {
	filename := file.GeneratedFilenamePrefix + ".cerpc.go"
	g := gen.NewGeneratedFile(filepath.Base(filename), file.GoImportPath)
	g.P("// Code generated by protoc-gen-cerpc. DO NOT EDIT.")
	g.P()
	g.P("package ", file.GoPackageName)

	cerpc := func(n string) protogen.GoIdent {
		return protogen.GoIdent{
			GoName:       n,
			GoImportPath: "github.com/Jille/cerpc/go/cerpc",
		}
	}
	http := func(n string) protogen.GoIdent {
		return protogen.GoIdent{
			GoName:       n,
			GoImportPath: "net/http",
		}
	}
	grpc := func(n string) protogen.GoIdent {
		return protogen.GoIdent{
			GoName:       n,
			GoImportPath: "google.golang.org/grpc",
		}
	}

	for _, s := range file.Services {
		g.P()
		g.P("func New", s.GoName, "HTTPHandler(impl ", s.GoName, "Server) ", cerpc("Service"), " {")
		g.P("	return _", s.GoName, "HTTPHandler{impl}")
		g.P("}")

		g.P()
		g.P("type _", s.GoName, "HTTPHandler struct{")
		g.P("	impl ", s.GoName, "Server")
		g.P("}")
		g.P()
		g.P("func (_", s.GoName, "HTTPHandler) Name() string {")
		g.P(fmt.Sprintf("	return %q", s.Desc.FullName()))
		g.P("}")
		g.P()

		g.P("func (s _", s.GoName, "HTTPHandler) ServeHTTP(w ", http("ResponseWriter"), ", r *", http("Request"), ") {")
		g.P("	switch ", protogen.GoIdent{"Base", "path"}, "(r.URL.Path) {")

		for _, m := range s.Methods {
			g.P(fmt.Sprintf("	case %q:", m.Desc.Name()))
			if m.Desc.IsStreamingClient() || m.Desc.IsStreamingServer() {
				g.P("		", cerpc("InternalUpgrade"), "(w, r, func(stream ", grpc("ServerStream"), ") error {")
				g.P("			sw := &", strings.ToLower(s.GoName[:1]), s.GoName[1:], m.GoName, "Server{stream}")
				if !m.Desc.IsStreamingClient() {
					g.P("			req := new(", m.Input.GoIdent, ")")
					g.P("			if err := sw.RecvMsg(req); err != nil {")
					g.P("				return err")
					g.P("			}")
					g.P("			return s.impl.", m.GoName, "(req, sw)")
				} else {
					g.P("			return s.impl.", m.GoName, "(sw)")
				}
				g.P("		})")
			} else {
				g.P("		var inMsg ", m.Input.GoIdent)
				g.P("		", cerpc("InternalHandleRequest"), `(w, r, &inMsg, func(ctx `, protogen.GoIdent{"Context", "context"}, `) (`, protogen.GoIdent{"Message", "google.golang.org/protobuf/proto"}, `, error) {`)
				g.P("			return s.impl.", m.GoName, "(ctx, &inMsg)")
				g.P("		})")
			}
			g.P()
		}
		g.P("	default:")
		g.P(cerpc("InternalRejectUnknownRPC"), `(w, path.Base(r.URL.Path))`)
		g.P("	}")
		g.P("}")

		g.P()
		g.P("func New", s.GoName, "CeRPCClient(baseUrl string, options ...", cerpc("ClientOption"), ") ", s.GoName, "CeRPCClient {")
		g.P("	return ", s.GoName, "CeRPCClient{")
		g.P(`		baseUrl: `, protogen.GoIdent{"TrimSuffix", "strings"}, `(baseUrl, "/"),`)
		g.P("		options: options,")
		g.P("	}")
		g.P("}")

		g.P()
		g.P("type ", s.GoName, "CeRPCClient struct{")
		g.P("	baseUrl string")
		g.P("	options []", cerpc("ClientOption"))
		g.P("}")
		g.P()

		for _, m := range s.Methods {
			if m.Desc.IsStreamingClient() || m.Desc.IsStreamingServer() {
				if m.Desc.IsStreamingClient() {
					g.P("func (c ", s.GoName, "CeRPCClient) ", m.GoName, "(ctx ", protogen.GoIdent{"Context", "context"}, ") (", s.GoName, "_", m.GoName, "Client, error) {")
				} else {
					g.P("func (c ", s.GoName, "CeRPCClient) ", m.GoName, "(ctx ", protogen.GoIdent{"Context", "context"}, ", req *", m.Input.GoIdent, ") (", s.GoName, "_", m.GoName, "Client, error) {")
					g.P("	ctx, cancel := context.WithCancel(ctx)")
				}
				g.P(`	stream, err := `, cerpc("InternalDoClientStream"), fmt.Sprintf("(ctx, c.baseUrl+%q, c.options)", "/"+string(s.Desc.FullName())+"/"+m.GoName))
				g.P("	if err != nil {")
				if !m.Desc.IsStreamingClient() {
					g.P("		cancel()")
				}
				g.P("		return nil, err")
				g.P("	}")
				g.P("	sw := &", strings.ToLower(s.GoName[:1]), s.GoName[1:], m.GoName, "Client{stream}")
				if !m.Desc.IsStreamingClient() {
					g.P("	if err := stream.SendMsg(req); err != nil {")
					g.P("		cancel()")
					g.P("		return nil, err")
					g.P("	}")
					g.P("	", protogen.GoIdent{"SetFinalizer", "runtime"}, "(sw, func(*", strings.ToLower(s.GoName[:1]), s.GoName[1:], m.GoName, "Client) { cancel() })")
				}
				g.P("	return sw, nil")
				g.P("}")
			} else {
				g.P("func (c ", s.GoName, "CeRPCClient) ", m.GoName, "(ctx ", protogen.GoIdent{"Context", "context"}, ", req *", m.Input.GoIdent, ") (*", m.Output.GoIdent, ", error) {")
				g.P("	resp := new(", m.Output.GoIdent, ")")
				g.P(`	err := `, cerpc("InternalDoClientRequest"), fmt.Sprintf("(ctx, c.baseUrl+%q, req, resp, c.options)", "/"+string(s.Desc.FullName())+"/"+m.GoName))
				g.P("	return resp, err")
				g.P("}")
			}
			g.P()
		}
	}
}
