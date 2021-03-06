/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package streaming

import (
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	restful "github.com/emicklei/go-restful"

	"k8s.io/apimachinery/pkg/types"
	runtimeapi "k8s.io/kubernetes/pkg/kubelet/api/v1alpha1/runtime"
	"k8s.io/kubernetes/pkg/kubelet/server/portforward"
	"k8s.io/kubernetes/pkg/kubelet/server/remotecommand"
	"k8s.io/kubernetes/pkg/util/term"
)

// The library interface to serve the stream requests.
type Server interface {
	http.Handler

	// Get the serving URL for the requests.
	// Requests must not be nil. Responses may be nil iff an error is returned.
	GetExec(*runtimeapi.ExecRequest) (*runtimeapi.ExecResponse, error)
	GetAttach(req *runtimeapi.AttachRequest) (*runtimeapi.AttachResponse, error)
	GetPortForward(*runtimeapi.PortForwardRequest) (*runtimeapi.PortForwardResponse, error)

	// Start the server.
	// addr is the address to serve on (address:port) stayUp indicates whether the server should
	// listen until Stop() is called, or automatically stop after all expected connections are
	// closed. Calling Get{Exec,Attach,PortForward} increments the expected connection count.
	// Function does not return until the server is stopped.
	Start(stayUp bool) error
	// Stop the server, and terminate any open connections.
	Stop() error
}

// The interface to execute the commands and provide the streams.
type Runtime interface {
	Exec(containerID string, cmd []string, in io.Reader, out, err io.WriteCloser, tty bool, resize <-chan term.Size) error
	Attach(containerID string, in io.Reader, out, err io.WriteCloser, tty bool, resize <-chan term.Size) error
	PortForward(podSandboxID string, port int32, stream io.ReadWriteCloser) error
}

// Config defines the options used for running the stream server.
type Config struct {
	// The host:port address the server will listen on.
	Addr string
	// The optional base URL for constructing streaming URLs. If empty, the baseURL will be
	// constructed from the serve address.
	BaseURL *url.URL

	// How long to leave idle connections open for.
	StreamIdleTimeout time.Duration
	// How long to wait for clients to create streams. Only used for SPDY streaming.
	StreamCreationTimeout time.Duration

	// The streaming protocols the server supports (understands and permits).  See
	// k8s.io/kubernetes/pkg/kubelet/server/remotecommand/constants.go for available protocols.
	// Only used for SPDY streaming.
	SupportedProtocols []string

	// The config for serving over TLS. If nil, TLS will not be used.
	TLSConfig *tls.Config
}

// DefaultConfig provides default values for server Config. The DefaultConfig is partial, so
// some fields like Addr must still be provided.
var DefaultConfig = Config{
	StreamIdleTimeout:     4 * time.Hour,
	StreamCreationTimeout: remotecommand.DefaultStreamCreationTimeout,
	SupportedProtocols:    remotecommand.SupportedStreamingProtocols,
}

// TODO(timstclair): Add auth(n/z) interface & handling.
func NewServer(config Config, runtime Runtime) (Server, error) {
	s := &server{
		config:  config,
		runtime: &criAdapter{runtime},
		cache:   newRequestCache(),
	}

	if s.config.BaseURL == nil {
		s.config.BaseURL = &url.URL{
			Scheme: "http",
			Host:   s.config.Addr,
		}
		if s.config.TLSConfig != nil {
			s.config.BaseURL.Scheme = "https"
		}
	}

	ws := &restful.WebService{}
	endpoints := []struct {
		path    string
		handler restful.RouteFunction
	}{
		{"/exec/{token}", s.serveExec},
		{"/attach/{token}", s.serveAttach},
		{"/portforward/{token}", s.servePortForward},
	}
	// If serving relative to a base path, set that here.
	pathPrefix := path.Dir(s.config.BaseURL.Path)
	for _, e := range endpoints {
		for _, method := range []string{"GET", "POST"} {
			ws.Route(ws.
				Method(method).
				Path(path.Join(pathPrefix, e.path)).
				To(e.handler))
		}
	}
	handler := restful.NewContainer()
	handler.Add(ws)
	s.handler = handler

	return s, nil
}

type server struct {
	config  Config
	runtime *criAdapter
	handler http.Handler
	cache   *requestCache
}

func (s *server) GetExec(req *runtimeapi.ExecRequest) (*runtimeapi.ExecResponse, error) {
	if req.ContainerId == "" {
		return nil, grpc.Errorf(codes.InvalidArgument, "missing required container_id")
	}
	token, err := s.cache.Insert(req)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.ExecResponse{
		Url: s.buildURL("exec", token),
	}, nil
}

func (s *server) GetAttach(req *runtimeapi.AttachRequest) (*runtimeapi.AttachResponse, error) {
	if req.ContainerId == "" {
		return nil, grpc.Errorf(codes.InvalidArgument, "missing required container_id")
	}
	token, err := s.cache.Insert(req)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.AttachResponse{
		Url: s.buildURL("attach", token),
	}, nil
}

func (s *server) GetPortForward(req *runtimeapi.PortForwardRequest) (*runtimeapi.PortForwardResponse, error) {
	if req.PodSandboxId == "" {
		return nil, grpc.Errorf(codes.InvalidArgument, "missing required pod_sandbox_id")
	}
	token, err := s.cache.Insert(req)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.PortForwardResponse{
		Url: s.buildURL("portforward", token),
	}, nil
}

func (s *server) Start(stayUp bool) error {
	if !stayUp {
		// TODO(timstclair): Implement this.
		return errors.New("stayUp=false is not yet implemented")
	}

	server := &http.Server{
		Addr:      s.config.Addr,
		Handler:   s.handler,
		TLSConfig: s.config.TLSConfig,
	}
	if s.config.TLSConfig != nil {
		return server.ListenAndServeTLS("", "") // Use certs from TLSConfig.
	} else {
		return server.ListenAndServe()
	}
}

func (s *server) Stop() error {
	// TODO(timstclair): Implement this.
	return errors.New("not yet implemented")
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *server) buildURL(method, token string) string {
	return s.config.BaseURL.ResolveReference(&url.URL{
		Path: path.Join(method, token),
	}).String()
}

func (s *server) serveExec(req *restful.Request, resp *restful.Response) {
	token := req.PathParameter("token")
	cachedRequest, ok := s.cache.Consume(token)
	if !ok {
		http.NotFound(resp.ResponseWriter, req.Request)
		return
	}
	exec, ok := cachedRequest.(*runtimeapi.ExecRequest)
	if !ok {
		http.NotFound(resp.ResponseWriter, req.Request)
		return
	}

	streamOpts := &remotecommand.Options{
		Stdin:  exec.Stdin,
		Stdout: true,
		Stderr: !exec.Tty,
		TTY:    exec.Tty,
	}

	remotecommand.ServeExec(
		resp.ResponseWriter,
		req.Request,
		s.runtime,
		"", // unused: podName
		"", // unusued: podUID
		exec.ContainerId,
		exec.Cmd,
		streamOpts,
		s.config.StreamIdleTimeout,
		s.config.StreamCreationTimeout,
		s.config.SupportedProtocols)
}

func (s *server) serveAttach(req *restful.Request, resp *restful.Response) {
	token := req.PathParameter("token")
	cachedRequest, ok := s.cache.Consume(token)
	if !ok {
		http.NotFound(resp.ResponseWriter, req.Request)
		return
	}
	attach, ok := cachedRequest.(*runtimeapi.AttachRequest)
	if !ok {
		http.NotFound(resp.ResponseWriter, req.Request)
		return
	}

	streamOpts := &remotecommand.Options{
		Stdin:  attach.Stdin,
		Stdout: true,
		Stderr: !attach.Tty,
		TTY:    attach.Tty,
	}
	remotecommand.ServeAttach(
		resp.ResponseWriter,
		req.Request,
		s.runtime,
		"", // unused: podName
		"", // unusued: podUID
		attach.ContainerId,
		streamOpts,
		s.config.StreamIdleTimeout,
		s.config.StreamCreationTimeout,
		s.config.SupportedProtocols)
}

func (s *server) servePortForward(req *restful.Request, resp *restful.Response) {
	token := req.PathParameter("token")
	cachedRequest, ok := s.cache.Consume(token)
	if !ok {
		http.NotFound(resp.ResponseWriter, req.Request)
		return
	}
	pf, ok := cachedRequest.(*runtimeapi.PortForwardRequest)
	if !ok {
		http.NotFound(resp.ResponseWriter, req.Request)
		return
	}

	portforward.ServePortForward(
		resp.ResponseWriter,
		req.Request,
		s.runtime,
		pf.PodSandboxId,
		"", // unused: podUID
		s.config.StreamIdleTimeout,
		s.config.StreamCreationTimeout)
}

// criAdapter wraps the Runtime functions to conform to the remotecommand interfaces.
// The adapter binds the container ID to the container name argument, and the pod sandbox ID to the pod name.
type criAdapter struct {
	Runtime
}

var _ remotecommand.Executor = &criAdapter{}
var _ remotecommand.Attacher = &criAdapter{}
var _ portforward.PortForwarder = &criAdapter{}

func (a *criAdapter) ExecInContainer(podName string, podUID types.UID, container string, cmd []string, in io.Reader, out, err io.WriteCloser, tty bool, resize <-chan term.Size, timeout time.Duration) error {
	return a.Exec(container, cmd, in, out, err, tty, resize)
}

func (a *criAdapter) AttachContainer(podName string, podUID types.UID, container string, in io.Reader, out, err io.WriteCloser, tty bool, resize <-chan term.Size) error {
	return a.Attach(container, in, out, err, tty, resize)
}

func (a *criAdapter) PortForward(podName string, podUID types.UID, port uint16, stream io.ReadWriteCloser) error {
	return a.Runtime.PortForward(podName, int32(port), stream)
}
