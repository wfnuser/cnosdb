package meta

import (
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"sync"
	"time"

	"github.com/cnosdb/cnosdb"
	internal "github.com/cnosdb/cnosdb/meta/internal"

	"github.com/gogo/protobuf/proto"
	"github.com/hashicorp/raft"
	"go.uber.org/zap"
)

// Retention policy settings.
const (
	autoCreateRetentionPolicyName   = "autogen"
	autoCreateRetentionPolicyPeriod = 0

	// maxAutoCreatedRetentionPolicyReplicaN is the maximum replication factor that will
	// be set for auto-created retention policies.
	maxAutoCreatedRetentionPolicyReplicaN = 3
)

// Raft configuration.
const (
	raftListenerStartupTimeout = time.Second
)

type store struct {
	mu      sync.RWMutex
	closing chan struct{}

	config      *Config
	data        *Data
	raftState   *raftState
	dataChanged chan struct{}
	path        string
	opened      bool
	logger      *zap.Logger

	raftAddr string
	httpAddr string

	node *cnosdb.Node
}

// newStore will create a new metastore with the passed in config
func newStore(c *Config, httpAddr, raftAddr string) *store {
	s := store{
		data: &Data{
			Index: 1,
		},
		closing:     make(chan struct{}),
		dataChanged: make(chan struct{}),
		path:        c.Dir,
		config:      c,
		logger:      zap.NewNop(),
		httpAddr:    httpAddr,
		raftAddr:    raftAddr,
	}

	return &s
}

func (s *store) withLogger(log *zap.Logger) {
	if s.config.HTTPD.LoggingEnabled {
		s.logger = log.With(zap.String("service", "meta-store"))
	} else {
		s.logger = zap.NewNop()
	}
}

// open opens and initializes the raft store.
func (s *store) open(raftln net.Listener) error {
	s.logger.Info("Open meta store", zap.String("data-dir", s.path))

	if err := s.setOpen(); err != nil {
		return err
	}

	// Create the root directory if it doesn't already exist.
	if err := os.MkdirAll(s.path, 0777); err != nil {
		return fmt.Errorf("mkdir all: %s", err)
	}

	// Open the raft store.
	if err := s.openRaft(raftln); err != nil {
		return fmt.Errorf("raft: %s", err)
	}

	// Wait for a leader to be elected so we know the raft log is loaded
	// and up to date
	if err := s.waitForLeader(0); err != nil {
		return err
	}

	// Make sure this server is in the list of metanodes
	peers, err := s.raftState.peers()
	if err != nil {
		return err
	}
	if len(peers) <= 1 {
		// we have to loop here because if the hostname has changed
		// raft will take a little bit to normalize so that this host
		// will be marked as the leader
		for {
			err := s.callSetMetaNode(s.httpAddr, s.raftAddr)
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	return nil
}

func (s *store) setOpen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Check if store has already been opened.
	if s.opened {
		return ErrStoreOpen
	}
	s.opened = true
	return nil
}

// peers returns the raft peers known to this store
func (s *store) peers() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.raftState == nil {
		return []string{s.raftAddr}
	}
	peers, err := s.raftState.peers()
	if err != nil {
		return []string{s.raftAddr}
	}
	return peers
}

func (s *store) filterAddr(addrs []string, filter string) ([]string, error) {
	host, port, err := net.SplitHostPort(filter)
	if err != nil {
		return nil, err
	}

	ip, err := net.ResolveIPAddr("ip", host)
	if err != nil {
		return nil, err
	}

	var joinPeers []string
	for _, addr := range addrs {
		joinHost, joinPort, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}

		joinIp, err := net.ResolveIPAddr("ip", joinHost)
		if err != nil {
			return nil, err
		}

		// Don't allow joining ourselves
		if ip.String() == joinIp.String() && port == joinPort {
			continue
		}
		joinPeers = append(joinPeers, addr)
	}
	return joinPeers, nil
}

func (s *store) openRaft(raftln net.Listener) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := newRaftState(s.config.HTTPD, s.raftAddr)
	rs.withLogger(s.logger)
	rs.path = s.path

	if err := rs.open(s, raftln); err != nil {
		return err
	}
	s.raftState = rs

	return nil
}

func (s *store) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	select {
	case <-s.closing:
		// already closed
		return nil
	default:
		close(s.closing)
		return s.raftState.close()
	}
}

func (s *store) snapshot() (*Data, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Clone(), nil
}

// afterIndex returns a channel that will be closed to signal
// the caller when an updated snapshot is available.
func (s *store) afterIndex(index uint64) <-chan struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if index < s.data.Index {
		// Client needs update so return a closed channel.
		ch := make(chan struct{})
		close(ch)
		return ch
	}

	return s.dataChanged
}

// WaitForLeader sleeps until a leader is found or a timeout occurs.
// timeout == 0 means to wait forever.
func (s *store) waitForLeader(timeout time.Duration) error {
	// Begin timeout timer.
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	// Continually check for leader until timeout.
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.closing:
			return errors.New("closing")
		case <-timer.C:
			if timeout != 0 {
				return errors.New("timeout")
			}
		case <-ticker.C:
			if s.leader() != "" {
				return nil
			}
		}
	}
}

// isLeader returns true if the store is currently the leader.
func (s *store) isLeader() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.raftState == nil {
		return false
	}
	return s.raftState.raft.State() == raft.Leader
}

// leader returns what the store thinks is the current leader. An empty
// string indicates no leader exists.
func (s *store) leader() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.raftState == nil || s.raftState.raft == nil {
		return ""
	}
	return string(s.raftState.raft.Leader())
}

// leaderHTTP returns the HTTP API connection info for the metanode
// that is the raft leader
func (s *store) leaderHTTP() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.raftState == nil {
		return ""
	}
	l := s.raftState.raft.Leader()

	for _, n := range s.data.MetaNodes {
		if n.TCPHost == string(l) {
			return n.Host
		}
	}

	return ""
}

// metaServersHTTP will return the HTTP bind addresses of all meta-servers in the cluster
func (s *store) metaServersHTTP() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var a []string
	for _, n := range s.data.MetaNodes {
		a = append(a, n.Host)
	}
	return a
}

// otherMetaServersHTTP will return the HTTP bind addresses of the other
// meta-servers in the cluster
func (s *store) otherMetaServersHTTP() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var a []string
	for _, n := range s.data.MetaNodes {
		if n.TCPHost != s.raftAddr {
			a = append(a, n.Host)
		}
	}
	return a
}

// index returns the current store index.
func (s *store) index() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Index
}

// getNode returns the current store node.
func (s *store) getNode() *NodeInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return &NodeInfo{
		ID:      s.node.ID,
		Host:    s.config.HTTPD.HTTPBindAddress,
		TCPHost: s.raftState.addr,
	}
}

// apply applies a command to raft.
func (s *store) apply(b []byte) error {
	if s.raftState == nil {
		return fmt.Errorf("store not open")
	}
	return s.raftState.apply(b)
}

// joinCluster
func (s *store) joinCluster(peers []string) (*NodeInfo, error) {
	if len(peers) > 0 {
		c := NewRemoteClient()
		c.SetMetaServers(peers)
		c.SetTLS(s.config.HTTPD.HTTPSEnabled)
		if err := c.Open(); err != nil {
			return nil, err
		}
		defer c.Close()

		n, err := c.joinMetaServer(s.httpAddr, s.raftAddr)
		if err != nil {
			return nil, err
		}
		s.node.ID = n.ID
		if err := s.node.Save("meta.json"); err != nil {
			return nil, err
		}
		return n, nil
	}

	return nil, fmt.Errorf("Empty peers!")
}

// addMetaNode adds a new server to the metaservice and raft
func (s *store) addMetaNode(n *NodeInfo) (*NodeInfo, error) {
	s.mu.RLock()
	if s.raftState == nil {
		s.mu.RUnlock()
		return nil, fmt.Errorf("store not open")
	}
	if err := s.raftState.addVoter(n.TCPHost); err != nil {
		s.mu.RUnlock()
		return nil, err
	}
	s.mu.RUnlock()

	if err := s.callCreateMetaNode(n.Host, n.TCPHost); err != nil {
		return nil, err
	}
	// TODO magz: this is a waste
	if err := s.callSetData(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, node := range s.data.MetaNodes {
		if node.TCPHost == n.TCPHost && node.Host == n.Host {
			return &node, nil
		}
	}
	return nil, ErrNodeNotFound
}

// leave removes a server from the metaservice and raft
func (s *store) leave(n *NodeInfo) error {
	return s.raftState.removeVoter(n.TCPHost)
}

// removeMetaNode remove a server from the metaservice and raft
func (s *store) removeMetaNode(n *NodeInfo) (*NodeInfo, error) {
	if s.leaderHTTP() == n.TCPHost {
		return nil, fmt.Errorf("Can't remove leader node")
	}

	s.mu.RLock()
	if s.raftState == nil {
		s.mu.RUnlock()
		return nil, fmt.Errorf("store not open")
	}

	if err := s.raftState.removeVoter(n.TCPHost); err != nil {
		s.mu.RUnlock()
		s.logger.Error("removeMeta remove voter failed", zap.Error(err))
		return nil, err
	}
	s.mu.RUnlock()

	if err := s.callDeleteMetaNode(n.ID); err != nil {
		s.logger.Error("removeMeta remove meta node failed", zap.Error(err))
		return nil, err
	}

	s.mu.RLock()
	for _, node := range s.data.MetaNodes {
		if node.TCPHost == n.TCPHost && node.Host == n.Host {
			s.mu.RUnlock()
			return nil, fmt.Errorf("unable to drop the node in a cluster")
		}
	}
	s.mu.RUnlock()

	return n, nil
}

// callCreateMetaNode is used by the join command to create the metanode into
// the metastore
func (s *store) callCreateMetaNode(addr, raftAddr string) error {
	val := &internal.CreateMetaNodeCommand{
		HTTPAddr: proto.String(addr),
		TCPAddr:  proto.String(raftAddr),
		Rand:     proto.Uint64(uint64(rand.Int63())),
	}
	t := internal.Command_CreateMetaNodeCommand
	cmd := &internal.Command{Type: &t}
	if err := proto.SetExtension(cmd, internal.E_CreateMetaNodeCommand_Command, val); err != nil {
		panic(err)
	}

	b, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}

	return s.apply(b)
}

// callDeleteMetaNode is used by the leave command to delete the metanode from
// the metastore
func (s *store) callDeleteMetaNode(id uint64) error {
	val := &internal.DeleteMetaNodeCommand{
		ID: proto.Uint64(id),
	}
	t := internal.Command_DeleteMetaNodeCommand
	cmd := &internal.Command{Type: &t}
	if err := proto.SetExtension(cmd, internal.E_DeleteMetaNodeCommand_Command, val); err != nil {
		panic(err)
	}

	b, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}

	return s.apply(b)
}

// callSetMetaNode is used when the raft group has only a single peer. It will
// either create a metanode or update the information for the one metanode
// that is there. It's used because hostnames can change
func (s *store) callSetMetaNode(addr, raftAddr string) error {
	val := &internal.SetMetaNodeCommand{
		HTTPAddr: proto.String(addr),
		TCPAddr:  proto.String(raftAddr),
		Rand:     proto.Uint64(uint64(rand.Int63())),
	}
	t := internal.Command_SetMetaNodeCommand
	cmd := &internal.Command{Type: &t}
	if err := proto.SetExtension(cmd, internal.E_SetMetaNodeCommand_Command, val); err != nil {
		panic(err)
	}

	b, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}

	return s.apply(b)
}

// callSetData
func (s *store) callSetData() error {
	val := &internal.SetDataCommand{
		Data: s.data.marshal(),
	}
	t := internal.Command_SetDataCommand
	cmd := &internal.Command{Type: &t}
	if err := proto.SetExtension(cmd, internal.E_SetDataCommand_Command, val); err != nil {
		panic(err)
	}

	b, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}

	return s.apply(b)
}
