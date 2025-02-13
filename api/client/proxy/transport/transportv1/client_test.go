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
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"testing"
	"time"

	"github.com/gravitational/trace"
	"github.com/gravitational/trace/trail"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh/agent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	proxyv1 "github.com/gravitational/teleport/api/gen/proto/go/teleport/proxy/v1"
	streamutils "github.com/gravitational/teleport/api/utils/grpc/stream"
)

type fakeGetClusterDetailsServer func(context.Context, *proxyv1.GetClusterDetailsRequest) (*proxyv1.GetClusterDetailsResponse, error)

type fakeProxySSHServer func(proxyv1.ProxyService_ProxySSHServer) error

type fakeProxyClusterServer func(proxyv1.ProxyService_ProxyClusterServer) error

// fakeServer is a [proxyv1.ProxyServiceServer] implementation
// that allows tests to manipulate the server side of various RPCs.
type fakeServer struct {
	proxyv1.UnimplementedProxyServiceServer

	details fakeGetClusterDetailsServer
	ssh     fakeProxySSHServer
	cluster fakeProxyClusterServer
}

func (s fakeServer) GetClusterDetails(ctx context.Context, req *proxyv1.GetClusterDetailsRequest) (*proxyv1.GetClusterDetailsResponse, error) {
	return s.details(ctx, req)
}

func (s fakeServer) ProxySSH(stream proxyv1.ProxyService_ProxySSHServer) error {
	return s.ssh(stream)
}

func (s fakeServer) ProxyCluster(stream proxyv1.ProxyService_ProxyClusterServer) error {
	return s.cluster(stream)
}

// TestClient_ClusterDetails validates that a Client can retrieve
// [proxyv1.ClusterDetails] from a [proxyv1.ProxyServiceServer].
func TestClient_ClusterDetails(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		server    fakeServer
		assertion func(t *testing.T, response *proxyv1.ClusterDetails, err error)
	}{
		{
			name: "details retrieved successfully",
			server: fakeServer{
				details: func(ctx context.Context, request *proxyv1.GetClusterDetailsRequest) (*proxyv1.GetClusterDetailsResponse, error) {
					return &proxyv1.GetClusterDetailsResponse{Details: &proxyv1.ClusterDetails{FipsEnabled: true}}, nil
				},
			},
			assertion: func(t *testing.T, response *proxyv1.ClusterDetails, err error) {
				require.NoError(t, err)
				require.NotNil(t, response)
				require.True(t, response.FipsEnabled)
			},
		},
		{
			name: "error getting details",
			server: fakeServer{
				details: func(ctx context.Context, request *proxyv1.GetClusterDetailsRequest) (*proxyv1.GetClusterDetailsResponse, error) {
					return nil, trail.ToGRPC(trace.NotImplemented("not implemented"))
				},
			},
			assertion: func(t *testing.T, response *proxyv1.ClusterDetails, err error) {
				require.ErrorIs(t, err, trace.NotImplemented("not implemented"))
				require.Nil(t, response)
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			pack := newServer(t, test.server)

			resp, err := pack.Client.ClusterDetails(context.Background())
			test.assertion(t, resp, err)
		})
	}
}

// TestClient_DialCluster validates that a Client can establish a
// connection to a cluster and that said connection is proxied over
// the gRPC stream.
func TestClient_DialCluster(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		cluster   string
		server    fakeServer
		assertion func(t *testing.T, conn net.Conn, err error)
	}{
		{
			name: "stream terminated",
			server: fakeServer{
				cluster: func(server proxyv1.ProxyService_ProxyClusterServer) error {
					return trail.ToGRPC(trace.NotImplemented("not implemented"))
				},
			},
			assertion: func(t *testing.T, conn net.Conn, err error) {
				require.NoError(t, err)
				require.NotNil(t, conn)

				n, err := conn.Read(make([]byte, 10))
				require.True(t, trace.IsConnectionProblem(err))
				require.Zero(t, n)
			},
		},
		{
			name:    "invalid cluster name",
			cluster: "unknown",
			server: fakeServer{
				cluster: func(server proxyv1.ProxyService_ProxyClusterServer) error {
					req, err := server.Recv()
					if err != nil {
						return trace.Wrap(err)
					}

					if req.Cluster == "" {
						return trace.BadParameter("first message must contain a cluster")
					}

					return trace.NotFound("unknown cluster: %q", req.Cluster)
				},
			},
			assertion: func(t *testing.T, conn net.Conn, err error) {
				require.NoError(t, err)
				require.NotNil(t, conn)

				n, err := conn.Read(make([]byte, 10))
				require.True(t, trace.IsConnectionProblem(err))
				require.Zero(t, n)
			},
		},
		{
			name:    "connection successfully established",
			cluster: "test",
			server: fakeServer{
				cluster: func(server proxyv1.ProxyService_ProxyClusterServer) error {
					req, err := server.Recv()
					if err != nil {
						return trace.Wrap(err)
					}

					if req.Cluster == "" {
						return trace.BadParameter("first message must contain a cluster")
					}

					// get the payload written
					req, err = server.Recv()
					if err != nil {
						return trace.Wrap(err)
					}

					// echo the data back
					if err := server.Send(&proxyv1.ProxyClusterResponse{Frame: &proxyv1.Frame{Payload: req.Frame.Payload}}); err != nil {
						return trace.Wrap(err)
					}

					return nil
				},
			},
			assertion: func(t *testing.T, conn net.Conn, err error) {
				require.NoError(t, err)
				require.NotNil(t, conn)

				msg := []byte("hello")
				n, err := conn.Write(msg)
				require.NoError(t, err)
				require.Equal(t, len(msg), n)

				out := make([]byte, n)
				n, err = conn.Read(out)
				require.NoError(t, err)
				require.Equal(t, len(msg), n)
				require.Equal(t, msg, out)

				require.NoError(t, conn.Close())
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			pack := newServer(t, test.server)

			conn, err := pack.Client.DialCluster(context.Background(), test.cluster, nil)
			test.assertion(t, conn, err)
		})
	}
}

// TestClient_DialHost validates that a Client can establish a
// connection to a host and that both SSH and SSH Agent protocol is
// proxied over the gRPC stream.
func TestClient_DialHost(t *testing.T) {
	t.Parallel()

	keyring := newKeyring(t)

	tests := []struct {
		name      string
		cluster   string
		target    string
		server    fakeServer
		keyring   agent.ExtendedAgent
		assertion func(t *testing.T, conn net.Conn, details *proxyv1.ClusterDetails, err error)
	}{
		{
			name: "stream terminated",
			server: fakeServer{
				ssh: func(server proxyv1.ProxyService_ProxySSHServer) error {
					return trail.ToGRPC(trace.NotImplemented("not implemented"))
				},
			},
			assertion: func(t *testing.T, conn net.Conn, details *proxyv1.ClusterDetails, err error) {
				require.ErrorIs(t, err, trace.NotImplemented("not implemented"))
				require.Nil(t, conn)
				require.Nil(t, details)
			},
		},
		{
			name: "invalid dial target",
			server: fakeServer{
				ssh: func(server proxyv1.ProxyService_ProxySSHServer) error {
					req, err := server.Recv()
					if err != nil {
						return trail.ToGRPC(err)
					}

					if req == nil {
						return trail.ToGRPC(trace.BadParameter("first message must contain a dial target"))
					}

					return trail.ToGRPC(trace.BadParameter("invalid dial target"))
				},
			},
			assertion: func(t *testing.T, conn net.Conn, details *proxyv1.ClusterDetails, err error) {
				require.ErrorIs(t, err, trace.BadParameter("invalid dial target"))
				require.Nil(t, conn)
				require.Nil(t, details)
			},
		},
		{
			name:    "connection terminated when receive returns an error",
			cluster: "test",
			server: fakeServer{
				ssh: func(server proxyv1.ProxyService_ProxySSHServer) error {
					req, err := server.Recv()
					if err != nil {
						return trail.ToGRPC(trace.Wrap(err))
					}

					if req.DialTarget == nil {
						return trail.ToGRPC(trace.BadParameter("first message must contain a cluster"))
					}

					if err := server.Send(&proxyv1.ProxySSHResponse{Details: &proxyv1.ClusterDetails{FipsEnabled: true}}); err != nil {
						return trail.ToGRPC(err)
					}

					req, err = server.Recv()
					if err != nil {
						return trail.ToGRPC(trace.Wrap(err))
					}

					switch f := req.Frame.(type) {
					case *proxyv1.ProxySSHRequest_Ssh:
						if err := server.Send(&proxyv1.ProxySSHResponse{
							Details: nil,
							Frame:   &proxyv1.ProxySSHResponse_Ssh{Ssh: &proxyv1.Frame{Payload: f.Ssh.Payload}},
						}); err != nil {
							return trail.ToGRPC(trace.Wrap(err))
						}
					case *proxyv1.ProxySSHRequest_Agent:
					}

					if err := server.Send(&proxyv1.ProxySSHResponse{
						Details: nil,
						Frame:   &proxyv1.ProxySSHResponse_Ssh{Ssh: &proxyv1.Frame{Payload: bytes.Repeat([]byte{0}, 1001)}},
					}); err != nil {
						return trail.ToGRPC(trace.Wrap(err))
					}

					return nil
				},
			},
			assertion: func(t *testing.T, conn net.Conn, details *proxyv1.ClusterDetails, err error) {
				require.NoError(t, err)
				require.NotNil(t, conn)

				msg := []byte("hello")
				n, err := conn.Write(msg)
				require.NoError(t, err)
				require.Equal(t, len(msg), n)

				out := make([]byte, n)
				n, err = conn.Read(out)
				require.NoError(t, err)
				require.Equal(t, len(msg), n)
				require.Equal(t, msg, out)

				n, err = conn.Read(out)
				require.True(t, trace.IsConnectionProblem(err))
				require.Zero(t, n)

				require.NoError(t, conn.Close())
			},
		},
		{
			name:    "connection successfully established without agent forwarding",
			cluster: "test",
			server: fakeServer{
				ssh: func(server proxyv1.ProxyService_ProxySSHServer) error {
					req, err := server.Recv()
					if err != nil {
						return trail.ToGRPC(trace.Wrap(err))
					}

					if req.DialTarget == nil {
						return trail.ToGRPC(trace.BadParameter("first message must contain a cluster"))
					}

					if err := server.Send(&proxyv1.ProxySSHResponse{Details: &proxyv1.ClusterDetails{FipsEnabled: true}}); err != nil {
						return trail.ToGRPC(err)
					}

					req, err = server.Recv()
					if err != nil {
						return trail.ToGRPC(trace.Wrap(err))
					}

					switch f := req.Frame.(type) {
					case *proxyv1.ProxySSHRequest_Ssh:
						if err := server.Send(&proxyv1.ProxySSHResponse{
							Details: nil,
							Frame:   &proxyv1.ProxySSHResponse_Ssh{Ssh: &proxyv1.Frame{Payload: f.Ssh.Payload}},
						}); err != nil {
							return trail.ToGRPC(trace.Wrap(err))
						}
					case *proxyv1.ProxySSHRequest_Agent:
					}

					return nil
				},
			},
			assertion: func(t *testing.T, conn net.Conn, details *proxyv1.ClusterDetails, err error) {
				require.NoError(t, err)
				require.NotNil(t, conn)

				msg := []byte("hello")
				n, err := conn.Write(msg)
				require.NoError(t, err)
				require.Equal(t, len(msg), n)

				out := make([]byte, n)
				n, err = conn.Read(out)
				require.NoError(t, err)
				require.Equal(t, len(msg), n)
				require.Equal(t, msg, out)

				n, err = conn.Read(out)
				require.ErrorIs(t, err, io.EOF)
				require.Zero(t, n)

				require.NoError(t, conn.Close())
			},
		},
		{
			name:    "connection successfully established with agent forwarding",
			cluster: "test",
			keyring: keyring,
			server: fakeServer{
				ssh: func(server proxyv1.ProxyService_ProxySSHServer) error {
					req, err := server.Recv()
					if err != nil {
						return trail.ToGRPC(trace.Wrap(err))
					}

					if req.DialTarget == nil {
						return trail.ToGRPC(trace.BadParameter("first message must contain a cluster"))
					}

					// send the initial cluster details
					if err := server.Send(&proxyv1.ProxySSHResponse{Details: &proxyv1.ClusterDetails{FipsEnabled: true}}); err != nil {
						return trail.ToGRPC(trace.Wrap(err))
					}

					// wait for the first ssh frame
					req, err = server.Recv()
					if err != nil {
						return trail.ToGRPC(trace.Wrap(err))
					}

					// echo the data back on an ssh frame
					switch f := req.Frame.(type) {
					case *proxyv1.ProxySSHRequest_Ssh:
						if err := server.Send(&proxyv1.ProxySSHResponse{
							Details: nil,
							Frame:   &proxyv1.ProxySSHResponse_Ssh{Ssh: &proxyv1.Frame{Payload: f.Ssh.Payload}},
						}); err != nil {
							return trail.ToGRPC(trace.Wrap(err))
						}
					case *proxyv1.ProxySSHRequest_Agent:
						return trail.ToGRPC(trace.BadParameter("test expects first frame to be ssh. got an agent frame"))
					}

					// create an agent stream and writer to communicate agent protocol on
					agentStream := newServerStream(server, func(payload []byte) *proxyv1.ProxySSHResponse {
						return &proxyv1.ProxySSHResponse{Frame: &proxyv1.ProxySSHResponse_Agent{Agent: &proxyv1.Frame{Payload: payload}}}
					})
					agentStreamRW, err := streamutils.NewReadWriter(agentStream)
					if err != nil {
						return trail.ToGRPC(trace.Wrap(err, "failed constructing ssh agent streamer"))
					}

					// read in agent frames
					go func() {
						for {
							req, err := server.Recv()
							if err != nil {
								if errors.Is(err, io.EOF) {
									return
								}

								return
							}

							switch frame := req.Frame.(type) {
							case *proxyv1.ProxySSHRequest_Agent:
								agentStream.incomingC <- frame.Agent.Payload
							default:
								continue
							}
						}
					}()

					// create an agent that will communicate over the agent frames
					// and list the keys from the client
					clt := agent.NewClient(agentStreamRW)
					keys, err := clt.List()
					if err != nil {
						return trail.ToGRPC(trace.Wrap(err))
					}

					if len(keys) != 1 {
						return trail.ToGRPC(fmt.Errorf("expected to receive 1 key. got %v", len(keys)))
					}

					// send the key blob back via an ssh frame to alert the
					// test that we finished listing keys
					if err := server.Send(&proxyv1.ProxySSHResponse{
						Details: nil,
						Frame:   &proxyv1.ProxySSHResponse_Ssh{Ssh: &proxyv1.Frame{Payload: keys[0].Blob}},
					}); err != nil {
						return trail.ToGRPC(trace.Wrap(err))
					}

					return nil
				},
			},
			assertion: func(t *testing.T, conn net.Conn, details *proxyv1.ClusterDetails, err error) {
				require.NoError(t, err)
				require.NotNil(t, conn)
				require.True(t, details.FipsEnabled)

				// write data via ssh frames
				msg := []byte("hello")
				n, err := conn.Write(msg)
				require.NoError(t, err)
				require.Equal(t, len(msg), n)

				// read data via ssh frames
				out := make([]byte, n)
				n, err = conn.Read(out)
				require.NoError(t, err)
				require.Equal(t, len(msg), n)
				require.Equal(t, msg, out)

				// get the keys from our local keyring
				keys, err := keyring.List()
				require.NoError(t, err)
				require.Len(t, keys, 1)

				// the server performs a remote list of keys
				// via ssh frames. to prevent the test from terminating
				// before it can complete it will write the blob of the
				// listed key back on the ssh frame. verify that the key
				// it received matches the one from out local keyring.
				out = make([]byte, len(keys[0].Blob))
				n, err = conn.Read(out)
				require.NoError(t, err)
				require.Equal(t, len(keys[0].Blob), n)
				require.Equal(t, keys[0].Blob, out)

				// close the stream
				require.NoError(t, conn.Close())
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			pack := newServer(t, test.server)

			conn, details, err := pack.Client.DialHost(context.Background(), test.cluster, test.target, nil, test.keyring)
			test.assertion(t, conn, details, err)
		})
	}
}

// testPack used to test a [Client].
type testPack struct {
	Client *Client
	Server proxyv1.ProxyServiceServer
}

// newServer creates a [grpc.Server] and registers the
// provided [proxyv1.ProxyServiceServer] with it opens
// an authenticated Client.
func newServer(t *testing.T, srv proxyv1.ProxyServiceServer) testPack {
	// gRPC testPack.
	const bufSize = 100 // arbitrary
	lis := bufconn.Listen(bufSize)
	t.Cleanup(func() {
		require.NoError(t, lis.Close())
	})

	s := grpc.NewServer()
	t.Cleanup(func() {
		s.GracefulStop()
		s.Stop()
	})

	// Register service.
	proxyv1.RegisterProxyServiceServer(s, srv)

	// Start.
	go func() {
		if err := s.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			panic(fmt.Sprintf("Serve returned err = %v", err))
		}
	}()

	// gRPC client.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cc, err := grpc.DialContext(ctx, "unused",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(1000)),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, cc.Close())
	})

	return testPack{
		Client: &Client{clt: proxyv1.NewProxyServiceClient(cc)},
		Server: srv,
	}
}

// newKeyring returns an [agent.ExtendedAgent] that has
// one key populated in it.
func newKeyring(t *testing.T) agent.ExtendedAgent {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	keyring := agent.NewKeyring()

	require.NoError(t, keyring.Add(agent.AddedKey{
		PrivateKey:   private,
		Comment:      "test",
		LifetimeSecs: math.MaxUint32,
	}))

	extendedKeyring, ok := keyring.(agent.ExtendedAgent)
	require.True(t, ok)

	return extendedKeyring
}

// serverStream implements the [streamutils.Source] interface
// for a [proxyv1.ProxyService_ProxySSHServer]. Instead of
// reading directly from the stream reads are from an incoming
// channel that is fed by the multiplexer.
type serverStream struct {
	incomingC  chan []byte
	stream     proxyv1.ProxyService_ProxySSHServer
	responseFn func(payload []byte) *proxyv1.ProxySSHResponse
}

func newServerStream(stream proxyv1.ProxyService_ProxySSHServer, responseFn func(payload []byte) *proxyv1.ProxySSHResponse) *serverStream {
	return &serverStream{
		incomingC:  make(chan []byte, 10),
		stream:     stream,
		responseFn: responseFn,
	}
}

func (s *serverStream) Recv() ([]byte, error) {
	select {
	case <-s.stream.Context().Done():
		return nil, io.EOF
	case frame := <-s.incomingC:
		return frame, nil
	}
}

func (s *serverStream) Send(frame []byte) error {
	return trace.Wrap(s.stream.Send(s.responseFn(frame)))
}
