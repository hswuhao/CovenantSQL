/*
 * Copyright 2018 The ThunderDB Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package sqlchain

// TODO(leventeliu): use pooled objects to speed up this index.

import (
	"sync"

	"gitlab.com/thunderdb/ThunderDB/crypto/hash"
	ct "gitlab.com/thunderdb/ThunderDB/sqlchain/types"
	wt "gitlab.com/thunderdb/ThunderDB/worker/types"
)

var (
	placeHolder = &hash.Hash{}
)

// requestTracker defines a tracker of a particular database query request.
// We use it to track and update queries in this index system.
type requestTracker struct {
	// TODO(leventeliu): maybe we don't need them to be "signed" here. Given that the response or
	// Ack is already verified, simply use Header.
	response *wt.SignedResponseHeader
	ack      *wt.SignedAckHeader
	// signedBlock is the hash of the block in the currently best chain which contains this query.
	signedBlock *hash.Hash
}

// queryTracker defines a tracker of a particular database query. It may contain multiple queries
// to differe workers.
type queryTracker struct {
	firstAck *requestTracker
	queries  []*requestTracker
}

// newQueryTracker returns a new queryTracker reference.
func newQueryTracker() *queryTracker {
	return &queryTracker{
		// TODO(leventeliu): set appropriate capacity.
		firstAck: nil,
		queries:  make([]*requestTracker, 0, 10),
	}
}

// updateAck updates the query tracker with a verified SignedAckHeader.
func (s *requestTracker) updateAck(ack *wt.SignedAckHeader) (isNew bool, err error) {
	if s.ack == nil {
		// A later Ack can overwrite the original Response setting
		*s = requestTracker{
			response: ack.SignedResponseHeader(),
			ack:      ack,
		}

		isNew = true
	} else if !s.ack.HeaderHash.IsEqual(&ack.HeaderHash) {
		// This may happen when a client sends multiple acknowledgements for a same query (same
		// response header hash)
		err = ErrMultipleAckOfResponse
	} // else it's same as s.Ack, let's try not to overwrite it

	return
}

// hashIndex defines a requestTracker index using hash as key.
type hashIndex map[hash.Hash]*requestTracker

// seqIndex defines a queryTracker index using sequence number as key.
type seqIndex map[uint64]*queryTracker

// ensure returns the *queryTracker associated with the given key. It creates a new item if the
// key doesn't exist.
func (i seqIndex) ensure(k uint64) (v *queryTracker) {
	var ok bool

	if v, ok = i[k]; !ok {
		v = newQueryTracker()
		i[k] = v
	}

	return
}

// multiIndex defines a combination of multiple indexes.
//
// Index layout is as following:
//
//  respIndex                                    +----------------+
//                +---------------------------+->| requestTracker |       +---------------------------+
// |  ...   |     |                           |  | +-response     |------>| signedresponseheader      |
// +--------+     |                           |  | +-ack (nil)    |       | +-ResponseHeader          |
// | hash#1 |-----+                           |  | +-...          |       | | +-SignedRequestHeader   |
// +--------+                                 |  +----------------+       | | | +-RequestHeader       |
// |  ...   |                                 |                           | | | | +-...               |
// +--------+           +------------------+  |                           | | | | +-SeqNo: seq#0      |
// | hash#3 |-----+  +->| queryTracker     |  |                           | | | | +-...               |
// +--------+     |  |  | +-firstAck (nil) |  |                           | | | +-HeaderHash = hash#0 |
// |  ...   |     |  |  | +-queries        |  |                           | | | +-Signee ====> pubk#0 |
// +--------+     |  |  |   +-[0]          |--+                           | | | +-Signature => sign#0 |
// | hash#6 |--+  |  |  |   +-...          |                              | | +-...                   |
// +--------+  |  |  |  +------------------+                              | +-HeaderHash = hash#1     |
// |  ...   |  |  |  |                                                    | +-Signee ====> pubk#1     |
//             |  |  |                                                    | +-Signature => sign#1     |
//             |  |  |                                                    +---------------------------+
//             |  |  |                           +----------------+
//             |  +-------------+---------+-+--->| requestTracker |
//             |     |          |         | |    | +-response     |----+  +-------------------------------+
//  ackindex   |     |          |         | |    | +-ack          |----|->| SignedAckHeader               |
//             |     |          |         | |    | +-...          |    |  | +-AckHeader                   |
// |  ...   |  |     |          |         | |    +----------------+    +->| | +-SignedResponseHeader      |
// +--------+  |     |          |         | |                             | | | +-ResponseHeader          |
// | hash#4 |--|----------------+         | |                             | | | | +-SignedRequestHeader   |
// +--------+  |     |                    | |                             | | | | | +-RequestHeader       |
// |  ...   |  |     |                    | |                             | | | | | | +-...               |
//             |     |                    | |                             | | | | | | +-SeqNo: seq#1      |
//             |     |                    | |                             | | | | | | +-...               |
//             |     |                    | |                             | | | | | +-HeaderHash = hash#2 |
//             |     |                    | |                             | | | | | +-Signee ====> pubk#2 |
//             |     |                    | |                             | | | | | +-Signature => sign#2 |
//  seqIndex   |     |                    | |    +----------------+       | | | | +-...                   |
//             +------------------------------+->| requestTracker |       | | | +-HeaderHash = hash#3     |
// |  ...   |        |                    | | |  | +-response     |---+   | | | +-signee ====> pubk#3     |
// +--------+        |                    | | |  | +-ack (nil)    |   |   | | | +-Signature => sign#3     |
// | seq#0  |--------+                    | | |  | +-...          |   |   | | +-...                       |
// +--------+                             | | |  +----------------+   |   | +-HeaderHash = hash#4         |
// |  ...   |                             | | |                       |   | +-Signee ====> pubk#2         |
// +--------+           +--------------+  | | |                       |   | +-Signature => sign#4         |
// | seq#1  |---------->| queryTracker |  | | |                       |   +-------------------------------+
// +--------+           | +-firstAck   |--+ | |                       |
// |  ...   |           | +-queries    |    | |                       |
//                      |   +-[0]      |----+ |                       |
//                      |   +-[1]      |------+                       |   +---------------------------+
//                      |   +-...      |                              +-->| SignedResponseHeader      |
//                      +--------------+                                  | +-ResponseHeader          |
//                                                                        | | +-SignedRequestHeader   |
//                                                                        | | | +-RequestHeader       |
//                                                                        | | | | +-...               |
//                                                                        | | | | +-SeqNo: seq#1      |
//                                                                        | | | | +-...               |
//                                                                        | | | +-HeaderHash = hash#5 |
//                                                                        | | | +-Signee ====> pubk#5 |
//                                                                        | | | +-Signature => sign#5 |
//                                                                        | | +-...                   |
//                                                                        | +-HeaderHash = hash#6     |
//                                                                        | +-Signee ====> pubk#6     |
//                                                                        | +-Signature => sign#6     |
//                                                                        +---------------------------+
//
type multiIndex struct {
	sync.Mutex
	respIndex, ackIndex hashIndex
	seqIndex
}

// newMultiIndex returns a new multiIndex reference.
func newMultiIndex() *multiIndex {
	return &multiIndex{
		respIndex: make(map[hash.Hash]*requestTracker),
		ackIndex:  make(map[hash.Hash]*requestTracker),
		seqIndex:  make(map[uint64]*queryTracker),
	}
}

// AddResponse adds the responsed query to the index.
func (i *multiIndex) AddResponse(resp *wt.SignedResponseHeader) (err error) {
	i.Lock()
	defer i.Unlock()

	if v, ok := i.respIndex[resp.HeaderHash]; ok {
		if v == nil || v.response == nil {
			// TODO(leventeliu): consider to panic.
			err = ErrCorruptedIndex
			return
		}

		// Given that `resp` is already verified by user, its header should be deeply equal to
		// v.response.ResponseHeader.
		// Considering that we may allow a node to update its key pair on-the-fly, just overwrite
		// this response.
		v.response = resp
		return
	}

	// Create new item
	s := &requestTracker{
		response: resp,
	}

	i.respIndex[resp.HeaderHash] = s
	q := i.seqIndex.ensure(resp.Request.SeqNo)
	q.queries = append(q.queries, s)

	return nil
}

// addAck adds the acknowledged query to the index.
func (i *multiIndex) addAck(ack *wt.SignedAckHeader) (err error) {
	i.Lock()
	defer i.Unlock()
	var v *requestTracker
	var ok bool
	q := i.seqIndex.ensure(ack.SignedRequestHeader().SeqNo)

	if v, ok = i.respIndex[ack.ResponseHeaderHash()]; ok {
		if v == nil || v.response == nil {
			// TODO(leventeliu): consider to panic.
			err = ErrCorruptedIndex
			return
		}

		// Add hash -> ack index anyway, so that we can find the request tracker later, even if
		// there is a earlier acknowledgement for the same request
		i.ackIndex[ack.HeaderHash] = v

		// This also updates the item indexed by ackIndex and seqIndex
		var isNew bool

		if isNew, err = v.updateAck(ack); err != nil {
			return
		}

		if isNew {
			q.queries = append(q.queries, v)
		}
	} else {
		// Build new queryTracker and update both indexes
		v = &requestTracker{
			response: ack.SignedResponseHeader(),
			ack:      ack,
		}

		i.respIndex[ack.ResponseHeaderHash()] = v
		i.ackIndex[ack.HeaderHash] = v
		q.queries = append(q.queries, v)
	}

	// TODO(leventeliu):
	// This query has multiple signed acknowledgements. It may be caused by a network problem.
	// We will keep the first ack counted anyway. But, should we report it to someone?
	if q.firstAck == nil {
		q.firstAck = v
	} else if !q.firstAck.ack.HeaderHash.IsEqual(&ack.HeaderHash) {
		err = ErrMultipleAckOfSeqNo
	}

	return
}

// setSignedBlock sets the signed block of the acknowledged query.
func (i *multiIndex) setSignedBlock(blockHash *hash.Hash, ackHeaderHash *hash.Hash) {
	i.Lock()
	defer i.Unlock()

	if v, ok := i.ackIndex[*ackHeaderHash]; ok {
		v.signedBlock = blockHash
	}
}

// checkBeforeExpire checks the index and does some necessary work before it expires.
func (i *multiIndex) checkBeforeExpire() {
	i.Lock()
	defer i.Unlock()

	for _, q := range i.seqIndex {
		if ack := q.firstAck; ack == nil {
			// TODO(leventeliu):
			// This query is not acknowledged and expires now.
		} else if ack.signedBlock == nil || ack.signedBlock == placeHolder {
			// TODO(leventeliu):
			// This query was acknowledged normally but collectors didn't pack it in any block.
			// There is definitely something wrong with them.
		}

		for _, s := range q.queries {
			if s != q.firstAck {
				// TODO(leventeliu): so these guys lost the competition in this query. Should we
				// do something about it?
			}
		}
	}
}

// checkAckFromBlock checks a acknowledged query from a block in this index.
func (i *multiIndex) checkAckFromBlock(b *hash.Hash, ack *hash.Hash) (isKnown bool, err error) {
	i.Lock()
	defer i.Unlock()

	// Check acknowledgement
	q, isKnown := i.ackIndex[*ack]

	if !isKnown {
		return
	}

	if q.signedBlock != nil && !q.signedBlock.IsEqual(b) {
		err = ErrQuerySignedByAnotherBlock
		return
	}

	qs := i.seqIndex[q.ack.SignedRequestHeader().SeqNo]

	// Check it as a first acknowledgement
	if i.respIndex[q.response.HeaderHash] != q || qs == nil || qs.firstAck == nil {
		err = ErrCorruptedIndex
		return
	}

	// If `q` is not considered first acknowledgement of this query locally
	if qs.firstAck != q {
		if qs.firstAck.signedBlock != nil {
			err = ErrQuerySignedByAnotherBlock
			return
		}

		// But if the acknowledgement is not signed yet, it is also acceptable to promote another
		// acknowledgement
		qs.firstAck = q
	}

	return
}

// markAndCollectUnsignedAcks marks and collects all the unsigned acknowledgements in the index.
func (i *multiIndex) markAndCollectUnsignedAcks(qs *[]*hash.Hash) {
	i.Lock()
	defer i.Unlock()

	for _, q := range i.seqIndex {
		if ack := q.firstAck; ack != nil && ack.signedBlock == nil {
			ack.signedBlock = placeHolder
			*qs = append(*qs, &ack.ack.HeaderHash)
		}
	}
}

// heightIndex defines a MultiIndex index using height as key.
type heightIndex struct {
	sync.Mutex
	index map[int32]*multiIndex
}

// ensureHeight returns the *MultiIndex associated with the given height. It creates a new item if
// the key doesn't exist.
func (i *heightIndex) ensureHeight(h int32) (v *multiIndex) {
	i.Lock()
	defer i.Unlock()
	v, ok := i.index[h]

	if !ok {
		v = newMultiIndex()
		i.index[h] = v
	}

	return
}

// ensureRange creates new *multiIndex items associated within the given height range [l, h) for
// those don't exist.
func (i *heightIndex) ensureRange(l, h int32) {
	i.Lock()
	defer i.Unlock()

	for x := l; x < h; x++ {
		if _, ok := i.index[x]; !ok {
			i.index[x] = newMultiIndex()
		}
	}
}

func (i *heightIndex) get(k int32) (v *multiIndex, ok bool) {
	i.Lock()
	defer i.Unlock()
	v, ok = i.index[k]
	return
}

func (i *heightIndex) del(k int32) {
	i.Lock()
	defer i.Unlock()
	delete(i.index, k)
}

// queryIndex defines a query index maintainer.
type queryIndex struct {
	barrier     int32
	heightIndex *heightIndex
}

// newQueryIndex returns a new queryIndex reference.
func newQueryIndex() *queryIndex {
	return &queryIndex{
		heightIndex: &heightIndex{
			index: make(map[int32]*multiIndex),
		},
	}
}

// addResponse adds the responsed query to the index.
func (i *queryIndex) addResponse(h int32, resp *wt.SignedResponseHeader) error {
	// TODO(leventeliu): we should ensure that the Request uses coordinated timestamp, instead of
	// any client local time.
	return i.heightIndex.ensureHeight(h).AddResponse(resp)
}

// addAck adds the acknowledged query to the index.
func (i *queryIndex) addAck(h int32, ack *wt.SignedAckHeader) error {
	return i.heightIndex.ensureHeight(h).addAck(ack)
}

// checkAckFromBlock checks a acknowledged query from a block at the given height.
func (i *queryIndex) checkAckFromBlock(h int32, b *hash.Hash, ack *hash.Hash) (bool, error) {
	if h < i.barrier {
		return false, ErrQueryExpired
	}

	return i.heightIndex.ensureHeight(h).checkAckFromBlock(b, ack)
}

// setSignedBlock updates the signed block in index for the acknowledged queries in the block.
func (i *queryIndex) setSignedBlock(h int32, b *ct.Block) {
	hi := i.heightIndex.ensureHeight(h)

	for _, v := range b.Queries {
		hi.setSignedBlock(&b.SignedHeader.BlockHash, v)
	}
}

// getAck gets the acknowledged queries from the index.
func (i *queryIndex) getAck(h int32, header *hash.Hash) (
	ack *wt.SignedAckHeader, err error,
) {
	if h >= i.barrier {
		if q, ok := i.heightIndex.ensureHeight(h).ackIndex[*header]; ok {
			ack = q.ack
		} else {
			err = ErrQueryNotCached
		}
	} else {
		err = ErrQueryExpired
	}

	return
}

// advanceBarrier moves barrier to given height. All buckets lower than this height will be set as
// expired, and all the queries which are not packed in these buckets will be reported.
func (i *queryIndex) advanceBarrier(height int32) {
	for x := i.barrier; x < height; x++ {
		if hi, ok := i.heightIndex.get(x); ok {
			hi.checkBeforeExpire()
			i.heightIndex.del(x)
		}
	}

	i.barrier = height
}

// markAndCollectUnsignedAcks marks and collects all the unsigned acknowledgements which can be
// signed by a block at the given height.
func (i *queryIndex) markAndCollectUnsignedAcks(height int32) (qs []*hash.Hash) {
	qs = make([]*hash.Hash, 0, 1024)

	for x := i.barrier; x < height; x++ {
		if hi, ok := i.heightIndex.get(x); ok {
			hi.markAndCollectUnsignedAcks(&qs)
		}
	}

	return
}