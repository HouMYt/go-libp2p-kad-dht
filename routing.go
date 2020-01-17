package dht

import (
	"bytes"
	"context"
	"fmt"
	"github.com/libp2p/go-libp2p-core/network"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/peerstore"
	"github.com/libp2p/go-libp2p-core/routing"

	"github.com/ipfs/go-cid"
	u "github.com/ipfs/go-ipfs-util"
	logging "github.com/ipfs/go-log"
	"github.com/libp2p/go-libp2p-kad-dht/kpeerset"
	pb "github.com/libp2p/go-libp2p-kad-dht/pb"
	kb "github.com/libp2p/go-libp2p-kbucket"
	record "github.com/libp2p/go-libp2p-record"
	"github.com/multiformats/go-multihash"
)

// asyncQueryBuffer is the size of buffered channels in async queries. This
// buffer allows multiple queries to execute simultaneously, return their
// results and continue querying closer peers. Note that different query
// results will wait for the channel to drain.
var asyncQueryBuffer = 10

// This file implements the Routing interface for the IpfsDHT struct.

// Basic Put/Get

// PutValue adds value corresponding to given Key.
// This is the top level "Store" operation of the DHT
func (dht *IpfsDHT) PutValue(ctx context.Context, key string, value []byte, opts ...routing.Option) (err error) {
	if !dht.enableValues {
		return routing.ErrNotSupported
	}

	eip := logger.EventBegin(ctx, "PutValue")
	defer func() {
		eip.Append(loggableKey(key))
		if err != nil {
			eip.SetError(err)
		}
		eip.Done()
	}()
	logger.Debugf("PutValue %s", key)

	// don't even allow local users to put bad values.
	if err := dht.Validator.Validate(key, value); err != nil {
		return err
	}

	old, err := dht.getLocal(key)
	if err != nil {
		// Means something is wrong with the datastore.
		return err
	}

	// Check if we have an old value that's not the same as the new one.
	if old != nil && !bytes.Equal(old.GetValue(), value) {
		// Check to see if the new one is better.
		i, err := dht.Validator.Select(key, [][]byte{value, old.GetValue()})
		if err != nil {
			return err
		}
		if i != 0 {
			return fmt.Errorf("can't replace a newer value with an older value")
		}
	}

	rec := record.MakePutRecord(key, value)
	rec.TimeReceived = u.FormatRFC3339(time.Now())
	err = dht.putLocal(key, rec)
	if err != nil {
		return err
	}

	pchan, err := dht.GetClosestPeers(ctx, key)
	if err != nil {
		return err
	}

	wg := sync.WaitGroup{}
	for p := range pchan {
		wg.Add(1)
		go func(p peer.ID) {
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			defer wg.Done()
			routing.PublishQueryEvent(ctx, &routing.QueryEvent{
				Type: routing.Value,
				ID:   p,
			})

			err := dht.putValueToPeer(ctx, p, rec)
			if err != nil {
				logger.Debugf("failed putting value to peer: %s", err)
			}
		}(p)
	}
	wg.Wait()

	return nil
}

// RecvdVal stores a value and the peer from which we got the value.
type RecvdVal struct {
	Val  []byte
	From peer.ID
}

// GetValue searches for the value corresponding to given Key.
func (dht *IpfsDHT) GetValue(ctx context.Context, key string, opts ...routing.Option) (_ []byte, err error) {
	if !dht.enableValues {
		return nil, routing.ErrNotSupported
	}

	eip := logger.EventBegin(ctx, "GetValue")
	defer func() {
		eip.Append(loggableKey(key))
		if err != nil {
			eip.SetError(err)
		}
		eip.Done()
	}()

	// apply defaultQuorum if relevant
	var cfg routing.Options
	if err := cfg.Apply(opts...); err != nil {
		return nil, err
	}
	opts = append(opts, Quorum(getQuorum(&cfg, defaultQuorum)))

	responses, err := dht.SearchValue(ctx, key, opts...)
	if err != nil {
		return nil, err
	}
	var best []byte

	for r := range responses {
		best = r
	}

	if ctx.Err() != nil {
		return best, ctx.Err()
	}

	if best == nil {
		return nil, routing.ErrNotFound
	}
	logger.Debugf("GetValue %v %v", key, best)
	return best, nil
}

func (dht *IpfsDHT) SearchValue(ctx context.Context, key string, opts ...routing.Option) (<-chan []byte, error) {
	if !dht.enableValues {
		return nil, routing.ErrNotSupported
	}

	var cfg routing.Options
	if err := cfg.Apply(opts...); err != nil {
		return nil, err
	}

	responsesNeeded := 0
	if !cfg.Offline {
		responsesNeeded = getQuorum(&cfg, 0)
	}

	valCh := dht.getValues(ctx, key, responsesNeeded)

	out := make(chan []byte)
	go func() {
		defer close(out)

		maxVals := responsesNeeded
		if maxVals < 0 {
			maxVals = defaultQuorum * 4 // we want some upper bound on how
			// much correctional entries we will send
		}

		// vals is used collect entries we got so far and send corrections to peers
		// when we exit this function
		vals := make([]RecvdVal, 0, maxVals)
		var best *RecvdVal

		defer func() {
			if len(vals) <= 1 || best == nil {
				return
			}
			fixupRec := record.MakePutRecord(key, best.Val)

			correctingEntries := make(map[peer.ID]RecvdVal)
			correctingPeers := make([]peer.ID, 0, len(vals))

			for _, v := range vals {
				// if someone sent us a different 'less-valid' record, lets correct them
				if !bytes.Equal(v.Val, best.Val) {
					correctingEntries[v.From] = v
					correctingPeers = append(correctingPeers, v.From)
				}
			}

			// only correct the peers closest to the target
			correctingPeers = kb.SortClosestPeers(correctingPeers, kb.ConvertKey(key))
			if numCorrectingPeers := len(correctingPeers); maxVals > numCorrectingPeers {
				maxVals = numCorrectingPeers
			}

			for _, p := range correctingPeers[:maxVals] {
				go func(v RecvdVal) {
					if v.From == dht.self {
						err := dht.putLocal(key, fixupRec)
						if err != nil {
							logger.Error("Error correcting local dht entry:", err)
						}
						return
					}
					ctx, cancel := context.WithTimeout(dht.Context(), time.Second*30)
					defer cancel()
					err := dht.putValueToPeer(ctx, v.From, fixupRec)
					if err != nil {
						logger.Debug("Error correcting DHT entry: ", err)
					}
				}(correctingEntries[p])
			}
		}()

		for {
			select {
			case v, ok := <-valCh:
				if !ok {
					return
				}

				vals = append(vals, v)

				if v.Val == nil {
					continue
				}
				// Select best value
				if best != nil {
					if bytes.Equal(best.Val, v.Val) {
						continue
					}
					sel, err := dht.Validator.Select(key, [][]byte{best.Val, v.Val})
					if err != nil {
						logger.Warning("Failed to select dht key: ", err)
						continue
					}
					if sel != 1 {
						continue
					}
				}
				best = &v
				select {
				case out <- v.Val:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, nil
}

// GetValues gets nvals values corresponding to the given key.
func (dht *IpfsDHT) GetValues(ctx context.Context, key string, nvals int) (_ []RecvdVal, err error) {
	if !dht.enableValues {
		return nil, routing.ErrNotSupported
	}

	eip := logger.EventBegin(ctx, "GetValues")
	eip.Append(loggableKey(key))
	defer eip.Done()

	valCh := dht.getValues(ctx, key, nvals)

	out := make([]RecvdVal, 0, nvals)
	for val := range valCh {
		out = append(out, val)
	}

	return out, ctx.Err()
}

func (dht *IpfsDHT) getValues(ctx context.Context, key string, nvals int) <-chan RecvdVal {
	valCh := make(chan RecvdVal, 1)

	valSignal := make(chan struct{}, 1)

	valsSent := 0
	valsSentMx := sync.RWMutex{}

	vbuf := make([]RecvdVal, 0, 32)
	vbufMx := sync.Mutex{}

	valEmitCtx, cancelValEmit := context.WithCancel(ctx)
	go func() {
		defer close(valCh)

		for {
			var next RecvdVal

			trySetNext := func() bool {
				vbufMx.Lock()
				notEmpty := len(vbuf) > 0
				if notEmpty {
					next = vbuf[0]
					vbuf = vbuf[1:]
				}
				vbufMx.Unlock()
				return notEmpty
			}

			if !trySetNext() {
			signal:
				for {
					select {
					case <-valSignal:
						if trySetNext() {
							break signal
						}
					case <-valEmitCtx.Done():
						if !trySetNext() {
							return
						}
						for {
							select {
							case valCh <- next:
								valsSentMx.Lock()
								valsSent++
								valsSentMx.Unlock()

								if !trySetNext() {
									return
								}
							default:
								return
							}
						}
					}
				}
			}

		send:
			for {
				select {
				case valCh <- next:
					valsSentMx.Lock()
					valsSent++
					valsSentMx.Unlock()

					if !trySetNext() {
						break send
					}
				case <-valEmitCtx.Done():
					break send
				}
			}
		}
	}()

	go func() {
		queries := dht.runDisjointQueries(ctx, dht.d, key,
			func(ctx context.Context, p peer.ID) ([]*peer.AddrInfo, error) {
				// For DHT query command
				routing.PublishQueryEvent(ctx, &routing.QueryEvent{
					Type: routing.SendingQuery,
					ID:   p,
				})

				rec, peers, err := dht.getValueOrPeers(ctx, p, key)
				switch err {
				case routing.ErrNotFound:
					// in this case, they responded with nothing,
					// still send a notification so listeners can know the
					// request has completed 'successfully'
					routing.PublishQueryEvent(ctx, &routing.QueryEvent{
						Type: routing.PeerResponse,
						ID:   p,
					})
					return nil, err
				default:
					return nil, err
				case nil, errInvalidRecord:
					// in either of these cases, we want to keep going
				}

				// TODO: What should happen if the record is invalid?
				// Pre-existing code counted it towards the quorum, but should it?
				if rec != nil && rec.GetValue() != nil {
					rv := RecvdVal{
						Val:  rec.GetValue(),
						From: p,
					}

					vbufMx.Lock()
					vbuf = append(vbuf, rv)
					vbufMx.Unlock()
					select {
					case valSignal <- struct{}{}:
					default:
					}
				}

				// For DHT query command
				routing.PublishQueryEvent(ctx, &routing.QueryEvent{
					Type:      routing.PeerResponse,
					ID:        p,
					Responses: peers,
				})

				return peers, err
			},
			func(peerset *kpeerset.SortedPeerset) bool {
				if nvals <= 0 {
					return false
				}
				valsSentMx.RLock()
				defer valsSentMx.RUnlock()
				return valsSent >= nvals
			},
		)

		cancelValEmit()

		shortcutTaken := false
		for _, q := range queries {
			if len(q.localPeers.KUnqueried()) > 0 {
				shortcutTaken = true
				break
			}
		}

		if !shortcutTaken {
			kadID := kb.ConvertKey(key)
			// refresh the cpl for this key as the query was successful
			dht.routingTable.ResetCplRefreshedAtForID(kadID, time.Now())
		}
	}()

	return valCh
}

// Provider abstraction for indirect stores.
// Some DHTs store values directly, while an indirect store stores pointers to
// locations of the value, similarly to Coral and Mainline DHT.

// Provide makes this node announce that it can provide a value for the given key
func (dht *IpfsDHT) Provide(ctx context.Context, key cid.Cid, brdcst bool) (err error) {
	if !dht.enableProviders {
		return routing.ErrNotSupported
	}
	keyMH := key.Hash()
	eip := logger.EventBegin(ctx, "Provide", multihashLoggableKey(keyMH), logging.LoggableMap{"broadcast": brdcst})
	defer func() {
		if err != nil {
			eip.SetError(err)
		}
		eip.Done()
	}()

	// add self locally
	dht.ProviderManager.AddProvider(ctx, keyMH, dht.self)
	if !brdcst {
		return nil
	}

	closerCtx := ctx
	if deadline, ok := ctx.Deadline(); ok {
		now := time.Now()
		timeout := deadline.Sub(now)

		if timeout < 0 {
			// timed out
			return context.DeadlineExceeded
		} else if timeout < 10*time.Second {
			// Reserve 10% for the final put.
			deadline = deadline.Add(-timeout / 10)
		} else {
			// Otherwise, reserve a second (we'll already be
			// connected so this should be fast).
			deadline = deadline.Add(-time.Second)
		}
		var cancel context.CancelFunc
		closerCtx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}

	peers, err := dht.GetClosestPeers(closerCtx, string(keyMH))
	if err != nil {
		return err
	}

	mes, err := dht.makeProvRecord(keyMH)
	if err != nil {
		return err
	}

	wg := sync.WaitGroup{}
	for p := range peers {
		wg.Add(1)
		go func(p peer.ID) {
			defer wg.Done()
			logger.Debugf("putProvider(%s, %s)", keyMH, p)
			err := dht.sendMessage(ctx, p, mes)
			if err != nil {
				logger.Debug(err)
			}
		}(p)
	}
	wg.Wait()
	return nil
}
func (dht *IpfsDHT) makeProvRecord(key []byte) (*pb.Message, error) {
	pi := peer.AddrInfo{
		ID:    dht.self,
		Addrs: dht.host.Addrs(),
	}

	// // only share WAN-friendly addresses ??
	// pi.Addrs = addrutil.WANShareableAddrs(pi.Addrs)
	if len(pi.Addrs) < 1 {
		return nil, fmt.Errorf("no known addresses for self. cannot put provider.")
	}

	pmes := pb.NewMessage(pb.Message_ADD_PROVIDER, key, 0)
	pmes.ProviderPeers = pb.RawPeerInfosToPBPeers([]peer.AddrInfo{pi})
	return pmes, nil
}

// FindProviders searches until the context expires.
func (dht *IpfsDHT) FindProviders(ctx context.Context, c cid.Cid) ([]peer.AddrInfo, error) {
	if !dht.enableProviders {
		return nil, routing.ErrNotSupported
	}
	var providers []peer.AddrInfo
	for p := range dht.FindProvidersAsync(ctx, c, dht.bucketSize) {
		providers = append(providers, p)
	}
	return providers, nil
}

// FindProvidersAsync is the same thing as FindProviders, but returns a channel.
// Peers will be returned on the channel as soon as they are found, even before
// the search query completes.
func (dht *IpfsDHT) FindProvidersAsync(ctx context.Context, key cid.Cid, count int) <-chan peer.AddrInfo {
	peerOut := make(chan peer.AddrInfo, count)
	if !dht.enableProviders {
		close(peerOut)
		return peerOut
	}

	keyMH := key.Hash()
	logger.Event(ctx, "findProviders", multihashLoggableKey(keyMH))

	go dht.findProvidersAsyncRoutine(ctx, keyMH, count, peerOut)
	return peerOut
}

func (dht *IpfsDHT) findProvidersAsyncRoutine(ctx context.Context, key multihash.Multihash, count int, peerOut chan peer.AddrInfo) {
	defer logger.EventBegin(ctx, "findProvidersAsync", multihashLoggableKey(key)).Done()
	defer close(peerOut)

	ps := peer.NewLimitedSet(count)
	provs := dht.ProviderManager.GetProviders(ctx, key)
	for _, p := range provs {
		// NOTE: Assuming that this list of peers is unique
		if ps.TryAdd(p) {
			pi := dht.peerstore.PeerInfo(p)
			select {
			case peerOut <- pi:
			case <-ctx.Done():
				return
			}
		}

		// If we have enough peers locally, don't bother with remote RPC
		// TODO: is this a DOS vector?
		if ps.Size() >= count {
			return
		}
	}

	dht.runDisjointQueries(ctx, dht.d, string(key),
		func(ctx context.Context, p peer.ID) ([]*peer.AddrInfo, error) {
			// For DHT query command
			routing.PublishQueryEvent(ctx, &routing.QueryEvent{
				Type: routing.SendingQuery,
				ID:   p,
			})

			pmes, err := dht.findProvidersSingle(ctx, p, key)
			if err != nil {
				return nil, err
			}

			logger.Debugf("%d provider entries", len(pmes.GetProviderPeers()))
			provs := pb.PBPeersToPeerInfos(pmes.GetProviderPeers())
			logger.Debugf("%d provider entries decoded", len(provs))

			// Add unique providers from request, up to 'count'
			for _, prov := range provs {
				if prov.ID != dht.self {
					dht.peerstore.AddAddrs(prov.ID, prov.Addrs, peerstore.TempAddrTTL)
				}
				logger.Debugf("got provider: %s", prov)
				if ps.TryAdd(prov.ID) {
					logger.Debugf("using provider: %s", prov)
					select {
					case peerOut <- *prov:
					case <-ctx.Done():
						logger.Debug("context timed out sending more providers")
						return nil, ctx.Err()
					}
				}
				if ps.Size() >= count {
					logger.Debugf("got enough providers (%d/%d)", ps.Size(), count)
					return nil, nil
				}
			}

			// Give closer peers back to the query to be queried
			closer := pmes.GetCloserPeers()
			peers := pb.PBPeersToPeerInfos(closer)
			logger.Debugf("got closer peers: %d %s", len(peers), peers)

			routing.PublishQueryEvent(ctx, &routing.QueryEvent{
				Type:      routing.PeerResponse,
				ID:        p,
				Responses: peers,
			})

			return peers, nil
		},
		func(peerset *kpeerset.SortedPeerset) bool {
			return ps.Size() > count
		},
	)

	if ctx.Err() == nil {
		// refresh the cpl for this key after the query is run
		dht.routingTable.ResetCplRefreshedAtForID(kb.ConvertKey(string(key)), time.Now())
	}
}

// FindPeer searches for a peer with given ID.
func (dht *IpfsDHT) FindPeer(ctx context.Context, id peer.ID) (_ peer.AddrInfo, err error) {
	eip := logger.EventBegin(ctx, "FindPeer", id)
	defer func() {
		if err != nil {
			eip.SetError(err)
		}
		eip.Done()
	}()

	// Check if were already connected to them
	if pi := dht.FindLocal(id); pi.ID != "" {
		return pi, nil
	}

	peers := dht.routingTable.NearestPeers(kb.ConvertPeerID(id), AlphaValue)
	if len(peers) == 0 {
		return peer.AddrInfo{}, kb.ErrLookupFailure
	}

	// Sanity...
	for _, p := range peers {
		if p == id {
			logger.Debug("found target peer in list of closest peers...")
			return dht.peerstore.PeerInfo(p), nil
		}
	}

	queries := dht.runDisjointQueries(ctx, dht.d, string(id),
		func(ctx context.Context, p peer.ID) ([]*peer.AddrInfo, error) {
			// For DHT query command
			routing.PublishQueryEvent(ctx, &routing.QueryEvent{
				Type: routing.SendingQuery,
				ID:   p,
			})

			pmes, err := dht.findPeerSingle(ctx, p, id)
			if err != nil {
				logger.Debugf("error getting closer peers: %s", err)
				return nil, err
			}
			peers := pb.PBPeersToPeerInfos(pmes.GetCloserPeers())

			// For DHT query command
			routing.PublishQueryEvent(ctx, &routing.QueryEvent{
				Type:      routing.PeerResponse,
				ID:        p,
				Responses: peers,
			})

			return peers, err
		},
		func(peerset *kpeerset.SortedPeerset) bool {
			return dht.host.Network().Connectedness(id) == network.Connected
		},
	)

	//	logger.Debugf("FindPeer %v %v", id, result.success)

	if dht.host.Network().Connectedness(id) == network.Connected {
		shortcutTaken := false
		for _, q := range queries {
			if len(q.localPeers.KUnqueried()) > 0 {
				shortcutTaken = true
				break
			}
		}

		if !shortcutTaken {
			kadID := kb.ConvertPeerID(id)
			// refresh the cpl for this key as the query was successful
			dht.routingTable.ResetCplRefreshedAtForID(kadID, time.Now())
		}

		return dht.peerstore.PeerInfo(id), nil
	} else {
		if ctx.Err() != nil {
			kadID := kb.ConvertPeerID(id)
			// refresh the cpl for this key as the query was successful
			dht.routingTable.ResetCplRefreshedAtForID(kadID, time.Now())
		}

		return peer.AddrInfo{}, routing.ErrNotFound
	}
}
