package protocol

import (
	"fmt"
	"github.com/deckarep/golang-set"
	"github.com/pkg/errors"
	"idena-go/blockchain/types"
	"idena-go/common"
	"idena-go/p2p"
	"time"
)

const (
	MaxKnownBlocks = 300
	MaxKwownTxs    = 2000
	MaxKnownProofs = 1000
	MaxKnownVotes  = 100000

	handshakeTimeout = 10 * time.Second
)

type peer struct {
	*p2p.Peer
	rw                p2p.MsgReadWriter
	id                string
	knownHeight       uint64
	knownTxs          mapset.Set // Set of transaction hashes known to be known by this peer
	knownBlocks       mapset.Set // Set of block hashes known to be known by this peer
	knownVotes        mapset.Set // Set of hashes of votes known to be known by this peer
	knownProofs       mapset.Set // Set of hashes of proposer proofs known to be known by this peer
	queuedTxs         chan *types.Transaction
	queuedProofs      chan *proposeProof // Queue of proposer proofs to broadcast to the peer
	queuedBlockRanges chan *blockRange   // Queue of blocks ranges to broadcast to the peer
	queuedProposals   chan *types.Block
	queuedVotes       chan *types.Vote
	queuedRequests    chan *request
	term              chan struct{}
}

type request struct {
	msgcode uint64
	data    interface{}
}

func (pm *ProtocolManager) makePeer(p *p2p.Peer, rw p2p.MsgReadWriter) *peer {
	return &peer{
		rw:                rw,
		Peer:              p,
		id:                fmt.Sprintf("%x", p.ID().Bytes()[:8]),
		knownBlocks:       mapset.NewSet(),
		knownTxs:          mapset.NewSet(),
		knownVotes:        mapset.NewSet(),
		queuedTxs:         make(chan *types.Transaction, 100),
		queuedBlockRanges: make(chan *blockRange, 10),
		queuedVotes:       make(chan *types.Vote, 100),
		queuedProposals:   make(chan *types.Block, 10),
		queuedRequests:    make(chan *request, 20),
		knownProofs:       mapset.NewSet(),
		queuedProofs:      make(chan *proposeProof, 10),
		term:              make(chan struct{}),
	}
}

func (p *peer) SendBlockRangeAsync(blockRange *blockRange) {
	p.queuedBlockRanges <- blockRange
}

func (p *peer) SendHeader(header *types.Header, code uint64) {
	p.queuedRequests <- &request{msgcode: code, data: header}
}

func (p *peer) RequestLastBlock() {
	p.queuedRequests <- &request{msgcode: GetHead, data: struct{}{}}
}

func (p *peer) RequestBlockByHash(hash common.Hash) {
	p.queuedRequests <- &request{msgcode: GetBlockByHash, data: &getBlockBodyRequest{
		Hash: hash,
	}}
}

func (p *peer) RequestBlocksRange(batchId uint32, from uint64, to uint64) {
	p.queuedRequests <- &request{msgcode: GetBlocksRange, data: &getBlocksRangeRequest{
		BatchId: batchId,
		From:    from,
		To:      to,
	}}
}

func (p *peer) broadcast() {
	defer p.Log().Info("Peer exited from broadcast loop")
	for {
		select {
		case blockRange := <-p.queuedBlockRanges:
			if err := p2p.Send(p.rw, BlocksRange, blockRange); err != nil {
				p.Log().Error(err.Error())
				return
			}
		case proof := <-p.queuedProofs:
			if err := p2p.Send(p.rw, ProposeProof, proof); err != nil {
				p.Log().Error(err.Error())
				return
			}
		case block := <-p.queuedProposals:
			if err := p2p.Send(p.rw, ProposeBlock, block); err != nil {
				p.Log().Error(err.Error())
				return
			}
		case vote := <-p.queuedVotes:
			if err := p2p.Send(p.rw, Vote, vote); err != nil {
				p.Log().Error(err.Error())
				return
			}
		case tx := <-p.queuedTxs:
			if err := p2p.Send(p.rw, NewTx, tx); err != nil {
				p.Log().Error(err.Error())
				return
			}
		case request := <-p.queuedRequests:
			if err := p2p.Send(p.rw, request.msgcode, request.data); err != nil {
				p.Log().Error(err.Error())
				return
			}
		case <-p.term:
			return
		}
	}
}

func (p *peer) Handshake(network types.Network, height uint64, genesis common.Hash) error {
	errc := make(chan error, 2)
	var handShake handshakeData

	go func() {
		errc <- p2p.Send(p.rw, Handshake, &handshakeData{

			NetworkId:    network,
			Height:       height,
			GenesisBlock: genesis,
		})
	}()
	go func() {
		errc <- p.readStatus(&handShake, network, genesis)
	}()
	timeout := time.NewTimer(handshakeTimeout)
	defer timeout.Stop()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errc:
			if err != nil {
				return err
			}
		case <-timeout.C:
			return p2p.DiscReadTimeout
		}
	}
	p.knownHeight = handShake.Height
	return nil
}

func (p *peer) readStatus(handShake *handshakeData, network types.Network, genesis common.Hash) (err error) {
	msg, err := p.rw.ReadMsg()
	if err != nil {
		return err
	}
	if msg.Code != Handshake {
		return errors.New(fmt.Sprintf("first msg has code %x (!= %x)", msg.Code, Handshake))
	}
	if err := msg.Decode(&handShake); err != nil {
		return errors.New(fmt.Sprintf("can't decode msg %v: %v", msg, err))
	}
	if handShake.GenesisBlock != genesis {
		return errors.New(fmt.Sprintf("Bad genesis block %x (!= %x)", handShake.GenesisBlock[:8], genesis[:8]))
	}
	if handShake.NetworkId != network {
		return errors.New(fmt.Sprintf("Network mismatch: %d (!= %d)", handShake.NetworkId, network))
	}

	return nil
}

func (p *peer) SendProofAsync(proof *proposeProof) {
	p.queuedProofs <- proof
	p.markProof(proof)
}
func (p *peer) ProposeBlockAsync(block *types.Block) {
	p.queuedProposals <- block
	p.markBlock(block)
}

func (p *peer) SendTxAsync(transaction *types.Transaction) {
	p.queuedTxs <- transaction
	p.markTx(transaction)
}

func (p *peer) markBlock(block *types.Block) {
	if p.knownBlocks.Cardinality() > MaxKnownBlocks {
		p.knownBlocks.Pop()
	}
	p.knownBlocks.Add(block.Hash())
}

func (p *peer) markProof(proof *proposeProof) {
	if p.knownProofs.Cardinality() > MaxKnownProofs {
		p.knownProofs.Pop()
	}
	p.knownProofs.Add(proof.Hash)
}
func (p *peer) markVote(vote *types.Vote) {
	if p.knownVotes.Cardinality() > MaxKnownVotes {
		p.knownVotes.Pop()
	}
	p.knownVotes.Add(vote.Hash())
}
func (p *peer) SendVoteAsync(vote *types.Vote) {
	p.queuedVotes <- vote
	p.markVote(vote)
}
func (p *peer) markTx(tx *types.Transaction) {
	if p.knownTxs.Cardinality() > MaxKwownTxs {
		p.knownTxs.Pop()
	}
	p.knownTxs.Add(tx)
}
func (p *peer) setHeight(newHeight uint64) {
	if newHeight > p.knownHeight {
		p.knownHeight = newHeight
	}
}
