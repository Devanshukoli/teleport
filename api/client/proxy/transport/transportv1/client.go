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
	"net"
	"sync"

	"github.com/gravitational/trace"
	"github.com/gravitational/trace/trail"
	"golang.org/x/crypto/ssh/agent"
	"google.golang.org/grpc/peer"

	proxyv1 "github.com/gravitational/teleport/api/gen/proto/go/teleport/proxy/v1"
	streamutils "github.com/gravitational/teleport/api/utils/grpc/stream"
)

// Client is a wrapper around a [proxyv1.ProxyServiceClient] that
// hides the implementation details of establishing connections
// over gRPC streams.
type Client struct {
	clt proxyv1.ProxyServiceClient
}

// NewClient constructs a Client that operates on the provided
// [proxyv1.ProxyServiceClient]. An error is returned if the client
// provided is nil.
func NewClient(client proxyv1.ProxyServiceClient) (*Client, error) {
	if client == nil {
		return nil, trace.BadParameter("parameter client required")
	}

	return &Client{clt: client}, nil
}

// ClusterDetails retrieves the cluster details as observed by the Teleport Proxy
// that the Client is connected to.
func (c *Client) ClusterDetails(ctx context.Context) (*proxyv1.ClusterDetails, error) {
	resp, err := c.clt.GetClusterDetails(ctx, &proxyv1.GetClusterDetailsRequest{})
	if err != nil {
		return nil, trail.FromGRPC(err)
	}

	return resp.Details, nil
}

// DialCluster establishes a connection to the provided cluster. The provided
// src address will be used as the LocalAddr of the returned [net.Conn].
func (c *Client) DialCluster(ctx context.Context, cluster string, src net.Addr) (net.Conn, error) {
	stream, err := c.clt.ProxyCluster(ctx)
	if err != nil {
		return nil, trail.FromGRPC(err, "unable to establish proxy stream")
	}

	if err := stream.Send(&proxyv1.ProxyClusterRequest{Cluster: cluster}); err != nil {
		return nil, trail.FromGRPC(err, "failed to send cluster request")
	}

	streamRW, err := streamutils.NewReadWriter(clusterStream{stream: stream})
	if err != nil {
		return nil, trace.Wrap(err, "unable to create stream reader")
	}

	p, ok := peer.FromContext(stream.Context())
	if !ok {
		return nil, trace.BadParameter("unable to retrieve peer information")
	}

	return streamutils.NewConn(streamRW, src, p.Addr), nil
}

// clusterStream implements the [streamutils.Source] interface
// for a [proxyv1.ProxyService_ProxyClusterClient].
type clusterStream struct {
	stream proxyv1.ProxyService_ProxyClusterClient
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
	return trace.Wrap(c.stream.Send(&proxyv1.ProxyClusterRequest{Frame: &proxyv1.Frame{Payload: frame}}))
}

// DialHost establishes a connection to the instance in the provided cluster that matches
// the hostport. If a keyring is provided then it will be forwarded to the remote instance.
// The src address will be used as the LocalAddr of the returned [net.Conn].
func (c *Client) DialHost(ctx context.Context, hostport, cluster string, src net.Addr, keyring agent.ExtendedAgent) (net.Conn, *proxyv1.ClusterDetails, error) {
	stream, err := c.clt.ProxySSH(ctx)
	if err != nil {
		return nil, nil, trail.FromGRPC(err, "unable to establish proxy stream")
	}

	if err := stream.Send(&proxyv1.ProxySSHRequest{DialTarget: &proxyv1.TargetHost{
		HostPort: hostport,
		Cluster:  cluster,
	}}); err != nil {
		return nil, nil, trail.FromGRPC(err, "failed to send dial target request")
	}

	resp, err := stream.Recv()
	if err != nil {
		return nil, nil, trail.FromGRPC(err, "failed to receive cluster details response")
	}

	// create a stream for agent protocol
	agentStream := newClientStream(stream, func(payload []byte) *proxyv1.ProxySSHRequest {
		return &proxyv1.ProxySSHRequest{Frame: &proxyv1.ProxySSHRequest_Agent{Agent: &proxyv1.Frame{Payload: payload}}}
	})
	// create a reader writer for agent protocol
	agentRW, err := streamutils.NewReadWriter(agentStream)
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}

	// create a stream for ssh protocol
	sshStream := newClientStream(stream, func(payload []byte) *proxyv1.ProxySSHRequest {
		return &proxyv1.ProxySSHRequest{Frame: &proxyv1.ProxySSHRequest_Ssh{Ssh: &proxyv1.Frame{Payload: payload}}}
	})

	// create a reader writer for SSH protocol
	sshRW, err := streamutils.NewReadWriter(sshStream)
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}

	sshConn := streamutils.NewConn(sshRW, src, addr(hostport))

	// multiplex the frames to the correct handlers
	var serveOnce sync.Once
	go func() {
		defer func() {
			// closing the agentRW will terminate the agent.ServeAgent goroutine
			agentRW.Close()
			// closing the connection will close sshRW and end the connection for
			// the user
			sshConn.Close()
		}()

		for {
			req, err := stream.Recv()
			if err != nil {
				// sending the error to the ssh stream and not the
				// agent stream so that it may get seen by the user.
				sshStream.errorC <- err
				return
			}

			switch frame := req.Frame.(type) {
			case *proxyv1.ProxySSHResponse_Ssh:
				sshStream.incomingC <- frame.Ssh.Payload
			case *proxyv1.ProxySSHResponse_Agent:
				if keyring == nil {
					continue
				}

				// start serving the agent only if the upstream
				// service attempts to interact with it
				serveOnce.Do(func() {
					go agent.ServeAgent(keyring, agentRW)
				})

				agentStream.incomingC <- frame.Agent.Payload
			default:
				continue
			}
		}
	}()

	return sshConn, resp.Details, nil
}

type addr string

func (a addr) Network() string {
	return "tcp"
}

func (a addr) String() string {
	return string(a)
}

// sshStream implements the [streamutils.Source] interface
// for a [proxyv1.ProxyService_ProxySSHClient]. Instead of
// reading directly from the stream reads are from an incoming
// channel that is fed by the multiplexer.
type sshStream struct {
	incomingC chan []byte
	errorC    chan error
	stream    proxyv1.ProxyService_ProxySSHClient
	requestFn func(payload []byte) *proxyv1.ProxySSHRequest
}

func newClientStream(stream proxyv1.ProxyService_ProxySSHClient, requestFn func(payload []byte) *proxyv1.ProxySSHRequest) *sshStream {
	return &sshStream{
		incomingC: make(chan []byte, 10),
		errorC:    make(chan error, 1),
		stream:    stream,
		requestFn: requestFn,
	}
}

func (s *sshStream) Recv() ([]byte, error) {
	select {
	case err := <-s.errorC:
		return nil, trace.Wrap(err)
	case frame := <-s.incomingC:
		return frame, nil
	}
}

func (s *sshStream) Send(frame []byte) error {
	return trace.Wrap(s.stream.Send(s.requestFn(frame)))
}
