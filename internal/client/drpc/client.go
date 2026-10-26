// Package drpc is the web gateway's client of the chat server.
package drpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"

	"google.golang.org/protobuf/proto"
	"storj.io/drpc"
	"storj.io/drpc/drpcconn"
	"storj.io/drpc/drpcerr"
	"storj.io/drpc/drpcmanager"
	"storj.io/drpc/drpcmetadata"
	"storj.io/drpc/drpcpool"
	"storj.io/drpc/drpcwire"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/utils"
)

// The metadata the server reads; see the server's drpc package.
const (
	metaGatewayKey = "x-gateway-key"
	metaSession    = "x-session"
	metaClientIP   = "x-client-ip"
	metaUserAgent  = "x-user-agent"
)

type Client struct {
	cfg  *Config
	tls  *tls.Config
	pool *drpcpool.Pool[string, *drpcconn.Conn]
	conn drpc.Conn

	Auth     pb.DRPCAuthServiceClient
	Teams    pb.DRPCTeamServiceClient
	Channels pb.DRPCChannelServiceClient
	Messages pb.DRPCMessageServiceClient
	Files    pb.DRPCFileServiceClient
	Users    pb.DRPCUserServiceClient
	Bots     pb.DRPCBotServiceClient
	Admin    pb.DRPCAdminServiceClient
	Voice    pb.DRPCVoiceServiceClient
}

func New(cfg *Config) (*Client, error) {
	c := &Client{cfg: cfg}
	if cfg.TLS.Enabled {
		tc, err := tlsConfig(cfg)
		if err != nil {
			return nil, err
		}
		c.tls = tc
	}
	c.pool = drpcpool.New[string, *drpcconn.Conn](drpcpool.Options{
		Capacity:   cfg.PoolCapacity,
		Expiration: cfg.PoolExpiration,
	})
	conn := c.pool.Get(context.Background(), cfg.Address, c.dial)
	c.conn = conn
	c.Auth = pb.NewDRPCAuthServiceClient(conn)
	c.Teams = pb.NewDRPCTeamServiceClient(conn)
	c.Channels = pb.NewDRPCChannelServiceClient(conn)
	c.Messages = pb.NewDRPCMessageServiceClient(conn)
	c.Files = pb.NewDRPCFileServiceClient(conn)
	c.Users = pb.NewDRPCUserServiceClient(conn)
	c.Bots = pb.NewDRPCBotServiceClient(conn)
	c.Admin = pb.NewDRPCAdminServiceClient(conn)
	c.Voice = pb.NewDRPCVoiceServiceClient(conn)
	return c, nil
}

func tlsConfig(cfg *Config) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: cfg.TLS.ServerName}
	if cfg.TLS.CAFile != "" {
		pem, err := os.ReadFile(cfg.TLS.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("CA file holds no certificate")
		}
		tc.RootCAs = pool
	}
	if cfg.TLS.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load client certificate: %w", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	return tc, nil
}

func (c *Client) dial(ctx context.Context, addr string) (*drpcconn.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.DialTimeout)
	defer cancel()
	var (
		raw net.Conn
		err error
	)
	d := &net.Dialer{}
	if c.tls != nil {
		raw, err = (&tls.Dialer{NetDialer: d, Config: c.tls}).DialContext(ctx, "tcp", addr)
	} else {
		raw, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, err
	}
	return drpcconn.NewWithOptions(utils.WriteTimeoutConn(raw, c.cfg.WriteTimeout), drpcconn.Options{
		Manager: drpcmanager.Options{Reader: drpcwire.ReaderOptions{MaximumBufferSize: c.cfg.MaxMessageSize}},
	}), nil
}

// Invoke makes the unary call rpc, such as
// "/gochat.v1.ChannelService/GetChannel", with any request and response
// messages: the client API forwards calls it does not know the types of at
// compile time.
func (c *Client) Invoke(ctx context.Context, rpc string, in, out proto.Message) error {
	return c.conn.Invoke(ctx, rpc, protoEncoding{}, in, out)
}

type protoEncoding struct{}

func (protoEncoding) Marshal(msg drpc.Message) ([]byte, error) {
	m, ok := msg.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("not a protobuf message: %T", msg)
	}
	return proto.Marshal(m)
}

func (protoEncoding) Unmarshal(buf []byte, msg drpc.Message) error {
	m, ok := msg.(proto.Message)
	if !ok {
		return fmt.Errorf("not a protobuf message: %T", msg)
	}
	return proto.Unmarshal(buf, m)
}

// Dial opens a connection of its own, for a long-lived stream.
func (c *Client) Dial(ctx context.Context) (*drpcconn.Conn, error) {
	return c.dial(ctx, c.cfg.Address)
}

func (c *Client) Close() error {
	return c.pool.Close()
}

// Call carries who a call is made for.
type Call struct {
	Session   string
	ClientIP  string
	UserAgent string
}

// Context returns ctx bounded by the request timeout and carrying the
// gateway key and the call's identity.
func (c *Client) Context(ctx context.Context, call Call) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	return c.Stream(ctx, call), cancel
}

// Stream returns ctx carrying the metadata, without a deadline.
func (c *Client) Stream(ctx context.Context, call Call) context.Context {
	md := map[string]string{metaGatewayKey: c.cfg.GatewayKey}
	if call.Session != "" {
		md[metaSession] = call.Session
	}
	if call.ClientIP != "" {
		md[metaClientIP] = call.ClientIP
	}
	if call.UserAgent != "" {
		md[metaUserAgent] = call.UserAgent
	}
	return drpcmetadata.AddPairs(ctx, md)
}

// Err turns a server error back into an *errors.Error; transport failures
// become Unavailable.
func Err(err error) *ierrors.Error {
	if err == nil {
		return nil
	}
	if e, ok := errors.AsType[*ierrors.Error](err); ok {
		return e
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &ierrors.Error{Code: ierrors.CodeDeadlineExceeded, Message: "the request timed out", Cause: err}
	}
	if code := drpcerr.Code(err); code != 0 {
		return &ierrors.Error{Code: ierrors.Code(code), Message: err.Error()}
	}
	return &ierrors.Error{Code: ierrors.CodeUnavailable, Message: "the chat server is unavailable", Cause: err}
}
