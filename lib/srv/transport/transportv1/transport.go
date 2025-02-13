// Copyright 2023 Gravitational, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package transportv1

import (
	"context"
	"io"
	"net"

	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh/agent"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"

	proxyv1 "github.com/gravitational/teleport/api/gen/proto/go/teleport/proxy/v1"
	streamutils "github.com/gravitational/teleport/api/utils/grpc/stream"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/teleagent"
	"github.com/gravitational/teleport/lib/utils"
)

// Dialer is the interface that groups basic dialing methods.
type Dialer interface {
	DialSite(ctx context.Context, clusterName string) (net.Conn, error)
	DialHost(ctx context.Context, from net.Addr, host, port, clusterName string, accessChecker services.AccessChecker, agentGetter teleagent.Getter) (net.Conn, error)
}

// ServerConfig holds creation parameters for Service.
type ServerConfig struct {
	// FIPS indicates whether the cluster if configured
	// to run in FIPS mode
	FIPS bool
	// Logger provides a mechanism to log output
	Logger logrus.FieldLogger
	// Dialer is used to establish remote connections
	Dialer Dialer

	// agentGetterFn used by tests to serve the agent directly
	agentGetterFn func(rw io.ReadWriter) teleagent.Getter

	accessCheckerFn func(info credentials.AuthInfo) (services.AccessChecker, error)
}

// CheckAndSetDefaults ensures required parameters are set
// and applies default values for missing optional parameters.
func (c *ServerConfig) CheckAndSetDefaults() error {
	if c.Dialer == nil {
		return trace.BadParameter("parameter Dialer required")
	}

	if c.Logger == nil {
		c.Logger = utils.NewLogger().WithField(trace.Component, "transport")
	}

	if c.agentGetterFn == nil {
		c.agentGetterFn = func(rw io.ReadWriter) teleagent.Getter {
			return func() (teleagent.Agent, error) {
				return teleagent.NopCloser(agent.NewClient(rw)), nil
			}
		}
	}

	if c.accessCheckerFn == nil {
		c.accessCheckerFn = func(info credentials.AuthInfo) (services.AccessChecker, error) {
			identityInfo, ok := info.(auth.IdentityInfo)
			if !ok {
				return nil, trace.AccessDenied("client is not authenticated")
			}

			return identityInfo.AuthContext.Checker, nil
		}
	}

	return nil
}

// Service implements the teleport.proxy.v1.ProxyService RPC
// service.
type Service struct {
	proxyv1.UnimplementedProxyServiceServer

	cfg ServerConfig
}

// NewService constructs a new Service from the provided ServerConfig.
func NewService(cfg ServerConfig) (*Service, error) {
	if err := cfg.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}

	return &Service{cfg: cfg}, nil
}

// GetClusterDetails returns the cluster details as seen by this service to the client.
func (s *Service) GetClusterDetails(context.Context, *proxyv1.GetClusterDetailsRequest) (*proxyv1.GetClusterDetailsResponse, error) {
	return &proxyv1.GetClusterDetailsResponse{Details: &proxyv1.ClusterDetails{FipsEnabled: s.cfg.FIPS}}, nil
}

// ProxyCluster establishes a connection to a cluster and proxies the connection
// over the stream. The client must send the first request with the cluster name
// before the connection is established.
func (s *Service) ProxyCluster(stream proxyv1.ProxyService_ProxyClusterServer) error {
	req, err := stream.Recv()
	if err != nil {
		return trace.Wrap(err, "failed receiving first frame")
	}

	conn, err := s.cfg.Dialer.DialSite(stream.Context(), req.Cluster)
	if err != nil {
		return trace.Wrap(err, "failed dialing cluster %q", req.Cluster)
	}

	// A client may provide a frame with the first message. Since the message is
	// already consumed it won't be copying during the proxying below. In order for
	// the contents to be copied to the connection it needs to be manually written.
	if req.Frame != nil {
		if _, err := conn.Write(req.Frame.Payload); err != nil {
			return trace.Wrap(err, "failed writing payload from first frame")
		}
	}

	streamRW, err := streamutils.NewReadWriter(clusterStream{stream: stream})
	if err != nil {
		return trace.Wrap(err, "failed constructing streamer")
	}

	return trace.Wrap(utils.ProxyConn(stream.Context(), conn, streamRW))
}

// clusterStream implements the [streamutils.Source] interface
// for a [proxyv1.ProxyService_ProxyClusterServer].
type clusterStream struct {
	stream proxyv1.ProxyService_ProxyClusterServer
}

func (c clusterStream) Recv() ([]byte, error) {
	req, err := c.stream.Recv()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if req.Frame == nil {
		return nil, trace.BadParameter("received invalid frame")
	}

	return req.Frame.Payload, nil
}

func (c clusterStream) Send(frame []byte) error {
	return trace.Wrap(c.stream.Send(&proxyv1.ProxyClusterResponse{Frame: &proxyv1.Frame{Payload: frame}}))
}

// ProxySSH establishes a connection to a host and proxies both the SSH and SSH
// Agent protocol over the stream. The first request from the client must contain
// a valid dial target before the connection can be established.
func (s *Service) ProxySSH(stream proxyv1.ProxyService_ProxySSHServer) error {
	ctx := stream.Context()

	p, ok := peer.FromContext(ctx)
	if !ok {
		return trace.BadParameter("unable to find peer")
	}

	checker, err := s.cfg.accessCheckerFn(p.AuthInfo)
	if err != nil {
		return trace.Wrap(err)
	}

	// wait for the first request to arrive with the dial request
	req, err := stream.Recv()
	if err != nil {
		return trace.Wrap(err, "failed receiving first frame")
	}

	// validate the target
	if req.DialTarget == nil {
		return trace.BadParameter("first frame must contain a dial target")
	}

	host, port, err := net.SplitHostPort(req.DialTarget.HostPort)
	if err != nil {
		return trace.BadParameter("dial target contains an invalid hostport")
	}

	// create a stream for SSH Agent protocol
	agentStream := newSSHStream(stream, func(payload []byte) *proxyv1.ProxySSHResponse {
		return &proxyv1.ProxySSHResponse{Frame: &proxyv1.ProxySSHResponse_Agent{Agent: &proxyv1.Frame{Payload: payload}}}
	})

	// create a stream for SSH protocol
	sshStream := newSSHStream(stream, func(payload []byte) *proxyv1.ProxySSHResponse {
		return &proxyv1.ProxySSHResponse{Frame: &proxyv1.ProxySSHResponse_Ssh{Ssh: &proxyv1.Frame{Payload: payload}}}
	})

	// multiplex incoming frames to the appropriate protocol
	// handlers for the duration of the stream
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				agentStream.Close()
				sshStream.Close()
				if !utils.IsOKNetworkError(err) {
					s.cfg.Logger.Errorf("ssh stream terminated unexpectedly: %v", err)
				}

				return
			}

			// The writes to the channels are intentionally not selecting
			// on `ctx.Done()` to ensure that all data is flushed to the
			// clients.
			switch frame := req.Frame.(type) {
			case *proxyv1.ProxySSHRequest_Ssh:
				sshStream.incomingC <- frame.Ssh.Payload
			case *proxyv1.ProxySSHRequest_Agent:
				agentStream.incomingC <- frame.Agent.Payload
			default:
				s.cfg.Logger.Errorf("received unexpected ssh frame: %T", frame)
				continue
			}
		}
	}()

	// create a reader/writer for SSH Agent protocol
	agentStreamRW, err := streamutils.NewReadWriter(agentStream)
	if err != nil {
		return trace.Wrap(err, "failed constructing ssh agent streamer")
	}
	defer agentStreamRW.Close()

	conn, err := s.cfg.Dialer.DialHost(ctx, p.Addr, host, port, req.DialTarget.Cluster, checker, s.cfg.agentGetterFn(agentStreamRW))
	if err != nil {
		return trace.Wrap(err, "failed to dial target host")
	}

	// create a reader/writer for SSH protocol
	sshStreamRW, err := streamutils.NewReadWriter(sshStream)
	if err != nil {
		return trace.Wrap(err, "failed constructing ssh streamer")
	}

	// send back the cluster details to alert the other side that
	// the connection has been established
	if err := stream.Send(&proxyv1.ProxySSHResponse{
		Details: &proxyv1.ClusterDetails{FipsEnabled: s.cfg.FIPS},
	}); err != nil {
		return trace.Wrap(err, "failed sending cluster details ")
	}

	// copy data to/from the connection and ssh stream
	return trace.Wrap(utils.ProxyConn(ctx, conn, sshStreamRW))
}

// sshStream implements the [streamutils.Source] interface
// for a [proxyv1.ProxyService_ProxySSHServer]. Instead of
// reading directly from the stream reads are from an incoming
// channel that is fed by the multiplexer.
type sshStream struct {
	incomingC  chan []byte
	stream     proxyv1.ProxyService_ProxySSHServer
	done       chan struct{}
	responseFn func(payload []byte) *proxyv1.ProxySSHResponse
}

func newSSHStream(stream proxyv1.ProxyService_ProxySSHServer, responseFn func(payload []byte) *proxyv1.ProxySSHResponse) *sshStream {
	return &sshStream{
		incomingC:  make(chan []byte, 10),
		done:       make(chan struct{}),
		stream:     stream,
		responseFn: responseFn,
	}
}

func (s *sshStream) Close() error {
	select {
	case <-s.done:
	default:
		close(s.done)
	}

	return nil
}

// Recv consumes ssh frames from the gRPC stream.
// All data must be consumed by clients to prevent
// leaking the multiplexing goroutine in Service.ProxySSH.
func (s *sshStream) Recv() ([]byte, error) {
	select {
	case <-s.done:
		return nil, io.EOF
	case frame := <-s.incomingC:
		return frame, nil
	}
}

func (s *sshStream) Send(frame []byte) error {
	return trace.Wrap(s.stream.Send(s.responseFn(frame)))
}
