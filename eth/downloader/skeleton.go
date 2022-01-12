// Copyright 2021 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package downloader

import (
	"encoding/json"
	"errors"
	"math/rand"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
)

// scratchHeaders is the number of headers to store in a scratch space to allow
// concurrent downloads. A header is about 0.5KB in size, so there is no worry
// about using too much memory. The only catch is that we can only validate gaps
// afer they're linked to the head, so the bigger the scratch space, the larger
// potential for invalid headers.
//
// The current scratch space of 131072 headers is expected to use 64MB RAM.
const scratchHeaders = 131072

// requestHeaders is the number of header to request from a remote peer in a single
// network packet. Although the skeleton downloader takes into consideration peer
// capacities when picking idlers, the packet size was decided to remain constant
// since headers are relatively small and it's easier to work with fixed batches
// vs. dynamic interval fillings.
const requestHeaders = 512

// errSyncLinked is an internal helper error to signal that the current sync
// cycle linked up to the genesis block, this the skeleton syncer should ping
// the backfiller to resume. Since we already have that logic on sync start,
// piggie-back on that instead of 2 entrypoints.
var errSyncLinked = errors.New("sync linked")

// errSyncMerged is an internal helper error to signal that the current sync
// cycle merged with a previously aborted subchain, thus the skeleton syncer
// should abort and restart with the new state.
var errSyncMerged = errors.New("sync merged")

// errSyncReorged is an internal helper error to signal that the head chain of
// the current sync cycle was (partially) reorged, thus the skeleton syncer
// should abort and restart with the new state.
var errSyncReorged = errors.New("sync reorged")

// errTerminated is returned if the sync mechanism was terminated for this run of
// the process. This is usually the case when Geth is shutting down and some events
// might still be propagating.
var errTerminated = errors.New("terminated")

func init() {
	// Tuning parameters is nice, but the scratch space must be assignable in
	// full to peers. It's a useless cornercase to support a dangling half-group.
	if scratchHeaders%requestHeaders != 0 {
		panic("Please make scratchHeaders divisible by requestHeaders")
	}
}

// subchain is a contiguous header chain segment that is backed by the database,
// but may not be linked to the live chain. The skeleton downloader may produce
// a new one of these every time it is restarted until the subchain grows large
// enough to connect with a previous subchain.
//
// The subchains use the exact same database namespace and are not disjoint from
// each other. As such, extending one to overlap the other entails reducing the
// second one first. This combined buffer model is used to avoid having to move
// data on disk when two subchains are joined together.
type subchain struct {
	Head uint64      // Block number of the newest header in the subchain
	Tail uint64      // Block number of the oldest header in the subchain
	Next common.Hash // Block hash of the next oldest header in the subchain
}

// skeletonProgress is a database entry to allow suspending and resuming a chain
// sync. As the skeleton header chain is downloaded backwards, restarts can and
// will produce temporarilly disjoint subchains. There is no way to restart a
// suspended skeleton sync without prior knowlege of all prior suspension points.
type skeletonProgress struct {
	Subchains []*subchain // Disjoint subchains downloaded until now
}

// headerRequest tracks a pending header request to ensure responses are to
// actual requests and to validate any security constraints.
//
// Concurrency note: header requests and responses are handled concurrently from
// the main runloop to allow Keccak256 hash verifications on the peer's thread and
// to drop on invalid response. The request struct must contain all the data to
// construct the response without accessing runloop internals (i.e. subchains).
// That is only included to allow the runloop to match a response to the task being
// synced without having yet another set of maps.
type headerRequest struct {
	peer string // Peer to which this request is assigned
	id   uint64 // Request ID of this request

	deliver chan *headerResponse // Channel to deliver successful response on
	revert  chan *headerRequest  // Channel to deliver request failure on
	cancel  chan struct{}        // Channel to track sync cancellation
	stale   chan struct{}        // Channel to signal the request was dropped

	head uint64 // Head number of the requested batch of headers
}

// headerResponse is an already verified remote response to a header request.
type headerResponse struct {
	peer    *peerConnection // Peer from which this response originates
	reqid   uint64          // Request ID that this response fulfils
	headers []*types.Header // Chain of headers
}

// backfiller is a callback interface through which the skeleton sync can tell
// the downloader that it should suspend or resume backfilling on specific head
// events (e.g. suspend on forks or gaps, resume on successfull linkups).
type backfiller interface {
	// suspend requests the backfiller to abort any running full or snap sync
	// based on the skeleton chain as it might be invalid. The backfiller should
	// gracefully handle multiple consecutive suspends without a resume, even
	// on initial sartup.
	suspend()

	// resume requests the backfiller to start running fill or snap sync based on
	// the skeleton chain as it has successfully been linked. Appending new heads
	// to the end of the chain will not result in suspend/resume cycles.
	resume()
}

// skeleton represents a header chain synchronized after the Ethereum 2 merge,
// where blocks aren't validated any more via PoW in a forward fashion, rather
// are dictated and extended at the head via the beacon chain and backfilled on
// the original Ethereum 1 block sync protocol.
//
// Since the skeleton is grown backwards from head to genesis, it is handled as
// a separate entity, not mixed in with the logical sequential transition of the
// blocks. Once the skeleton is connected to an existing, validated chain, the
// headers will be moved into the main downloader for filling and execution.
//
// Opposed to the Ethereum 1 block synchronization which is trustless (and uses a
// master peer to minimize the attack surface), Ethereum 2 block synchronization
// starts from a trusted head. As such, there is no need for a master peer any
// more and headers can be requested fully concurrently (though some batches might
// be discarded if they don't link up correctly).
//
// Although a skeleton is part of a sync cycle, it is not recreated, rather stays
// alive throughout the lifetime of the downloader. This allows it to be extended
// concurrently with the sync cycle, since extensions arrive from an API surface,
// not from within (vs. Ethereum 1 sync).
//
// Since the skeleton tracks the entire header chain until it is cosumed by the
// forward block filling, it needs 0.5KB/block storage. At current mainnet sizes
// this is only possible with a disk backend. Since the skeleton is separate from
// the node's header chain, storing the headers ephemerally until sync finishes
// is wasted disk IO, but it's a price we're going to pay to keep things simple
// for now.
type skeleton struct {
	db     ethdb.Database // Database backing the skeleton
	filler backfiller     // Chain syncer suspended/resumed by head events

	peers *peerSet                   // Set of peers we can sync from
	idles map[string]*peerConnection // Set of idle peers in the current sync cycle
	drop  peerDropFn                 // Drops a peer for misbehaving

	progress *skeletonProgress // Sync progress tracker for resumption and metrics
	started  time.Time         // Timestamp when the skeleton syncer was created
	logged   time.Time         // Timestamp when progress was last logged to the user
	pulled   uint64            // Number of headers downloaded in this run

	scratchSpace  []*types.Header // Scratch space to accumulate headers in (first = recent)
	scratchOwners []string        // Peer IDs owning chunks of the scratch space (pend or delivered)
	scratchHead   uint64          // Block number of the first item in the scratch space

	requests map[uint64]*headerRequest // Header requests currently running

	headEvents chan *types.Header // Notification channel for new heads
	terminate  chan chan error    // Termination channel to abort sync
	terminated chan struct{}      // Channel to signal that the syner is dead

	// Callback hooks used during testing
	syncStarting func() // callback triggered after a sync cycle is inited but before started
}

// newSkeleton creates a new sync skeleton that tracks a potentially dangling
// header chain until it's linked into an existing set of blocks.
func newSkeleton(db ethdb.Database, peers *peerSet, drop peerDropFn, filler backfiller) *skeleton {
	sk := &skeleton{
		db:         db,
		filler:     filler,
		peers:      peers,
		drop:       drop,
		requests:   make(map[uint64]*headerRequest),
		headEvents: make(chan *types.Header),
		terminate:  make(chan chan error),
		terminated: make(chan struct{}),
	}
	go sk.startup()
	return sk
}

// startup is an initial background loop which waits for an event to start or
// tear the syncer down. This is required to make the skeleton sync loop once
// per process but at the same time not start before the beacon chain announces
// a new (existing) head.
func (s *skeleton) startup() {
	// Close a notification channel so anyone sending us events will know if the
	// sync loop was torn down for good.
	defer close(s.terminated)

	// Wait for startup or teardown
	select {
	case errc := <-s.terminate:
		// No head was announced but Geth is shutting down
		errc <- nil
		return

	case head := <-s.headEvents:
		// New head announced, start syncing to it, looping every time a current
		// cycle is terminated due to a chain event (head reorg, old chain merge)
		s.started = time.Now()

		for {
			// If the sync cycle terminated or was terminated, propagate up when
			// higher layers request termination. There's no fancy explicit error
			// signalling as the sync loop should never terminate (TM).
			newhead, err := s.sync(head)
			switch {
			case err == errSyncLinked:
				// Sync cycle linked up to the genesis block. Tear down the loop
				// and restart it so, it can properly notify the backfiller. Don't
				// account a new head.
				head = nil

			case err == errSyncMerged:
				// Subchains were merged, we just need to reinit the internal
				// start to continue on the tail of the merged chain. Don't
				// announce a new head,
				head = nil

			case err == errSyncReorged:
				// The subchain being synced got modified at the head in a
				// way that requires resyncing it. Restart sync with the new
				// head to force a cleanup.
				head = newhead

			case err == errTerminated:
				// Sync was requested to be terminated from within, stop and
				// return (no need to pass a message, was already done internally)
				return

			default:
				// Sync either successfully terminated or failed with an unhandled
				// error. Abort and wait until Geth requests a termination.
				errc := <-s.terminate
				errc <- err
				return
			}
		}
	}
}

// Terminate tears down the syncer indefinitely.
func (s *skeleton) Terminate() error {
	// Request termination and fetch any errors
	errc := make(chan error)
	s.terminate <- errc
	err := <-errc

	// Wait for full shutdown (not necessary, but cleaner)
	<-s.terminated
	return err
}

// Sync starts or resumes a previous sync cycle to download and maintain a reverse
// header chain starting at the head and leading towards genesis to an available
// ancestor.
//
// This method does not block, rather it just waits until the syncer receives the
// fed header. What the syncer does with it is the syncer's problem.
func (s *skeleton) Sync(head *types.Header) error {
	log.Trace("New skeleton head announced", "number", head.Number, "hash", head.Hash())
	select {
	case s.headEvents <- head:
		return nil
	case <-s.terminated:
		return errTerminated
	}
}

// sync is the internal version of Sync that executes a single sync cycle, either
// until some termination condition is reached, or until the current cycle merges
// with a previously aborted run.
func (s *skeleton) sync(head *types.Header) (*types.Header, error) {
	// If we're continuing a previous merge interrupt, just access the existing
	// old state without initing from disk.
	if head == nil {
		head = rawdb.ReadSkeletonHeader(s.db, s.progress.Subchains[0].Head)
	} else {
		// Otherwise, initialize the sync, trimming and previous leftovers until
		// we're consistent with the newly requested chain head
		s.initSync(head)
	}
	// Create the scratch space to fill with concurrently downloaded headers
	s.scratchSpace = make([]*types.Header, scratchHeaders)
	defer func() { s.scratchSpace = nil }() // don't hold on to references after sync

	s.scratchOwners = make([]string, scratchHeaders/requestHeaders)
	defer func() { s.scratchOwners = nil }() // don't hold on to references after sync

	s.scratchHead = s.progress.Subchains[0].Tail - 1 // tail must not be 0!

	// If the sync is already done, resume the backfiller. When the loop stops,
	// terminate the backfiller too.
	if s.scratchHead == 0 {
		s.filler.resume()
	}
	defer s.filler.suspend()

	// Create a set of unique channels for this sync cycle. We need these to be
	// ephemeral so a data race doesn't accidentally deliver something stale on
	// a persistent channel across syncs (yup, this happened)
	var (
		requestFails = make(chan *headerRequest)
		responses    = make(chan *headerResponse)
	)
	cancel := make(chan struct{})
	defer close(cancel)

	log.Debug("Starting reverse header sync cycle", "head", head.Number, "hash", head.Hash(), "cont", s.scratchHead)

	// Whether sync completed or not, disregard any future packets
	defer func() {
		log.Debug("Terminating reverse header sync cycle", "head", head.Number, "hash", head.Hash(), "cont", s.scratchHead)
		s.requests = make(map[uint64]*headerRequest)
	}()

	// Start tracking idle peers for task assignments
	peering := make(chan *peeringEvent, 64) // arbitrary buffer, just some burst protection

	peeringSub := s.peers.SubscribeEvents(peering)
	defer peeringSub.Unsubscribe()

	s.idles = make(map[string]*peerConnection)
	for _, peer := range s.peers.AllPeers() {
		s.idles[peer.id] = peer
	}
	// Nofity any tester listening for startup events
	if s.syncStarting != nil {
		s.syncStarting()
	}
	for {
		// Something happened, try to assign new tasks to any idle peers
		s.assingTasks(responses, requestFails, cancel)

		// Wait for something to happen
		select {
		case event := <-peering:
			// A peer joined or left, the tasks queue and allocations need to be
			// checked for potential assignment or reassignment
			peerid := event.peer.id
			if event.join {
				s.idles[peerid] = event.peer
			} else {
				s.revertRequests(peerid)
				delete(s.idles, peerid)
			}

		case errc := <-s.terminate:
			errc <- nil
			return nil, errTerminated

		case head := <-s.headEvents:
			// New head was announced, try to integrate it. If successful, nothing
			// needs to be done as the head simply extended the last range. For now
			// we don't seamlessly integrate reorgs to keep things simple. If the
			// network starts doing many mini reorgs, it might be worthwhile handling
			// a limited depth without an error.
			if reorged := s.processNewHead(head); reorged {
				return head, errSyncReorged
			}
			// New head was integrated into the skeleton chain. If the backfiller
			// is still running, it will pick it up. If it already terminated,
			// a new cycle needs to be spun up.
			if s.scratchHead == 0 {
				s.filler.resume()
			}

		case req := <-requestFails:
			s.revertRequest(req)

		case res := <-responses:
			// Process the batch of headers. If though processing we managed to
			// link the curret subchain to a previously downloaded one, abort the
			// sync and restart with the merged subchains. We could probably hack
			// the internal state to switch the scratch space over to the tail of
			// the extended subchain, but since the scenario is rare, it's cleaner
			// to rely on the restart mechanism than a stateful modification.
			if merged := s.processResponse(res); merged {
				return nil, errSyncMerged
			}
			// If we've just reached the genesis block, tear down the sync cycle
			// and restart it to resume the backfiller. We could just as well do
			// a signalling here, but it's a tad cleaner to have only one entry
			// pathway to suspending/resuming it.
			return nil, errSyncLinked
		}
	}
}

// initSync attempts to get the skeleton sync into a consistent state wrt any
// past state on disk and the newly requested head to sync to. If the new head
// is nil, the method will return and continue from the previous head.
func (s *skeleton) initSync(head *types.Header) {
	// Extract the head number, we'll need it all over
	number := head.Number.Uint64()

	// Retrieve the previously saved sync progress
	if status := rawdb.ReadSkeletonSyncStatus(s.db); len(status) > 0 {
		s.progress = new(skeletonProgress)
		if err := json.Unmarshal(status, s.progress); err != nil {
			log.Error("Failed to decode skeleton sync status", "err", err)
		} else {
			// Previous sync was available, print some continuation logs
			for _, subchain := range s.progress.Subchains {
				log.Debug("Restarting skeleton subchain", "head", subchain.Head, "tail", subchain.Tail)
			}
			// Create a new subchain for the head (unless the last can be extended),
			// trimming anything it would overwrite
			headchain := &subchain{
				Head: number,
				Tail: number,
				Next: head.ParentHash,
			}
			for len(s.progress.Subchains) > 0 {
				// If the last chain is above the new head, delete altogether
				lastchain := s.progress.Subchains[0]
				if lastchain.Tail >= headchain.Tail {
					log.Debug("Dropping skeleton subchain", "head", lastchain.Head, "tail", lastchain.Tail)
					s.progress.Subchains = s.progress.Subchains[1:]
					continue
				}
				// Otherwise truncate the last chain if needed and abort trimming
				if lastchain.Head >= headchain.Tail {
					log.Debug("Trimming skeleton subchain", "oldhead", lastchain.Head, "newhead", headchain.Tail-1, "tail", lastchain.Tail)
					lastchain.Head = headchain.Tail - 1
				}
				break
			}
			// If the last subchain can be extended, we're lucky. Otherwise create
			// a new subchain sync task.
			var extended bool
			if n := len(s.progress.Subchains); n > 0 {
				lastchain := s.progress.Subchains[0]
				if lastchain.Head == headchain.Tail-1 {
					lasthead := rawdb.ReadSkeletonHeader(s.db, lastchain.Head)
					if lasthead.Hash() == head.ParentHash {
						log.Debug("Extended skeleton subchain with new head", "head", headchain.Tail, "tail", lastchain.Tail)
						lastchain.Head = headchain.Tail
						extended = true
					}
				}
			}
			if !extended {
				log.Debug("Created new skeleton subchain", "head", number, "tail", number)
				s.progress.Subchains = append([]*subchain{headchain}, s.progress.Subchains...)
			}
			// Update the database with the new sync stats and insert the new
			// head header. We won't delete any trimmed skeleton headers since
			// those will be outside the index space of the many subchains and
			// the database space will be reclaimed eventually when processing
			// blocks above the current head (TODO(karalabe): don't forget).
			batch := s.db.NewBatch()

			rawdb.WriteSkeletonHeader(batch, head)
			s.saveSyncStatus(batch)

			if err := batch.Write(); err != nil {
				log.Crit("Failed to write skeleton sync status", "err", err)
			}
			return
		}
	}
	// Either we've failed to decode the previus state, or there was none. Start
	// a fresh sync with a single subchain represented by the currently sent
	// chain head.
	s.progress = &skeletonProgress{
		Subchains: []*subchain{
			{
				Head: number,
				Tail: number,
				Next: head.ParentHash,
			},
		},
	}
	batch := s.db.NewBatch()

	rawdb.WriteSkeletonHeader(batch, head)
	s.saveSyncStatus(batch)

	if err := batch.Write(); err != nil {
		log.Crit("Failed to write initial skeleton sync status", "err", err)
	}
	log.Debug("Created initial skeleton subchain", "head", number, "tail", number)
}

// saveSyncStatus marshals the remaining sync tasks into leveldb.
func (s *skeleton) saveSyncStatus(db ethdb.KeyValueWriter) {
	status, err := json.Marshal(s.progress)
	if err != nil {
		panic(err) // This can only fail during implementation
	}
	rawdb.WriteSkeletonSyncStatus(db, status)
}

// processNewHead does the internal shuffling for a new head marker and either
// accepts and integrates it into the skeleton or requests a reorg. Upon reorg,
// the syncer will tear itself down and restart with a fresh head. It is simpler
// to reconstruct the sync state than to mutate it and hope for the best.
func (s *skeleton) processNewHead(head *types.Header) bool {
	// If the header cannot be inserted without interruption, return an error for
	// the outer loop to tear down the skeleton sync and restart it
	number := head.Number.Uint64()

	lastchain := s.progress.Subchains[0]
	if lastchain.Tail >= number {
		log.Warn("Beacon chain reorged", "tail", lastchain.Tail, "newHead", number)
		return true
	}
	if lastchain.Head+1 < number {
		log.Warn("Beacon chain gapped", "head", lastchain.Head, "newHead", number)
		return true
	}
	if parent := rawdb.ReadSkeletonHeader(s.db, number-1); parent.Hash() != head.ParentHash {
		log.Warn("Beacon chain forked", "ancestor", parent.Number, "hash", parent.Hash(), "want", head.ParentHash)
		return true
	}
	// New header seems to be in the last subchain range. Unwind any extra headers
	// from the chain tip and insert the new head. We won't delete any trimmed
	// skeleton headers since those will be outside the index space of the many
	// subchains and the database space will be reclaimed eventually when processing
	// blocks above the current head (TODO(karalabe): don't forget).
	batch := s.db.NewBatch()

	rawdb.WriteSkeletonHeader(batch, head)
	lastchain.Head = number
	s.saveSyncStatus(batch)

	if err := batch.Write(); err != nil {
		log.Crit("Failed to write skeleton sync status", "err", err)
	}
	return false
}

// assingTasks attempts to match idle peers to pending header retrievals.
func (s *skeleton) assingTasks(success chan *headerResponse, fail chan *headerRequest, cancel chan struct{}) {
	// Sort the peers by download capacity to use faster ones if many available
	idlers := &peerCapacitySort{
		peers: make([]*peerConnection, 0, len(s.idles)),
		caps:  make([]int, 0, len(s.idles)),
	}
	targetTTL := s.peers.rates.TargetTimeout()
	for _, peer := range s.idles {
		idlers.peers = append(idlers.peers, peer)
		idlers.caps = append(idlers.caps, s.peers.rates.Capacity(peer.id, eth.BlockHeadersMsg, targetTTL))
	}
	if len(idlers.peers) == 0 {
		return
	}
	sort.Sort(idlers)

	// Find header regions not yet downloading and fill them
	for task, owner := range s.scratchOwners {
		// If we're out of idle peers, stop assigning tasks
		if len(idlers.peers) == 0 {
			return
		}
		// Skip any tasks already filling
		if owner != "" {
			continue
		}
		// If we've reached the genesis, stop assigning tasks
		if uint64(task*requestHeaders) >= s.scratchHead {
			return
		}
		// Found a task and have peers available, assign it
		idle := idlers.peers[0]

		idlers.peers = idlers.peers[1:]
		idlers.caps = idlers.caps[1:]

		// Matched a pending task to an idle peer, allocate a unique request id
		var reqid uint64
		for {
			reqid = uint64(rand.Int63())
			if reqid == 0 {
				continue
			}
			if _, ok := s.requests[reqid]; ok {
				continue
			}
			break
		}
		// Generate the network query and send it to the peer
		req := &headerRequest{
			peer:    idle.id,
			id:      reqid,
			deliver: success,
			revert:  fail,
			cancel:  cancel,
			stale:   make(chan struct{}),
			head:    s.scratchHead - uint64(task*requestHeaders),
		}
		s.requests[reqid] = req
		delete(s.idles, idle.id)

		// Generate the network query and send it to the peer
		go s.executeTask(idle, req)

		// Inject the request into the task to block further assignments
		s.scratchOwners[task] = idle.id
	}
}

// executeTask executes a single fetch request, blocking until either a result
// arrives or a timeouts / cancellation is triggered. The method should be run
// on its own goroutine and will deliver on the requested channels.
func (s *skeleton) executeTask(peer *peerConnection, req *headerRequest) {
	start := time.Now()
	resCh := make(chan *eth.Response)

	// Figure out how many headers to fetch. Usually this will be a full batch,
	// but for the very tail of the chain, trim the request to the number left.
	// Since nodes may or may not return the genesis header for a batch request,
	// don't even request it. The parent hash of block #1 is enough to link.
	requestCount := requestHeaders
	if req.head < requestHeaders {
		requestCount = int(req.head)
	}
	peer.log.Trace("Fetching skeleton headers", "from", req.head, "count", requestCount)
	netreq, err := peer.peer.RequestHeadersByNumber(req.head, requestCount, 0, true, resCh)
	if err != nil {
		peer.log.Trace("Failed to request headers", "err", err)
		s.scheduleRevertRequest(req)
		return
	}
	defer netreq.Close()

	// Wait until the response arrives, the request is cancelled or times out
	ttl := s.peers.rates.TargetTimeout()

	timeoutTimer := time.NewTimer(ttl)
	defer timeoutTimer.Stop()

	select {
	case <-req.cancel:
		peer.log.Debug("Header request cancelled")
		s.scheduleRevertRequest(req)

	case <-timeoutTimer.C:
		// Header retrieval timed out, update the metrics
		peer.log.Trace("Header request timed out", "elapsed", ttl)
		headerTimeoutMeter.Mark(1)
		s.peers.rates.Update(peer.id, eth.BlockHeadersMsg, 0, 0)
		s.scheduleRevertRequest(req)

	case res := <-resCh:
		// Headers successfully retrieved, update the metrics
		headers := *res.Res.(*eth.BlockHeadersPacket)

		headerReqTimer.Update(time.Since(start))
		s.peers.rates.Update(peer.id, eth.BlockHeadersMsg, res.Time, len(headers))

		// Cross validate the headers with the requests
		switch {
		case len(headers) == 0:
			// No headers were delivered, reject the response and reschedule
			peer.log.Debug("No headers delivered")
			res.Done <- errors.New("no headers delivered")
			s.scheduleRevertRequest(req)

		case headers[0].Number.Uint64() != req.head:
			// Header batch anchored at non-requested number
			peer.log.Debug("Invalid header response head", "have", headers[0].Number, "want", req.head)
			res.Done <- errors.New("invalid header batch anchor")
			s.scheduleRevertRequest(req)

		case headers[0].Number.Uint64() >= requestHeaders && len(headers) != requestHeaders:
			// Invalid number of non-genesis headers delivered, reject the response and reschedule
			peer.log.Debug("Invalid non-genesis header count", "have", len(headers), "want", requestHeaders)
			res.Done <- errors.New("not enough non-genesis headers delivered")
			s.scheduleRevertRequest(req)

		case headers[0].Number.Uint64() < requestHeaders && uint64(len(headers)) != headers[0].Number.Uint64():
			// Invalid number of genesis headers delivered, reject the response and reschedule
			peer.log.Debug("Invalid genesis header count", "have", len(headers), "want", headers[0].Number.Uint64())
			res.Done <- errors.New("not enough genesis headers delivered")
			s.scheduleRevertRequest(req)

		default:
			// Packet seems structurally valid, check hash progression and if it
			// is correct too, deliver for storage
			for i := 0; i < len(headers)-1; i++ {
				if headers[i].ParentHash != headers[i+1].Hash() {
					peer.log.Debug("Invalid genesis header count", "have", len(headers), "want", headers[0].Number.Uint64())
					res.Done <- errors.New("not enough genesis headers delivered")
					s.scheduleRevertRequest(req)
					return
				}
			}
			// Hash chain is valid. The delivery might still be junk as we're
			// downloading batches concurrently (so no way to link the headers
			// until gaps are filled); in that case, we'll nuke the peer when
			// we detect the fault.
			res.Done <- nil

			select {
			case req.deliver <- &headerResponse{
				peer:    peer,
				reqid:   req.id,
				headers: headers,
			}:
			case <-req.cancel:
			}
		}
	}
}

// revertRequests locates all the currently pending reuqests from a particular
// peer and reverts them, rescheduling for others to fulfill.
func (s *skeleton) revertRequests(peer string) {
	// Gather the requests first, revertals need the lock too
	var requests []*headerRequest
	for _, req := range s.requests {
		if req.peer == peer {
			requests = append(requests, req)
		}
	}
	// Revert all the requests matching the peer
	for _, req := range requests {
		s.revertRequest(req)
	}
}

// scheduleRevertRequest asks the event loop to clean up a request and return
// all failed retrieval tasks to the scheduler for reassignment.
func (s *skeleton) scheduleRevertRequest(req *headerRequest) {
	select {
	case req.revert <- req:
		// Sync event loop notified
	case <-req.cancel:
		// Sync cycle got cancelled
	case <-req.stale:
		// Request already reverted
	}
}

// revertRequest cleans up a request and returns all failed retrieval tasks to
// the scheduler for reassignment.
//
// Note, this needs to run on the event runloop thread to reschedule to idle peers.
// On peer threads, use scheduleRevertRequest.
func (s *skeleton) revertRequest(req *headerRequest) {
	log.Trace("Reverting header request", "peer", req.peer, "reqid", req.id)
	select {
	case <-req.stale:
		log.Trace("Header request already reverted", "peer", req.peer, "reqid", req.id)
		return
	default:
	}
	close(req.stale)

	// Remove the request from the tracked set
	delete(s.requests, req.id)

	// Remove the request from the tracked set and mark the task as not-pending,
	// ready for resheduling
	s.scratchOwners[(s.scratchHead-req.head)/requestHeaders] = ""
}

func (s *skeleton) processResponse(res *headerResponse) bool {
	res.peer.log.Trace("Processing header response", "head", res.headers[0].Number, "hash", res.headers[0].Hash(), "count", len(res.headers))

	// Whether or not the response is valid, we can mark the peer as idle and
	// notify the scheduler to assign a new task. If the response is invalid,
	// we'll drop the peer in a bit.
	s.idles[res.peer.id] = res.peer

	// Ensure the response is for a valid request
	if _, ok := s.requests[res.reqid]; !ok {
		// Request stale, perhaps the peer timed out but came through in the end
		res.peer.log.Warn("Unexpected header packet")
		return false
	}
	delete(s.requests, res.reqid)

	// Insert the delivered headers into the scratch space independent of the
	// content or continuation; those will be validated in a moment
	head := res.headers[0].Number.Uint64()
	copy(s.scratchSpace[s.scratchHead-head:], res.headers)

	// If there's still a gap in the head of the scratch space, abort
	if s.scratchSpace[0] == nil {
		return false
	}
	// Try to consume any head headers, validating the boundary conditions
	var merged bool // Whether subchains were merged

	batch := s.db.NewBatch()
	for s.scratchSpace[0] != nil {
		// Next batch of headers available, cross-reference with the subchain
		// we are extending and either accept or discard
		if s.progress.Subchains[0].Next != s.scratchSpace[0].Hash() {
			// Print a log messages to track what's going on
			tail := s.progress.Subchains[0].Tail
			want := s.progress.Subchains[0].Next
			have := s.scratchSpace[0].Hash()

			log.Warn("Invalid skeleton headers", "peer", s.scratchOwners[0], "number", tail-1, "want", want, "have", have)

			// The peer delivered junk, or at least not the subchain we are
			// syncing to. Free up the scratch space and assignment, reassign
			// and drop the original peer.
			for i := 0; i < requestHeaders; i++ {
				s.scratchSpace[i] = nil
			}
			s.drop(s.scratchOwners[0])
			s.scratchOwners[0] = ""
			break
		}
		// Scratch delivery matches required subchain, deliver the batch of
		// headers and push the subchain forward
		var consumed int
		for _, header := range s.scratchSpace[:requestHeaders] {
			if header != nil { // nil when the genesis is reached
				consumed++

				rawdb.WriteSkeletonHeader(batch, header)
				s.pulled++

				s.progress.Subchains[0].Tail--
				s.progress.Subchains[0].Next = header.ParentHash
			}
		}
		// Batch of headers consumed, shift the download window forward
		head := s.progress.Subchains[0].Head
		tail := s.progress.Subchains[0].Tail
		next := s.progress.Subchains[0].Next

		log.Trace("Primary subchain extended", "head", head, "tail", tail, "next", next)

		copy(s.scratchSpace, s.scratchSpace[requestHeaders:])
		for i := 0; i < requestHeaders; i++ {
			s.scratchSpace[scratchHeaders-i-1] = nil
		}
		copy(s.scratchOwners, s.scratchOwners[1:])
		s.scratchOwners[scratchHeaders/requestHeaders-1] = ""

		s.scratchHead -= uint64(consumed)

		// If the subchain extended into the next subchain, we need to handle
		// the overlap. Since there could be many overlaps (come on), do this
		// in a loop.
		for len(s.progress.Subchains) > 1 && s.progress.Subchains[1].Head >= s.progress.Subchains[0].Tail {
			// Extract some stats from the second subchain
			head := s.progress.Subchains[1].Head
			tail := s.progress.Subchains[1].Tail
			next := s.progress.Subchains[1].Next

			// Since we just overwrote part of the next subchain, we need to trim
			// its head independent of matching or mismatching content
			if s.progress.Subchains[1].Tail >= s.progress.Subchains[0].Tail {
				// Fully overwritten, get rid of the subchain as a whole
				log.Debug("Previous subchain fully overwritten", "head", head, "tail", tail, "next", next)
				s.progress.Subchains = append(s.progress.Subchains[:1], s.progress.Subchains[2:]...)
				continue
			} else {
				// Partially overwritten, trim the head to the overwritten size
				log.Debug("Previous subchain partially overwritten", "head", head, "tail", tail, "next", next)
				s.progress.Subchains[1].Head = s.progress.Subchains[0].Tail - 1
			}
			// If the old subchain is an extension of the new one, merge the two
			// and let the skeleton syncer restart (to clean internal state)
			if rawdb.ReadSkeletonHeader(s.db, s.progress.Subchains[1].Head).Hash() == s.progress.Subchains[0].Next {
				log.Debug("Previous subchain merged", "head", head, "tail", tail, "next", next)
				s.progress.Subchains[0].Tail = s.progress.Subchains[1].Tail
				s.progress.Subchains[0].Next = s.progress.Subchains[1].Next

				s.progress.Subchains = append(s.progress.Subchains[:1], s.progress.Subchains[2:]...)
				merged = true
			}
		}
	}
	s.saveSyncStatus(batch)
	if err := batch.Write(); err != nil {
		log.Crit("Failed to write skeleton headers and progress", "err", err)
	}
	// Print a progress report to make the UX a bit nicer
	left := s.progress.Subchains[0].Tail - 1
	if time.Since(s.logged) > 8*time.Second || left == 0 {
		s.logged = time.Now()

		if s.pulled == 0 {
			log.Info("Beacon sync starting", "left", left)
		} else {
			eta := float64(time.Since(s.started)) / float64(s.pulled) * float64(left)
			log.Info("Syncing beacon headers", "downloaded", s.pulled, "left", left, "eta", common.PrettyDuration(eta))
		}
	}
	return merged
}

// Head retrieves the current head tracked by the skeleton syncer. This method
// is meant to be used by the backfiller, whose life cycle is controlled by the
// skeleton syncer.
//
// Note, the method will not use the internal state of the skeleton, but will
// rather blindly pull stuff from the database. This is fine, because the back-
// filler will only run when the skeleton chain is fully downloaded and stable.
// There might be new heads appended, but those are stomic from the perspective
// of this method. Any head reorg will first tear down the backfiller and only
// then make the modification.
func (s *skeleton) Head() (*types.Header, error) {
	// Read the current sync progress from disk and figure out the current head.
	// Although there's a lot of error handling here, these are mostly as sanity
	// checks to avoid crashing if a programming error happens. These should not
	// happen in live code.
	status := rawdb.ReadSkeletonSyncStatus(s.db)
	if len(status) == 0 {
		return nil, errors.New("beacon sync not yet started")
	}
	progress := new(skeletonProgress)
	if err := json.Unmarshal(status, progress); err != nil {
		return nil, err
	}
	if progress.Subchains[0].Tail != 1 {
		return nil, errors.New("beacon sync not yet finished")
	}
	return rawdb.ReadSkeletonHeader(s.db, progress.Subchains[0].Head), nil
}

// Header retrieves a specific header tracked by the skeleton syncer. This method
// is meant to be used by the backfiller, whose life cycle is controlled by the
// skeleton syncer.
//
// Note, outside the permitted runtimes, this method might return nil results and
// subsequent calls might return headers from different chains.
func (s *skeleton) Header(number uint64) *types.Header {
	return rawdb.ReadSkeletonHeader(s.db, number)
}
