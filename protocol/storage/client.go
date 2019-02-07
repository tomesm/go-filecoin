package storage

import (
	"context"
	"fmt"
	"math/big"
	"sync"

	"gx/ipfs/QmR8BauakNcBa3RbE4nbQu76PDiJgoQgz8AJdhJuiU4TAw/go-cid"
	cbor "gx/ipfs/QmRoARq3nkUb13HSKZGepCZSWe5GrVPwx7xURJGZ7KWv9V/go-ipld-cbor"
	"gx/ipfs/QmVmDhyTTUcQXFD1rRQ64fGLMSAoaQvNH3hwuaCFAPq2hy/errors"
	"gx/ipfs/QmY5Grm8pJdiSSVsYxx4uNRgweY72EmYwuSDbRnbFok3iY/go-libp2p-peer"
	"gx/ipfs/QmZNkThpqfVXs9GNbexPrfBbXSLNYeKrE7jwFM2oqHbyqN/go-libp2p-protocol"
	"gx/ipfs/QmabLh8TrJ3emfAoQk5AbqbLTbMyj7XqumMFmAFxa9epo8/go-multistream"
	"gx/ipfs/QmaoXrM4Z41PD48JY36YqQGKQpLGjyLA2cKcLsES7YddAq/go-libp2p-host"
	ipld "gx/ipfs/QmcKKBwfz6FyQdHR2jsXrrF6XeSBXYL86anmWNewpFpoF5/go-ipld-format"
	"gx/ipfs/Qmf4xQhNomPNhrtZc67qSnfJSjxjXs9LWvknJtSXwimPrM/go-datastore"

	"github.com/filecoin-project/go-filecoin/actor/builtin/miner"
	"github.com/filecoin-project/go-filecoin/actor/builtin/paymentbroker"
	"github.com/filecoin-project/go-filecoin/address"
	cbu "github.com/filecoin-project/go-filecoin/cborutil"
	"github.com/filecoin-project/go-filecoin/porcelain"
	"github.com/filecoin-project/go-filecoin/protocol/storage/deal"
	"github.com/filecoin-project/go-filecoin/repo"
	"github.com/filecoin-project/go-filecoin/types"
	"github.com/filecoin-project/go-filecoin/util/convert"
)

const (
	_ = iota
	// ErrDuplicateDeal indicates that a deal being proposed is a duplicate of an existing deal
	ErrDuplicateDeal
)

// Errors map error codes to messages
var Errors = map[uint8]error{
	ErrDuplicateDeal: errors.New("proposal is a duplicate of existing deal; if you would like to create a duplicate, add the --allow-duplicates flag"),
}

const (
	// VoucherInterval defines how many block pass before creating a new voucher
	VoucherInterval = 1000

	// ChannelExpiryBuffer defines how long the channel remains open past the last voucher
	ChannelExpiryBuffer = 2000

	// CreateChannelGasPrice is the gas price of the message used to create the payment channel
	CreateChannelGasPrice = 0

	// CreateChannelGasLimit is the gas limit of the message used to create the payment channel
	CreateChannelGasLimit = 300
)

type clientNode interface {
	GetFileSize(context.Context, cid.Cid) (uint64, error)
	MakeProtocolRequest(ctx context.Context, protocol protocol.ID, peer peer.ID, request interface{}, response interface{}) error
}

type clientPorcelainAPI interface {
	ConfigGet(dottedPath string) (interface{}, error)
	ChainBlockHeight(ctx context.Context) (*types.BlockHeight, error)
	CreatePayments(ctx context.Context, config porcelain.CreatePaymentsParams) (*porcelain.CreatePaymentsReturn, error)
	MinerGetAsk(ctx context.Context, minerAddr address.Address, askID uint64) (miner.Ask, error)
	MinerGetOwnerAddress(ctx context.Context, minerAddr address.Address) (address.Address, error)
	MinerGetPeerID(ctx context.Context, minerAddr address.Address) (peer.ID, error)
	DealsLs() (<-chan *deal.Deal, <-chan error)
}

// Client is used to make deals directly with storage miners.
type Client struct {
	deals   map[cid.Cid]*deal.Deal
	dealsDs repo.Datastore
	dealsLk sync.Mutex

	node clientNode
	api  clientPorcelainAPI
}

func init() {
	cbor.RegisterCborType(deal.Deal{})
}

// NewClient creates a new storage client.
func NewClient(nd clientNode, api clientPorcelainAPI, dealsDs repo.Datastore) (*Client, error) {
	smc := &Client{
		deals:   make(map[cid.Cid]*deal.Deal),
		node:    nd,
		api:     api,
		dealsDs: dealsDs,
	}
	if err := smc.loadDeals(); err != nil {
		return nil, errors.Wrap(err, "failed to load client deals")
	}
	return smc, nil
}

// ProposeDeal is
func (smc *Client) ProposeDeal(ctx context.Context, miner address.Address, data cid.Cid, askID uint64, duration uint64, allowDuplicates bool) (*deal.Response, error) {
	size, err := smc.node.GetFileSize(ctx, data)
	if err != nil {
		return nil, errors.Wrap(err, "failed to determine the size of the data")
	}

	ask, err := smc.api.MinerGetAsk(ctx, miner, askID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get ask price")
	}
	price := ask.Price

	chainHeight, err := smc.api.ChainBlockHeight(ctx)
	if err != nil {
		return nil, err
	}

	from, err := smc.api.ConfigGet("wallet.defaultAddress")
	if err != nil {
		return nil, err
	}
	fromAddress, ok := from.(address.Address)
	if !ok || fromAddress.Empty() {
		return nil, errors.New("Default wallet address is not set correctly")
	}

	minerOwner, err := smc.api.MinerGetOwnerAddress(ctx, miner)
	if err != nil {
		return nil, err
	}

	totalPrice := price.MulBigInt(big.NewInt(int64(size * duration)))

	proposal := &deal.Proposal{
		PieceRef:     data,
		Size:         types.NewBytesAmount(size),
		TotalPrice:   totalPrice,
		Duration:     duration,
		MinerAddress: miner,
		// TODO: Sign this proposal
	}

	// check for duplicate deal prior to creating payment info
	proposalCid, err := convert.ToCid(proposal)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get cid of proposal")
	}

	_, isDuplicate := smc.deals[proposalCid]
	if isDuplicate && !allowDuplicates {
		return nil, Errors[ErrDuplicateDeal]
	}

	for ; isDuplicate; _, isDuplicate = smc.deals[proposalCid] {
		proposal.LastDuplicate = proposalCid.String()

		proposalCid, err = convert.ToCid(proposal)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get cid of proposal")
		}
	}

	// create payment information
	cpResp, err := smc.api.CreatePayments(ctx, porcelain.CreatePaymentsParams{
		From:            fromAddress,
		To:              minerOwner,
		Value:           *price.MulBigInt(big.NewInt(int64(size * duration))),
		Duration:        duration,
		PaymentInterval: VoucherInterval,
		ChannelExpiry:   *chainHeight.Add(types.NewBlockHeight(duration + ChannelExpiryBuffer)),
		GasPrice:        *types.NewAttoFIL(big.NewInt(CreateChannelGasPrice)),
		GasLimit:        types.NewGasUnits(CreateChannelGasLimit),
	})
	if err != nil {
		return nil, err
	}

	proposal.Payment.Channel = cpResp.Channel
	proposal.Payment.ChannelMsgCid = cpResp.ChannelMsgCid.String()
	proposal.Payment.Vouchers = cpResp.Vouchers

	// send proposal
	pid, err := smc.api.MinerGetPeerID(ctx, miner)
	if err != nil {
		return nil, err
	}

	var response deal.Response
	err = smc.node.MakeProtocolRequest(ctx, makeDealProtocol, pid, proposal, &response)
	if err != nil {
		return nil, errors.Wrap(err, "error sending proposal")
	}

	if err := smc.checkDealResponse(ctx, &response); err != nil {
		return nil, errors.Wrap(err, "response check failed")
	}

	// Note: currently the miner requests the data out of band

	if err := smc.recordResponse(&response, miner, proposal, proposalCid); err != nil {
		return nil, errors.Wrap(err, "failed to track response")
	}

	return &response, nil
}

func (smc *Client) recordResponse(resp *deal.Response, miner address.Address, p *deal.Proposal, proposalCid cid.Cid) error {
	smc.dealsLk.Lock()
	defer smc.dealsLk.Unlock()
	_, ok := smc.deals[proposalCid]
	if ok {
		return fmt.Errorf("deal [%s] is already in progress", proposalCid.String())
	}

	smc.deals[proposalCid] = &deal.Deal{
		Miner:    miner,
		Proposal: p,
		Response: resp,
	}
	return smc.saveDeal(proposalCid)
}

func (smc *Client) checkDealResponse(ctx context.Context, resp *deal.Response) error {
	switch resp.State {
	case deal.Rejected:
		return fmt.Errorf("deal rejected: %s", resp.Message)
	case deal.Failed:
		return fmt.Errorf("deal failed: %s", resp.Message)
	case deal.Accepted:
		return nil
	default:
		return fmt.Errorf("invalid proposal response: %s", resp.State)
	}
}

func (smc *Client) minerForProposal(c cid.Cid) (address.Address, error) {
	smc.dealsLk.Lock()
	defer smc.dealsLk.Unlock()
	st, ok := smc.deals[c]
	if !ok {
		return address.Address{}, fmt.Errorf("no such proposal by cid: %s", c)
	}

	return st.Miner, nil
}

// QueryDeal queries an in-progress proposal.
func (smc *Client) QueryDeal(ctx context.Context, proposalCid cid.Cid) (*deal.Response, error) {
	mineraddr, err := smc.minerForProposal(proposalCid)
	if err != nil {
		return nil, err
	}

	minerpid, err := smc.api.MinerGetPeerID(ctx, mineraddr)
	if err != nil {
		return nil, err
	}

	q := deal.QueryRequest{Cid: proposalCid}
	var resp deal.Response
	err = smc.node.MakeProtocolRequest(ctx, queryDealProtocol, minerpid, q, &resp)
	if err != nil {
		return nil, errors.Wrap(err, "error querying deal")
	}

	return &resp, nil
}

func (smc *Client) loadDeals() error {
	smc.deals = make(map[cid.Cid]*deal.Deal)

	deals, doneOrError := smc.api.DealsLs()
	select {
	case storageDeal := <-deals:
		smc.deals[storageDeal.Response.ProposalCid] = storageDeal
	case errOrNil := <-doneOrError:
		return errOrNil
	}
	return nil
}

func (smc *Client) saveDeal(cid cid.Cid) error {
	storageDeal, ok := smc.deals[cid]
	if !ok {
		return errors.Errorf("Could not find client deal with cid: %s", cid.String())
	}
	datum, err := cbor.DumpObject(storageDeal)
	if err != nil {
		return errors.Wrap(err, "could not marshal storageDeal")
	}

	key := datastore.KeyWithNamespaces([]string{deal.ClientDatastorePrefix, cid.String()})
	err = smc.dealsDs.Put(key, datum)
	if err != nil {
		return errors.Wrap(err, "could not save client deal to disk, in-memory deals differ from persisted deals!")
	}
	return nil
}

// LoadVouchersForDeal loads vouchers from disk for a given deal
func (smc *Client) LoadVouchersForDeal(dealCid cid.Cid) ([]*paymentbroker.PaymentVoucher, error) {
	var results []*paymentbroker.PaymentVoucher

	deals, doneOrError := smc.api.DealsLs()
	select {
	case storageDeal := <-deals:
		if storageDeal.Response.ProposalCid == dealCid {
			results = append(results, storageDeal.Proposal.Payment.Vouchers...)
		}
	case errOrNil := <-doneOrError:
		return results, errOrNil
	}
	return results, nil
}

// ClientNodeImpl implements the client node interface
type ClientNodeImpl struct {
	dserv ipld.DAGService
	host  host.Host
}

// NewClientNodeImpl constructs a ClientNodeImpl
func NewClientNodeImpl(ds ipld.DAGService, host host.Host) *ClientNodeImpl {
	return &ClientNodeImpl{
		dserv: ds,
		host:  host,
	}
}

// GetFileSize returns the size of the file referenced by 'c'
func (cni *ClientNodeImpl) GetFileSize(ctx context.Context, c cid.Cid) (uint64, error) {
	return getFileSize(ctx, c, cni.dserv)
}

// MakeProtocolRequest makes a request and expects a response from the host using the given protocol.
func (cni *ClientNodeImpl) MakeProtocolRequest(ctx context.Context, protocol protocol.ID, peer peer.ID, request interface{}, response interface{}) error {
	s, err := cni.host.NewStream(ctx, peer, protocol)
	if err != nil {
		if err == multistream.ErrNotSupported {
			return errors.New("could not establish connection with peer. Peer does not support protocol")
		}

		return errors.Wrap(err, "failed to establish connection with the peer")
	}

	if err := cbu.NewMsgWriter(s).WriteMsg(request); err != nil {
		return errors.Wrap(err, "failed to write request")
	}

	if err := cbu.NewMsgReader(s).ReadMsg(response); err != nil {
		return errors.Wrap(err, "failed to read response")
	}
	return nil
}
