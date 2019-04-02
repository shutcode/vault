package cluster

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/vault/helper/consts"
	"golang.org/x/net/http2"
)

var (
	// Making this a package var allows tests to modify
	HeartbeatInterval = 5 * time.Second
)

const (
	ListenerAcceptDeadline = 500 * time.Millisecond
)

// ClusterClient is used to lookup a client certificate.
type ClusterClient interface {
	ClientLookup(context.Context, *tls.CertificateRequestInfo) (*tls.Certificate, error)
}

// ClusterHandler exposes functions for looking up TLS configuration and handing
// off a connection for a cluster listener application.
type ClusterHandler interface {
	ServerLookup(context.Context, *tls.ClientHelloInfo) (*tls.Certificate, error)
	CALookup(context.Context) (*x509.Certificate, error)

	// Handoff is used to pass the connection lifetime off to
	// the handler
	Handoff(context.Context, *sync.WaitGroup, chan struct{}, *tls.Conn) error
	Stop() error
}

type ClusterHook interface {
	AddClient(alpn string, client ClusterClient)
	RemoveClient(alpn string)
	AddHandler(alpn string, handler ClusterHandler)
	StopHandler(alpn string)
	TLSConfig(ctx context.Context) (*tls.Config, error)
	Addr() net.Addr
}

func NewClusterListener(addrs []*net.TCPAddr, cipherSuites []uint16, logger log.Logger) *ClusterListener {
	// Create the HTTP/2 server that will be shared by both RPC and regular
	// duties. Doing it this way instead of listening via the server and gRPC
	// allows us to re-use the same port via ALPN. We can just tell the server
	// to serve a given conn and which handler to use.
	h2Server := &http2.Server{
		// Our forwarding connections heartbeat regularly so anything else we
		// want to go away/get cleaned up pretty rapidly
		IdleTimeout: 5 * HeartbeatInterval,
	}

	return &ClusterListener{
		handlers:   make(map[string]ClusterHandler),
		clients:    make(map[string]ClusterClient),
		shutdown:   new(uint32),
		shutdownWg: &sync.WaitGroup{},
		server:     h2Server,

		clusterListenerAddrs: addrs,
		clusterCipherSuites:  cipherSuites,
		logger:               logger.Named("cluster-listener"),
	}
}

// ClusterListener is the source of truth for cluster handlers and connection
// clients. It dynamically builds the cluster TLS information. It's also
// responsible for starting tcp listeners and accepting new cluster connections.
type ClusterListener struct {
	handlers   map[string]ClusterHandler
	clients    map[string]ClusterClient
	shutdown   *uint32
	shutdownWg *sync.WaitGroup
	server     *http2.Server

	clusterListenerAddrs []*net.TCPAddr
	clusterCipherSuites  []uint16
	logger               log.Logger
	l                    sync.RWMutex
}

// TODO: This probably isn't correct
func (cl *ClusterListener) Addr() net.Addr {
	return cl.clusterListenerAddrs[0]
}

// AddClient adds a new client for an ALPN name
func (cl *ClusterListener) AddClient(alpn string, client ClusterClient) {
	cl.l.Lock()
	cl.clients[alpn] = client
	cl.l.Unlock()
}

// RemoveClient removes the client for the specified ALPN name
func (cl *ClusterListener) RemoveClient(alpn string) {
	cl.l.Lock()
	delete(cl.clients, alpn)
	cl.l.Unlock()
}

// AddHandler registers a new cluster handler for the provided ALPN name.
func (cl *ClusterListener) AddHandler(alpn string, handler ClusterHandler) {
	cl.l.Lock()
	cl.handlers[alpn] = handler
	cl.l.Unlock()
}

// StopHandler stops the cluster handler for the provided ALPN name, it also
// calls stop on the handler.
func (cl *ClusterListener) StopHandler(alpn string) {
	cl.l.Lock()
	handler, ok := cl.handlers[alpn]
	delete(cl.handlers, alpn)
	cl.l.Unlock()
	if ok {
		handler.Stop()
	}
}

// Server returns the http2 server that the cluster listener is using
func (cl *ClusterListener) Server() *http2.Server {
	return cl.server
}

// TLSConfig returns a tls config object that uses dynamic lookups to correctly
// authenticate registered handlers/clients
func (cl *ClusterListener) TLSConfig(ctx context.Context) (*tls.Config, error) {
	serverLookup := func(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		cl.logger.Debug("performing server cert lookup")

		cl.l.RLock()
		defer cl.l.RUnlock()
		for _, v := range clientHello.SupportedProtos {
			if handler, ok := cl.handlers[v]; ok {
				return handler.ServerLookup(ctx, clientHello)
			}
		}

		cl.logger.Warn("no TLS certs found for ALPN", "ALPN", clientHello.SupportedProtos)
		return nil, errors.New("unsupported protocol")
	}

	clientLookup := func(requestInfo *tls.CertificateRequestInfo) (*tls.Certificate, error) {
		cl.logger.Debug("performing client cert lookup")

		cl.l.RLock()
		defer cl.l.RUnlock()
		for _, client := range cl.clients {
			cert, err := client.ClientLookup(ctx, requestInfo)
			if err == nil && cert != nil {
				return cert, nil
			}
		}

		cl.logger.Warn("no client information found")
		return nil, errors.New("no client cert found")
	}

	serverConfigLookup := func(clientHello *tls.ClientHelloInfo) (*tls.Config, error) {
		caPool := x509.NewCertPool()

		ret := &tls.Config{
			ClientAuth:           tls.RequireAndVerifyClientCert,
			GetCertificate:       serverLookup,
			GetClientCertificate: clientLookup,
			MinVersion:           tls.VersionTLS12,
			RootCAs:              caPool,
			ClientCAs:            caPool,
			NextProtos:           clientHello.SupportedProtos,
			CipherSuites:         cl.clusterCipherSuites,
		}

		cl.l.RLock()
		defer cl.l.RUnlock()
		for _, v := range clientHello.SupportedProtos {
			if handler, ok := cl.handlers[v]; ok {
				ca, err := handler.CALookup(ctx)
				if err != nil {
					return nil, err
				}

				caPool.AddCert(ca)
				return ret, nil
			}
		}

		cl.logger.Warn("no TLS config found for ALPN", "ALPN", clientHello.SupportedProtos)
		return nil, errors.New("unsupported protocol")
	}

	return &tls.Config{
		ClientAuth:           tls.RequireAndVerifyClientCert,
		GetCertificate:       serverLookup,
		GetClientCertificate: clientLookup,
		GetConfigForClient:   serverConfigLookup,
		MinVersion:           tls.VersionTLS12,
		CipherSuites:         cl.clusterCipherSuites,
	}, nil
}

// Run starts the tcp listeners and will accept connections until stop is
// called. This function blocks so should be called in a go routine.
func (cl *ClusterListener) Run(ctx context.Context) error {
	// Get our TLS config
	tlsConfig, err := cl.TLSConfig(ctx)
	if err != nil {
		cl.logger.Error("failed to get tls configuration when starting cluster listener", "error", err)
		return err
	}

	// The server supports all of the possible protos
	tlsConfig.NextProtos = []string{"h2", consts.RequestForwardingALPN, consts.PerfStandbyALPN, consts.PerformanceReplicationALPN, consts.DRReplicationALPN, consts.RaftStorageALPN}

	for _, addr := range cl.clusterListenerAddrs {
		cl.shutdownWg.Add(1)

		// Force a local resolution to avoid data races
		laddr := addr

		// Start our listening loop
		go func() {
			defer cl.shutdownWg.Done()

			// closeCh is used to shutdown the spawned goroutines once this
			// function returns
			closeCh := make(chan struct{})
			defer func() {
				close(closeCh)
			}()

			if cl.logger.IsInfo() {
				cl.logger.Info("starting listener", "listener_address", laddr)
			}

			// Create a TCP listener. We do this separately and specifically
			// with TCP so that we can set deadlines.
			tcpLn, err := net.ListenTCP("tcp", laddr)
			if err != nil {
				cl.logger.Error("error starting listener", "error", err)
				return
			}

			// Wrap the listener with TLS
			tlsLn := tls.NewListener(tcpLn, tlsConfig)
			defer tlsLn.Close()

			if cl.logger.IsInfo() {
				cl.logger.Info("serving cluster requests", "cluster_listen_address", tlsLn.Addr())
			}

			for {
				if atomic.LoadUint32(cl.shutdown) > 0 {
					return
				}

				// Set the deadline for the accept call. If it passes we'll get
				// an error, causing us to check the condition at the top
				// again.
				tcpLn.SetDeadline(time.Now().Add(ListenerAcceptDeadline))

				// Accept the connection
				conn, err := tlsLn.Accept()
				if err != nil {
					if err, ok := err.(net.Error); ok && !err.Timeout() {
						cl.logger.Debug("non-timeout error accepting on cluster port", "error", err)
					}
					if conn != nil {
						conn.Close()
					}
					continue
				}
				if conn == nil {
					continue
				}

				// Type assert to TLS connection and handshake to populate the
				// connection state
				tlsConn := conn.(*tls.Conn)

				// Set a deadline for the handshake. This will cause clients
				// that don't successfully auth to be kicked out quickly.
				// Cluster connections should be reliable so being marginally
				// aggressive here is fine.
				err = tlsConn.SetDeadline(time.Now().Add(30 * time.Second))
				if err != nil {
					if cl.logger.IsDebug() {
						cl.logger.Debug("error setting deadline for cluster connection", "error", err)
					}
					tlsConn.Close()
					continue
				}

				err = tlsConn.Handshake()
				if err != nil {
					if cl.logger.IsDebug() {
						cl.logger.Debug("error handshaking cluster connection", "error", err)
					}
					tlsConn.Close()
					continue
				}

				// Now, set it back to unlimited
				err = tlsConn.SetDeadline(time.Time{})
				if err != nil {
					if cl.logger.IsDebug() {
						cl.logger.Debug("error setting deadline for cluster connection", "error", err)
					}
					tlsConn.Close()
					continue
				}

				cl.l.RLock()
				handler, ok := cl.handlers[tlsConn.ConnectionState().NegotiatedProtocol]
				cl.l.RUnlock()
				if !ok {
					cl.logger.Debug("unknown negotiated protocol on cluster port")
					tlsConn.Close()
					continue
				}

				if err := handler.Handoff(ctx, cl.shutdownWg, closeCh, tlsConn); err != nil {
					cl.logger.Error("error handling cluster connection", "error", err)
					continue
				}
			}
		}()
	}

	return nil
}

// Stop stops the cluster listner
func (cl *ClusterListener) Stop() {
	// Set the shutdown flag. This will cause the listeners to shut down
	// within the deadline in clusterListenerAcceptDeadline
	atomic.StoreUint32(cl.shutdown, 1)
	cl.logger.Info("forwarding rpc listeners stopped")

	// Wait for them all to shut down
	cl.shutdownWg.Wait()
	cl.logger.Info("rpc listeners successfully shut down")
}