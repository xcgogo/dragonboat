// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
//
//
// Copyright 2017-2019 Lei Ni (nilei81@gmail.com)
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
//
// This file contains code derived from CockroachDB. The asyncSendSnapshot method,
// connectAndProcessSnapshot method and the processSnapshotQueue method is similar
// to the one used in CockroachDB.

package transport

import (
	"errors"
	"sync/atomic"

	"github.com/lni/dragonboat/internal/settings"
	"github.com/lni/dragonboat/internal/utils/fileutil"
	"github.com/lni/dragonboat/internal/utils/logutil"
	"github.com/lni/dragonboat/raftio"
	pb "github.com/lni/dragonboat/raftpb"
)

var (
	maxConnectionCount uint32 = uint32(settings.Soft.MaxSnapshotConnections)
)

// ASyncSendSnapshot sends raft snapshot message to its target.
func (t *Transport) ASyncSendSnapshot(m pb.Message) bool {
	if !t.asyncSendSnapshot(m) {
		plog.Debugf("failed to send snapshot to %s",
			logutil.DescribeNode(m.ClusterId, m.To))
		t.sendSnapshotNotification(m.ClusterId, m.To, true)
		return false
	}
	return true
}

// GetStreamConnection returns a connection used for streaming snapshot.
func (t *Transport) GetStreamConnection(clusterID uint64,
	nodeID uint64) *Sink {
	s := t.getStreamConnection(clusterID, nodeID)
	if s == nil {
		t.sendSnapshotNotification(clusterID, nodeID, true)
	}
	return s
}

func (t *Transport) getStreamConnection(clusterID uint64,
	nodeID uint64) *Sink {
	addr, _, err := t.resolver.Resolve(clusterID, nodeID)
	if err != nil {
		return nil
	}
	if !t.GetCircuitBreaker(addr).Ready() {
		return nil
	}
	key := raftio.GetNodeInfo(clusterID, nodeID)
	if c := t.tryCreateConnection(key, addr, true, 0); c != nil {
		return &Sink{l: c}
	}
	return nil
}

func (t *Transport) asyncSendSnapshot(m pb.Message) bool {
	toNodeID := m.To
	clusterID := m.ClusterId
	if m.Type != pb.InstallSnapshot {
		panic("non-snapshot message received by ASyncSendSnapshot")
	}
	chunks := splitSnapshotMessage(m)
	addr, _, err := t.resolver.Resolve(clusterID, toNodeID)
	if err != nil {
		return false
	}
	if !t.GetCircuitBreaker(addr).Ready() {
		return false
	}
	key := raftio.GetNodeInfo(clusterID, toNodeID)
	c := t.tryCreateConnection(key, addr, false, len(chunks))
	if c == nil {
		return false
	}
	c.sendSavedSnapshot(m)
	return true
}

func (t *Transport) tryCreateConnection(key raftio.NodeInfo,
	addr string, streaming bool, sz int) *connection {
	if v := atomic.AddUint32(&t.connections, 1); v > maxConnectionCount {
		r := atomic.AddUint32(&t.connections, ^uint32(0))
		plog.Errorf("connection count is rate limited %d", r)
		return nil
	}
	return t.createConnection(key, addr, streaming, sz)
}

func (t *Transport) createConnection(key raftio.NodeInfo,
	addr string, streaming bool, sz int) *connection {
	c := newConnection(t.ctx, key.ClusterID, key.NodeID,
		t.getDeploymentID(), streaming, sz, t.raftRPC, t.stopper.ShouldStop())
	c.streamChunkSent = t.streamChunkSent
	c.preStreamChunkSend = t.preStreamChunkSend
	shutdown := func() {
		atomic.AddUint32(&t.connections, ^uint32(0))
	}
	t.stopper.RunWorker(func() {
		t.connectAndProcessSnapshot(c, addr)
		plog.Infof("%s connectAndProcessSnapshot returned",
			logutil.DescribeNode(key.ClusterID, key.NodeID))
		shutdown()
	})
	return c
}

func (t *Transport) connectAndProcessSnapshot(c *connection, addr string) {
	breaker := t.GetCircuitBreaker(addr)
	successes := breaker.Successes()
	consecFailures := breaker.ConsecFailures()
	clusterID := c.clusterID
	nodeID := c.nodeID
	if err := func() error {
		if err := c.connect(addr); err != nil {
			plog.Warningf("failed to get snapshot client to %s",
				logutil.DescribeNode(clusterID, nodeID))
			t.sendSnapshotNotification(clusterID, nodeID, true)
			close(c.failed)
			return err
		}
		defer c.close()
		breaker.Success()
		if successes == 0 || consecFailures > 0 {
			plog.Infof("snapshot stream to %s (%s) established",
				logutil.DescribeNode(clusterID, nodeID), addr)
		}
		err := c.process()
		if err != nil {
			plog.Errorf("snapshot chunk processing failed %v", err)
		}
		t.sendSnapshotNotification(clusterID, nodeID, err != nil)
		return err
	}(); err != nil {
		plog.Warningf("connectAndProcessSnapshot failed: %v", err)
		breaker.Fail()
	}
}

func splitBySnapshotFile(msg pb.Message,
	filepath string, filesize uint64, startChunkID uint64,
	sf *pb.SnapshotFile) []pb.SnapshotChunk {
	if filesize == 0 {
		panic("empty file included in snapshot")
	}
	results := make([]pb.SnapshotChunk, 0)
	chunkCount := uint64((filesize-1)/snapChunkSize + 1)
	plog.Infof("splitBySnapshotFile called, chunkCount %d, filesize %d",
		chunkCount, filesize)
	for i := uint64(0); i < chunkCount; i++ {
		var csz uint64
		if i == chunkCount-1 {
			csz = filesize - (chunkCount-1)*snapChunkSize
		} else {
			csz = snapChunkSize
		}
		c := pb.SnapshotChunk{
			BinVer:         raftio.RPCBinVersion,
			ClusterId:      msg.ClusterId,
			NodeId:         msg.To,
			From:           msg.From,
			FileChunkId:    i,
			FileChunkCount: chunkCount,
			ChunkId:        startChunkID + i,
			ChunkSize:      csz,
			Index:          msg.Snapshot.Index,
			Term:           msg.Snapshot.Term,
			Membership:     msg.Snapshot.Membership,
			Filepath:       filepath,
			FileSize:       filesize,
		}
		if sf != nil {
			c.HasFileInfo = true
			c.FileInfo = *sf
		}
		results = append(results, c)
	}
	return results
}

func splitSnapshotMessage(m pb.Message) []pb.SnapshotChunk {
	if m.Type != pb.InstallSnapshot {
		panic("not a snapshot message")
	}
	startChunkID := uint64(0)
	results := splitBySnapshotFile(m,
		m.Snapshot.Filepath, m.Snapshot.FileSize, startChunkID, nil)
	startChunkID += uint64(len(results))
	for _, snapshotFile := range m.Snapshot.Files {
		chunks := splitBySnapshotFile(m,
			snapshotFile.Filepath, snapshotFile.FileSize, startChunkID, snapshotFile)
		results = append(results, chunks...)
		startChunkID += uint64(len(chunks))
	}
	for idx := range results {
		results[idx].ChunkCount = uint64(len(results))
	}
	return results
}

func loadSnapshotChunkData(chunk pb.SnapshotChunk,
	data []byte) ([]byte, error) {
	f, err := fileutil.OpenChunkFileForRead(chunk.Filepath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	offset := chunk.FileChunkId * snapChunkSize
	if _, err = f.SeekFromBeginning(int64(offset)); err != nil {
		return nil, err
	}
	if chunk.ChunkSize != uint64(len(data)) {
		data = make([]byte, chunk.ChunkSize)
	}
	n, err := f.Read(data)
	if err != nil {
		return nil, err
	}
	if uint64(n) != chunk.ChunkSize {
		return nil, errors.New("failed to read the chunk from snapshot file")
	}
	return data, nil
}
