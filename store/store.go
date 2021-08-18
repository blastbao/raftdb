// Package store provides a simple distributed key-value store. The keys and
// associated values are changed via distributed consensus, meaning that the
// values are changed only when a majority of nodes in the cluster agree on
// the new value.
//
// Distributed consensus is provided via the Raft algorithm, specifically the
// Hashicorp implementation.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	"github.com/hashicorp/raft-boltdb"
)

var (
	// ErrNotLeader is returned when a node attempts to execute a leader-only
	// operation.
	ErrNotLeader = errors.New("not leader")

	// ErrOpenTimeout is returned when the Store does not apply its initial
	// logs within the specified time.
	ErrOpenTimeout = errors.New("timeout waiting for initial logs application")
)

const (
	retainSnapshotCount = 2
	raftTimeout         = 10 * time.Second
	applyTimeout        = 10 * time.Second
	openTimeout         = 120 * time.Second
	leaderWaitDelay     = 100 * time.Millisecond
	appliedWaitDelay    = 100 * time.Millisecond
)

type command struct {
	Op    string `json:"op,omitempty"`
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
}

type ConsistencyLevel int

// Represents the available consistency levels.
const (
	Default ConsistencyLevel = iota
	Stale
	Consistent
)

// Store is a simple key-value store, where all changes are made via Raft consensus.
type Store struct {
	RaftDir  string
	RaftBind string

	mu sync.Mutex
	m  map[string]string // The key-value store for the system.

	raft *raft.Raft // The consensus mechanism

	logger *log.Logger
}

// New returns a new Store.
func New() *Store {
	return &Store{
		m:      make(map[string]string),
		logger: log.New(os.Stderr, "[store] ", log.LstdFlags),
	}
}

// LeaderAddr 获取 Leader Addr
func (s *Store) LeaderAddr() string {
	return string(s.raft.Leader())
}

// LeaderID returns the node ID of the Raft leader.
// Returns a blank string if there is no leader, or an error.
func (s *Store) LeaderID() (string, error) {
	// 获取 Leader Raft Addr
	addr := s.LeaderAddr()
	configFuture := s.raft.GetConfiguration()
	if err := configFuture.Error(); err != nil {
		s.logger.Printf("failed to get raft configuration: %v", err)
		return "", err
	}
	// 根据 Leader Raft Addr 获取 Leader NodeId
	for _, srv := range configFuture.Configuration().Servers {
		if srv.Address == raft.ServerAddress(addr) {
			return string(srv.ID), nil
		}
	}
	return "", nil
}

func (s *Store) LeaderAPIAddr() string {
	// 获取 Leader NodeId
	id, err := s.LeaderID()
	if err != nil {
		return ""
	}

	// 根据 Leader NodeId 获取 Leader Api Addr
	addr, err := s.GetMeta(id)
	if err != nil {
		return ""
	}

	return addr
}

// Open opens the store. If enableSingle is set, and there are no existing peers,
// then this node becomes the first node, and therefore leader, of the cluster.
// localID should be the server identifier for this node.
//
//
//
//
func (s *Store) Open(enableSingle bool, localID string) error {
	// Setup Raft configuration.
	// 设置 Raft 配置
	config := raft.DefaultConfig()
	config.LocalID = raft.ServerID(localID)

	newNode := !pathExists(filepath.Join(s.RaftDir, "raft.db"))

	// Setup Raft communication.
	// 设置 Raft 通信
	addr, err := net.ResolveTCPAddr("tcp", s.RaftBind)
	if err != nil {
		return err
	}
	transport, err := raft.NewTCPTransport(s.RaftBind, addr, 3, 10*time.Second, os.Stderr)
	if err != nil {
		return err
	}

	// Create the snapshot store. This allows the Raft to truncate the log.
	// 设置 Raft 存储对象
	snapshots, err := raft.NewFileSnapshotStore(s.RaftDir, retainSnapshotCount, os.Stderr)
	if err != nil {
		return fmt.Errorf("file snapshot store: %s", err)
	}

	// Create the log store and stable store.
	var logStore raft.LogStore
	var stableStore raft.StableStore

	boltDB, err := raftboltdb.NewBoltStore(filepath.Join(s.RaftDir, "raft.db"))
	if err != nil {
		return fmt.Errorf("new bolt store: %s", err)
	}
	logStore = boltDB
	stableStore = boltDB

	// Instantiate the Raft systems.
	// 创建 raft 实例，并使用该 raft 实例启动集群
	ra, err := raft.NewRaft(config, (*fsm)(s), logStore, stableStore, snapshots, transport)
	if err != nil {
		return fmt.Errorf("new raft: %s", err)
	}
	s.raft = ra

	if enableSingle && newNode {
		s.logger.Printf("bootstrap needed")
		configuration := raft.Configuration{
			Servers: []raft.Server{
				{
					ID:      config.LocalID,
					Address: transport.LocalAddr(),
				},
			},
		}
		ra.BootstrapCluster(configuration)
	} else {
		s.logger.Printf("no bootstrap needed")
	}

	return nil
}

// WaitForLeader blocks until a leader is detected, or the timeout expires.
//
// 获取 leader raft address ，直到获取成功。
func (s *Store) WaitForLeader(timeout time.Duration) (string, error) {
	// 100ms
	tck := time.NewTicker(leaderWaitDelay)
	defer tck.Stop()

	// timeout
	tmr := time.NewTimer(timeout)
	defer tmr.Stop()

	for {
		select {
		case <-tck.C:
			// 定时获取 leader raft addr ，为空则重试。
			l := s.LeaderAddr()
			if l != "" {
				return l, nil
			}
		case <-tmr.C:
			return "", fmt.Errorf("timeout expired")
		}
	}
}

// WaitForAppliedIndex blocks until a given log index has been applied,
// or the timeout expires.
func (s *Store) WaitForAppliedIndex(idx uint64, timeout time.Duration) error {

	tck := time.NewTicker(appliedWaitDelay)
	defer tck.Stop()

	tmr := time.NewTimer(timeout)
	defer tmr.Stop()

	for {
		select {
		case <-tck.C:
			//
			if s.raft.AppliedIndex() >= idx {
				return nil
			}
		case <-tmr.C:
			return fmt.Errorf("timeout expired")
		}
	}
}

// WaitForApplied waits for all Raft log entries to to be applied to the underlying database.
// WaitForApplied 等待所有 Raft 日志条目被应用到底层数据库。
func (s *Store) WaitForApplied(timeout time.Duration) error {
	if timeout == 0 {
		return nil
	}
	s.logger.Printf("waiting for up to %s for application of initial logs", timeout)
	if err := s.WaitForAppliedIndex(s.raft.LastIndex(), timeout); err != nil {
		return ErrOpenTimeout
	}
	return nil
}

// consistentRead is used to ensure we do not perform a stale
// read. This is done by verifying leadership before the read.
func (s *Store) consistentRead() error {
	future := s.raft.VerifyLeader()
	if err := future.Error(); err != nil {
		return err //fail fast if leader verification fails
	}

	return nil
}

// Get returns the value for the given key.
func (s *Store) Get(key string, lvl ConsistencyLevel) (string, error) {
	if lvl != Stale {
		if s.raft.State() != raft.Leader {
			return "", ErrNotLeader
		}
	}

	if lvl == Consistent {
		if err := s.consistentRead(); err != nil {
			return "", err
		}
	}

	// 备注：读操作不需要同步到 raft 集群，只需要从本地 fsm 中读取即可，raft 会保证每个节点的 fsm 是强一致的。
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[key], nil
}

// Set sets the value for the given key.
func (s *Store) Set(key, value string) error {
	// 仅 Leader 能处理写请求
	if s.raft.State() != raft.Leader {
		return ErrNotLeader
	}

	// 构造 Set 请求
	c := &command{
		Op:    "set",
		Key:   key,
		Value: value,
	}

	// 序列化
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}

	// 将 command 应用于 FSM
	//
	// 备注：写操作需要同步到整个 raft 集群，需要通过 raft + fsm 来同步变更。
	f := s.raft.Apply(b, raftTimeout)
	return f.Error()
}

// Delete deletes the given key.
func (s *Store) Delete(key string) error {
	if s.raft.State() != raft.Leader {
		return ErrNotLeader
	}

	c := &command{
		Op:  "delete",
		Key: key,
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}

	// 将 command 应用于 FSM
	//
	// 备注：写操作需要同步到整个 raft 集群，需要通过 raft + fsm 来同步变更。
	f := s.raft.Apply(b, raftTimeout)
	return f.Error()
}

// SetMeta 保存 Leader Api Addr 到 Leader NodeId 上
func (s *Store) SetMeta(key, value string) error {
	return s.Set(key, value)
}

// GetMeta 根据 Leader NodeId 获取 Leader Api Addr
func (s *Store) GetMeta(key string) (string, error) {
	return s.Get(key, Stale)
}

func (s *Store) DeleteMeta(key string) error {
	return s.Delete(key)
}

// Join joins a node, identified by nodeID and located at addr, to this store.
// The node must be ready to respond to Raft communications at that address.
//
// Join 将节点 nodeID 加入到集群，该节点必须准备好响应 addr 上的 Raft 通信。
// 只有 Leader 能够处理 Join 请求，其它节点收到 Join 请求会重定向到 Leader 节点。
func (s *Store) Join(nodeID, httpAddr string, addr string) error {
	s.logger.Printf("received join request for remote node %s at %s", nodeID, addr)

	configFuture := s.raft.GetConfiguration()
	if err := configFuture.Error(); err != nil {
		s.logger.Printf("failed to get raft configuration: %v", err)
		return err
	}

	// 遍历集群中所有节点
	for _, srv := range configFuture.Configuration().Servers {
		// If a node already exists with either the joining node's ID or address,
		// that node may need to be removed from the config first.
		//
		// 如果某个节点的 nodeID 或 raft addr 已经存在，可能需要先将其从集群中删除。
		if srv.ID == raft.ServerID(nodeID) || srv.Address == raft.ServerAddress(addr) {
			// However if *both* the ID and the address are the same, then nothing -- not even a join operation -- is needed.
			// 如果 nodeId 和 raft addr 均匹配，则当前节点已经存在于集群，无需 join ，直接返回。
			if srv.Address == raft.ServerAddress(addr) && srv.ID == raft.ServerID(nodeID) {
				s.logger.Printf("node %s at %s already member of cluster, ignoring join request", nodeID, addr)
				return nil
			}
			// 否则，存在冲突节点，将旧节点从集群中移除
			future := s.raft.RemoveServer(srv.ID, 0, 0)
			if err := future.Error(); err != nil {
				return fmt.Errorf("error removing existing node %s at %s: %s", nodeID, addr, err)
			}
		}
	}

	// 把 NodeId => RaftAddr 加入 raft 集群中
	f := s.raft.AddVoter(raft.ServerID(nodeID), raft.ServerAddress(addr), 0, 0)
	if f.Error() != nil {
		return f.Error()
	}

	// Set meta info
	// 完成 Join ，则将 NodeId => ApiAddr 保存到 store 中。
	if err := s.SetMeta(nodeID, httpAddr); err != nil {
		return err
	}

	s.logger.Printf("node %s at %s joined successfully", nodeID, addr)
	return nil
}







type fsm Store

// Apply applies a Raft log entry to the key-value store.
func (f *fsm) Apply(l *raft.Log) interface{} {
	// 解析命令
	var c command
	if err := json.Unmarshal(l.Data, &c); err != nil {
		panic(fmt.Sprintf("failed to unmarshal command: %s", err.Error()))
	}
	// 执行命令
	switch c.Op {
	case "set":
		return f.applySet(c.Key, c.Value)
	case "delete":
		return f.applyDelete(c.Key)
	default:
		panic(fmt.Sprintf("unrecognized command op: %s", c.Op))
	}
}

// Snapshot returns a snapshot of the key-value store.
func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Clone the map.
	o := make(map[string]string)
	for k, v := range f.m {
		o[k] = v
	}
	return &fsmSnapshot{store: o}, nil
}

// Restore stores the key-value store to a previous state.
func (f *fsm) Restore(rc io.ReadCloser) error {
	o := make(map[string]string)
	if err := json.NewDecoder(rc).Decode(&o); err != nil {
		return err
	}

	// Set the state from the snapshot, no lock required according to
	// Hashicorp docs.
	f.m = o
	return nil
}

func (f *fsm) applySet(key, value string) interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[key] = value
	return nil
}

func (f *fsm) applyDelete(key string) interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, key)
	return nil
}

type fsmSnapshot struct {
	store map[string]string
}

func (f *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	err := func() error {
		// Encode data.
		b, err := json.Marshal(f.store)
		if err != nil {
			return err
		}

		// Write data to sink.
		if _, err := sink.Write(b); err != nil {
			return err
		}

		// Close the sink.
		return sink.Close()
	}()

	if err != nil {
		sink.Cancel()
	}

	return err
}

func (f *fsmSnapshot) Release() {}

// pathExists returns true if the given path exists.
func pathExists(p string) bool {
	if _, err := os.Lstat(p); err != nil && os.IsNotExist(err) {
		return false
	}
	return true
}
