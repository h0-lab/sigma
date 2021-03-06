package node

import (
	"errors"
	"sync"

	"google.golang.org/grpc/metadata"

	"github.com/golang/glog"
	sigmaV1 "github.com/homebot/protobuf/pkg/api/sigma/v1"
	"github.com/homebot/sigma"
	"golang.org/x/net/context"
)

// NodeServer handles communication with function nodes
// TODO(ppacher): find a better name
type NodeServer interface {
	sigmaV1.NodeHandlerServer

	Prepare(string, string, sigma.FunctionSpec) (Conn, error)

	Remove(string) error
}

// nodeServer provides a `protobuf/api/sigma` node handler server
type nodeServer struct {
	rw    sync.RWMutex
	conns map[string]*nodeConn
}

// NewNodeServer returns a new handler service
func NewNodeServer() NodeServer {
	return &nodeServer{
		conns: make(map[string]*nodeConn),
	}
}

// Register implements sigma.NodeHandlerServer
func (h *nodeServer) Register(ctx context.Context, in *sigmaV1.NodeRegistrationRequest) (*sigmaV1.NodeRegistrationResponse, error) {
	urn, secret, err := getAuth(ctx)
	if err != nil {
		return nil, err
	}

	typ := in.GetNodeType()
	if typ == "" {
		return nil, errors.New("missing node type")
	}

	conn, err := h.getConnection(urn, secret)
	if err != nil {
		return nil, err
	}

	if conn.Registered() {
		return nil, errors.New("already registered")
	}

	if conn.isClosed() {
		return nil, errors.New("node marked for shutdown")
	}

	conn.setRegistered(true)

	return &sigmaV1.NodeRegistrationResponse{
		Urn:        in.GetUrn(),
		Content:    []byte(conn.spec.Content),
		Parameters: conn.spec.Parameteres.ToProto(),
	}, nil
}

// Subscribe implements sigmaV1.NodeHandlerServer
func (h *nodeServer) Subscribe(stream sigmaV1.NodeHandler_SubscribeServer) error {
	urn, secret, err := getAuth(stream.Context())
	if err != nil {
		return err
	}

	conn, err := h.getConnection(urn, secret)
	if err != nil {
		return err
	}

	if !conn.Registered() {
		return errors.New("connection not registered")
	}

	if conn.Connected() {
		return errors.New("connection already established")
	}

	channel := &nodeChannel{
		request:  make(chan *sigmaV1.DispatchEvent, 100),
		response: make(chan *sigmaV1.ExecutionResult, 100),
	}

	conn.setConnected(channel)
	defer conn.setConnected(nil)

	ch := make(chan struct{})

	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				glog.Error(urn, " connection failed ", err)
				close(ch)
				return
			}

			if conn.isClosed() {
				return
			}

			channel.response <- msg
		}
	}()

	for {
		select {
		case req, ok := <-channel.request:
			if !ok {
				return errors.New("request channel terminated")
			}

			if err := stream.Send(req); err != nil {
				glog.Error(urn, " connection failed ", err)
				return err
			}
		case <-ch:
			return errors.New("internal server error")
		case <-conn.closed:
			return errors.New("closed")
		}
	}
}

func (h *nodeServer) Prepare(urn string, secret string, spec sigma.FunctionSpec) (Conn, error) {
	node := newNodeConn(urn, secret, spec)

	return node, h.addPendingConn(node)
}

func (h *nodeServer) Remove(urn string) error {
	h.rw.Lock()
	conn, ok := h.conns[urn]
	if ok {
		delete(h.conns, urn)
	}
	h.rw.Unlock()

	if !ok {
		return errors.New("unknown connection")
	}

	return conn.Close()
}

func (h *nodeServer) addPendingConn(conn *nodeConn) error {
	h.rw.Lock()
	defer h.rw.Unlock()

	if e, ok := h.conns[conn.URN]; ok {
		if e.secret == conn.secret {
			return errors.New("URN collision with different secrets")
		}
		return errors.New("connection already added")
	}

	h.conns[conn.URN] = conn
	return nil
}

func (h *nodeServer) getConnection(urn string, secret string) (*nodeConn, error) {
	h.rw.RLock()
	defer h.rw.RUnlock()

	c, ok := h.conns[urn]
	if !ok {
		return nil, errors.New("unknown URN")
	}

	if c.secret != secret {
		return nil, errors.New("invalid secret")
	}

	return c, nil
}

func getAuth(ctx context.Context) (string, string, error) {
	md, ok := metadata.FromIncomingContext(ctx)

	urnList, ok := md["node-urn"]
	if len(urnList) != 1 || !ok {
		return "", "", errors.New("invalid URN header")
	}

	urn := urnList[0]

	secretList, ok := md["node-secret"]
	if len(secretList) != 1 || !ok {
		return "", "", errors.New("missing or invalid node-secret header")
	}

	secret := secretList[0]

	return urn, secret, nil
}
