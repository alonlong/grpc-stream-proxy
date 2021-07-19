package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"grpc-stream-proxy/proto/apis/test"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/testing/protocmp"
)

const (
	clientMdKey        = "test-client-header"
	serverHeaderMdKey  = "test-server-header"
	serverTrailerMdKey = "test-server-trailer"
	rejectingMdKey     = "test-reject-rpc-if-in-context"
)

type ProxyHappySuite struct {
	suite.Suite

	ctx              context.Context
	cancel           context.CancelFunc
	server           *grpc.Server
	proxy            *grpc.Server
	connProxy2Server *grpc.ClientConn
	connClient2Proxy *grpc.ClientConn
	client           test.TestServiceClient
}

func (s *ProxyHappySuite) SetupSuite() {
	s.ctx, s.cancel = context.WithTimeout(context.Background(), time.Minute)

	listenerProxy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(s.T(), err, "must be able to allocate a port for listen proxy")
	listenerServer, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(s.T(), err, "must be able to allocate a port for listen server")

	// setup the proxy's director
	s.connProxy2Server, err = grpc.Dial(
		listenerServer.Addr().String(),
		grpc.WithInsecure(),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(NewCodec())),
	)
	require.NoError(s.T(), err, "must not error on deferred client dial")
	director := func(ctx context.Context, fullName string, peeker StreamPeeker) (*StreamParameters, error) {
		payload, err := peeker.Peek()
		if err != nil {
			return nil, err
		}

		return NewStreamParameters(Destination{
			Ctx:  IncomingToOutgoing(ctx),
			Conn: s.connProxy2Server,
			Msg:  payload,
		}, nil, nil, nil), nil
	}

	// setup backend server for test suite
	s.server = grpc.NewServer()
	test.RegisterTestServiceServer(s.server, &assertingService{t: s.T()})
	go func() {
		s.server.Serve(listenerServer)
	}()

	// setup grpc-proxy server for test suite
	s.proxy = grpc.NewServer(
		grpc.ForceServerCodec(NewCodec()),
		grpc.UnknownServiceHandler(TransparentHandler(director)),
	)
	go func() {
		s.proxy.Serve(listenerProxy)
	}()

	// setup client for test suite
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.connClient2Proxy, err = grpc.DialContext(ctx, listenerProxy.Addr().String(), grpc.WithInsecure())
	require.NoError(s.T(), err, "must not error on deferred client Dial")
	s.client = test.NewTestServiceClient(s.connClient2Proxy)
}

func (s *ProxyHappySuite) TearDownSuite() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.connClient2Proxy != nil {
		s.connClient2Proxy.Close()
	}
	if s.connProxy2Server != nil {
		s.connProxy2Server.Close()
	}
	if s.proxy != nil {
		s.proxy.Stop()
	}
	if s.server != nil {
		s.server.Stop()
	}
}

func TestProxyHappySuite(t *testing.T) {
	suite.Run(t, &ProxyHappySuite{})
}

// asserting service is implemented on the server side and serves as a handler for stuff
type assertingService struct {
	test.UnimplementedTestServiceServer
	t *testing.T
}

// IncomingToOutgoing creates an outgoing context out of an incoming context with the same storage metadata
func IncomingToOutgoing(ctx context.Context) context.Context {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx
	}

	return metadata.NewOutgoingContext(ctx, md)
}

func (s *assertingService) Ping(ctx context.Context, request *test.PingRequest) (*test.PingResponse, error) {
	// send user trailers and headers.
	require.NoError(s.t, grpc.SendHeader(ctx, metadata.Pairs(serverHeaderMdKey, "I like turtles.")))
	require.NoError(s.t, grpc.SetTrailer(ctx, metadata.Pairs(serverTrailerMdKey, "I like ending turtles.")))
	return &test.PingResponse{Value: request.Value, Counter: 42}, nil
}

func (s *assertingService) PingTwoStream(stream test.TestService_PingTwoStreamServer) error {
	require.NoError(s.t, stream.SendHeader(metadata.Pairs(serverHeaderMdKey, "I like turtles.")))
	counter := int32(0)

	for {
		ping, err := stream.Recv()
		if err == io.EOF {
			break
		} else if err != nil {
			require.NoError(s.t, err, "can't fail receiving stream")
			return err
		}

		pong := &test.PingResponse{Value: ping.Value, Counter: counter}
		if err := stream.Send(pong); err != nil {
			require.NoError(s.t, err, "can't' fail sending back a pong")
		}
		counter++
	}

	stream.SetTrailer(metadata.Pairs(serverTrailerMdKey, "I like ending turtles."))
	return nil
}

func (s *ProxyHappySuite) TestPing() {
	header, tailer := metadata.MD{}, metadata.MD{}

	out, err := s.client.Ping(s.ctx, &test.PingRequest{Value: "foo"}, grpc.Header(&header), grpc.Trailer(&tailer))
	require.NoError(s.T(), err, "ping should succeed without error")
	ProtoEqual(s.T(), &test.PingResponse{Value: "foo", Counter: 42}, out)
	s.T().Logf("header: %v, tail: %v", header, tailer)
}

func (s *ProxyHappySuite) TestPingTwoStream() {
	stream, err := s.client.PingTwoStream(s.ctx)
	require.NoError(s.T(), err, "ping-two-stream request should be successful.")

	for i := 0; i < 10; i++ {
		ping := &test.PingRequest{Value: fmt.Sprintf("foo: %d", i)}
		require.NoError(s.T(), stream.Send(ping), "send stream must not fail")
		pong, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if i == 0 {
			header, err := stream.Header()
			require.NoError(s.T(), err, "ping-two-stream headers should not error.")
			s.T().Logf("header: %v", header)
		}

		assert.EqualValues(s.T(), i, pong.Counter, "ping must succeed with the correct id")
	}
	require.NoError(s.T(), stream.CloseSend(), "no error on close send")
}

// ProtoEqual asserts that expected and actual protobuf messages are equal.
// It can accept not only proto.Message, but slices, maps, and structs too.
// This is required as comparing messages directly with `require.Equal` doesn't
// work.
func ProtoEqual(t testing.TB, expected, actual interface{}) {
	t.Helper()
	require.Empty(t, cmp.Diff(expected, actual, protocmp.Transform()))
}
