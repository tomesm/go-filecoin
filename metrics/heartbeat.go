package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"gx/ipfs/QmPMtD39NN63AEUNghk1LFQcTLcCmYL8MtRzdv8BRUsC4Z/go-libp2p-host"
	net "gx/ipfs/QmQSbtGXCyNrj34LWL8EgXyNNYDZ8r3SwQcpW5pPxVhLnM/go-libp2p-net"
	"gx/ipfs/QmQsErDt8Qgw1XrsXf2BpEzDgGWtB1YLsTAARBup5b6B9W/go-libp2p-peer"
	logging "gx/ipfs/QmRREK2CAZ5Re2Bd9zZFG6FeYDppUWt5cMgsoUEp3ktgSr/go-log"
	ma "gx/ipfs/QmYmsdtJ3HsodkePE3eU3TsCaP2YvPZJ4LoXnNkDE5Tpt7/go-multiaddr"
	pstore "gx/ipfs/QmeKD8YT7887Xu6Z86iZmpYNxrLogJexqxEugSmaf14k64/go-libp2p-peerstore"

	"github.com/filecoin-project/go-filecoin/config"
	"github.com/filecoin-project/go-filecoin/consensus"
)

// HeartbeatProtocol is the libp2p protocol used for the heartbeat service
const HeartbeatProtocol = "fil/heartbeat/1.0.0"

var log = logging.Logger("metrics")

// Heartbeat contains the information required to determine the current state of a node.
// Heartbeats are used for aggregating information about nodes in a log aggregator
// to support alerting and cluster visualization.
type Heartbeat struct {
	// Head represents the heaviest tipset the nodes is mining on
	Head string
	// Height represents the current height of the Tipset
	Height uint64
	// Nickname is the nickname given to the filecoin node by the user
	Nickname string
	// TODO: add when implemented
	// Syncing is `true` iff the node is currently syncing its chain with the network.
	// Syncing bool
}

// HeartbeatService is responsible for sending heartbeats.
type HeartbeatService struct {
	Host   host.Host
	Config *config.HeartbeatConfig

	// A function that returns the heaviest tipset
	HeadGetter func() consensus.TipSet

	streamMu sync.Mutex
	stream   net.Stream
}

// NewHeartbeatService returns a HeartbeatService
func NewHeartbeatService(h host.Host, hbc *config.HeartbeatConfig, hg func() consensus.TipSet) *HeartbeatService {
	return &HeartbeatService{
		Host:       h,
		Config:     hbc,
		HeadGetter: hg,
	}
}

// Stream returns the HeartbeatService stream. Safe for concurrent access.
// Stream is a libp2p connection that heartbeat messages are sent over to an aggregator.
func (hbs *HeartbeatService) Stream() net.Stream {
	hbs.streamMu.Lock()
	defer hbs.streamMu.Unlock()
	return hbs.stream
}

// SetStream sets the stream on the HeartbeatService. Safe for concurrent access.
func (hbs *HeartbeatService) SetStream(s net.Stream) {
	hbs.streamMu.Lock()
	defer hbs.streamMu.Unlock()
	hbs.stream = s
}

// Start starts the heartbeat service by, starting the connection loop. The connection
// loop will attempt to connected to the aggregator service, once a successful
// connection is made with the aggregator service hearbeats will be sent to it.
// If the connection is broken the heartbeat service will attempt to reconnect via
// the connection loop. Start will not return until context `ctx` is 'Done'.
func (hbs *HeartbeatService) Start(ctx context.Context) {
	log.Debug("starting heartbeat service")

	rd, err := time.ParseDuration(hbs.Config.ReconnectPeriod)
	if err != nil {
		log.Errorf("invalid heartbeat reconnectPeriod: %s", err)
		return
	}

	reconTicker := time.NewTicker(rd)
	defer reconTicker.Stop()
	for {
		log.Debug("running heartbeat reconnect loop")
		select {
		case <-ctx.Done():
			log.Debug("stopping heartbeat service")
			return
		case <-reconTicker.C:
			if err := hbs.Connect(ctx); err != nil {
				log.Debugf("heartbeat service failed to connect: %s", err)
				// failed to connect, continue reconnect loop
				continue
			}
			// we connected, send heartbeats!
			// Run will block until it fails to send a heartbeat.
			if err := hbs.Run(ctx); err != nil {
				log.Debugf("heartbeat run failed: %s", err)
				log.Warning("disconnecting from aggregator, failed to send heartbeat")
				continue
			}
		}
	}
}

// Run is called once the heartbeat service connects to the aggregator. Run
// send the actual heartbeat. Run will block until `ctx` is 'Done`. An error will
// be returned if Run encounters an error when sending the heartbeat and the connection
// to the aggregator will be closed.
func (hbs *HeartbeatService) Run(ctx context.Context) error {
	log.Debug("running heartbeat service")
	bd, err := time.ParseDuration(hbs.Config.BeatPeriod)
	if err != nil {
		log.Errorf("invalid heartbeat beatPeriod: %s", err)
		return err
	}
	beatTicker := time.NewTicker(bd)
	defer beatTicker.Stop()

	// TODO use cbor instead of json
	encoder := json.NewEncoder(hbs.stream)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-beatTicker.C:
			if err := hbs.Beat(encoder); err != nil {
				hbs.stream.Conn().Close() // nolint: errcheck
				return err
			}
		}
	}
}

// Beat will create a heartbeat.
func (hbs *HeartbeatService) Beat(enc *json.Encoder) error {
	log.Debug("heartbeat service creating heartbeat")
	nick := hbs.Config.Nickname
	ts := hbs.HeadGetter()
	tipset := ts.ToSortedCidSet().String()
	height, err := ts.Height()
	if err != nil {
		log.Warningf("heartbeat service failed to get chain height: %s", err)
	}
	return enc.Encode(Heartbeat{
		Head:     tipset,
		Height:   height,
		Nickname: nick,
	})
}

// Connect will connects to `hbs.Config.BeatTarget` or returns an error
func (hbs *HeartbeatService) Connect(ctx context.Context) error {
	log.Debugf("heartbeat service attempting to connect, targetAddress: %s", hbs.Config.BeatTarget)
	targetMaddr, err := ma.NewMultiaddr(hbs.Config.BeatTarget)
	if err != nil {
		return err
	}

	pid, err := targetMaddr.ValueForProtocol(ma.P_P2P)
	if err != nil {
		return err
	}

	peerid, err := peer.IDB58Decode(pid)
	if err != nil {
		return err
	}

	// Decapsulate the /p2p/<peerID> part from the target
	// /ip4/<a.b.c.d>/p2p/<peer> becomes /ip4/<a.b.c.d>
	targetPeerAddr, _ := ma.NewMultiaddr(
		fmt.Sprintf("/p2p/%s", peer.IDB58Encode(peerid)))
	targetAddr := targetMaddr.Decapsulate(targetPeerAddr)

	log.Infof("attempting to open stream, peerID: %s, targetAddr: %s", peerid, targetAddr)
	hbs.Host.Peerstore().AddAddr(peerid, targetAddr, pstore.PermanentAddrTTL)

	s, err := hbs.Host.NewStream(ctx, peerid, HeartbeatProtocol)
	if err != nil {
		log.Warningf("failed to open stream, peerID: %s, targetAddr: %s %s", peerid, targetAddr, err)
		return err
	}
	log.Infof("successfully to open stream, peerID: %s, targetAddr: %s", peerid, targetAddr)

	hbs.SetStream(s)
	return nil
}