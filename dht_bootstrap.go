package dht

import (
	"context"
	"time"

	process "github.com/jbenet/goprocess"
	processctx "github.com/jbenet/goprocess/context"
	"github.com/libp2p/go-libp2p-core/routing"
	"github.com/multiformats/go-multiaddr"
	_ "github.com/multiformats/go-multiaddr-dns"
)

var DefaultBootstrapPeers []multiaddr.Multiaddr

// Minimum number of peers in the routing table. If we drop below this and we
// see a new peer, we trigger a bootstrap round.
var minRTRefreshThreshold = 4

func init() {
	for _, s := range []string{
		"/dnsaddr/bootstrap.libp2p.io/ipfs/QmNnooDu7bfjPFoTZYxMNLWUQJyrVwtbZg5gBMjTezGAJN",
		"/dnsaddr/bootstrap.libp2p.io/ipfs/QmQCU2EcMqAqQPR2i9bChDtGNJchTbq5TbXJJ16u19uLTa",
		"/dnsaddr/bootstrap.libp2p.io/ipfs/QmbLHAnMoJPWSCR5Zhtx6BHJX9KiKNN6tpvbUcqanj75Nb",
		"/dnsaddr/bootstrap.libp2p.io/ipfs/QmcZf59bWwK5XFi76CZX8cbJ4BhTzzA3gU1ZjYZcYW3dwt",
		"/ip4/104.131.131.82/tcp/4001/ipfs/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ",            // mars.i.ipfs.io
		"/ip4/104.236.179.241/tcp/4001/ipfs/QmSoLPppuBtQSGwKDZT2M73ULpjvfd3aZ6ha4oFGL1KrGM",           // pluto.i.ipfs.io
		"/ip4/128.199.219.111/tcp/4001/ipfs/QmSoLSafTMBsPKadTEgaXctDQVcqN88CNLHXMkTNwMKPnu",           // saturn.i.ipfs.io
		"/ip4/104.236.76.40/tcp/4001/ipfs/QmSoLV4Bbm51jM9C4gDYZQ9Cy3U6aXMJDAbzgu2fzaDs64",             // venus.i.ipfs.io
		"/ip4/178.62.158.247/tcp/4001/ipfs/QmSoLer265NRgSp2LA3dPaeykiS1J6DifTC88f5uVQKNAd",            // earth.i.ipfs.io
		"/ip6/2604:a880:1:20::203:d001/tcp/4001/ipfs/QmSoLPppuBtQSGwKDZT2M73ULpjvfd3aZ6ha4oFGL1KrGM",  // pluto.i.ipfs.io
		"/ip6/2400:6180:0:d0::151:6001/tcp/4001/ipfs/QmSoLSafTMBsPKadTEgaXctDQVcqN88CNLHXMkTNwMKPnu",  // saturn.i.ipfs.io
		"/ip6/2604:a880:800:10::4a:5001/tcp/4001/ipfs/QmSoLV4Bbm51jM9C4gDYZQ9Cy3U6aXMJDAbzgu2fzaDs64", // venus.i.ipfs.io
		"/ip6/2a03:b0c0:0:1010::23:1001/tcp/4001/ipfs/QmSoLer265NRgSp2LA3dPaeykiS1J6DifTC88f5uVQKNAd", // earth.i.ipfs.io
	} {
		ma, err := multiaddr.NewMultiaddr(s)
		if err != nil {
			panic(err)
		}
		DefaultBootstrapPeers = append(DefaultBootstrapPeers, ma)
	}
}

// Start the refresh worker.
func (dht *IpfsDHT) startRefreshing() error {
	// scan the RT table periodically & do a random walk on k-buckets that haven't been queried since the given bucket period
	dht.proc.Go(func(proc process.Process) {
		ctx := processctx.OnClosingContext(proc)

		refreshTicker := time.NewTicker(dht.rtRefreshPeriod)
		defer refreshTicker.Stop()

		// refresh if option is set
		if dht.autoRefresh {
			dht.doRefresh(ctx)
		} else {
			// disable the "auto-refresh" ticker so that no more ticks are sent to this channel
			refreshTicker.Stop()
		}

		for {
			select {
			case <-refreshTicker.C:
			case <-dht.triggerRtRefresh:
				logger.Infof("triggering a refresh: RT has %d peers", dht.routingTable.Size())
			case <-ctx.Done():
				return
			}
			dht.doRefresh(ctx)
		}
	})

	return nil
}

func (dht *IpfsDHT) doRefresh(ctx context.Context) {
	dht.selfWalk(ctx)
	dht.refreshBuckets(ctx)
}

// refreshBuckets scans the routing table, and does a random walk on k-buckets that haven't been queried since the given bucket period
func (dht *IpfsDHT) refreshBuckets(ctx context.Context) {
	doQuery := func(bucketId int, target string, f func(context.Context) error) error {
		logger.Infof("starting refreshing bucket %d to %s (routing table size was %d)",
			bucketId, target, dht.routingTable.Size())
		defer func() {
			logger.Infof("finished refreshing bucket %d to %s (routing table size is now %d)",
				bucketId, target, dht.routingTable.Size())
		}()
		queryCtx, cancel := context.WithTimeout(ctx, dht.rtRefreshQueryTimeout)
		defer cancel()
		err := f(queryCtx)
		if err == context.DeadlineExceeded && queryCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			return nil
		}
		return err
	}

	buckets := dht.routingTable.GetAllBuckets()
	if len(buckets) > 16 {
		// Don't bother bootstrapping more than 16 buckets.
		// GenRandPeerID can't generate target peer IDs with more than
		// 16 bits specified anyways.
		buckets = buckets[:16]
	}
	for bucketID, bucket := range buckets {
		if time.Since(bucket.RefreshedAt()) <= dht.rtRefreshPeriod {
			continue
		}
		// gen rand peer in the bucket
		randPeerInBucket := dht.routingTable.GenRandPeerID(bucketID)

		// walk to the generated peer
		walkFnc := func(c context.Context) error {
			_, err := dht.FindPeer(c, randPeerInBucket)
			if err == routing.ErrNotFound {
				return nil
			}
			return err
		}

		if err := doQuery(bucketID, randPeerInBucket.String(), walkFnc); err != nil {
			logger.Warningf("failed to do a random walk on bucket %d: %s", bucketID, err)
		}
	}
}

// Traverse the DHT toward the self ID
func (dht *IpfsDHT) selfWalk(ctx context.Context) {
	queryCtx, cancel := context.WithTimeout(ctx, dht.rtRefreshQueryTimeout)
	defer cancel()
	_, err := dht.FindPeer(queryCtx, dht.self)
	if err == routing.ErrNotFound {
		return
	}
	logger.Warningf("failed to query self during routing table refresh: %s", err)
}

// Bootstrap tells the DHT to get into a bootstrapped state satisfying the
// IpfsRouter interface.
//
// This just calls `RefreshRoutingTable`.
func (dht *IpfsDHT) Bootstrap(_ context.Context) error {
	dht.RefreshRoutingTable()
	return nil
}

// RefreshRoutingTable tells the DHT to refresh it's routing tables.
func (dht *IpfsDHT) RefreshRoutingTable() {
	select {
	case dht.triggerRtRefresh <- struct{}{}:
	default:
	}
}
