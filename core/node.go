package core

import (
	"context"
	"crypto/rand"
	"fmt"

	logging "github.com/ipfs/go-log"

	ipld "github.com/ipfs/go-ipld-format"

	host "github.com/libp2p/go-libp2p-host"
	protocol "github.com/libp2p/go-libp2p-protocol"
	contract "github.com/RTradeLtd/toychain/contract"
	lookup "github.com/RTradeLtd/toychain/lookup"
	state "github.com/RTradeLtd/toychain/state"
	types "github.com/RTradeLtd/toychain/types"

	hamt "github.com/ipfs/go-hamt-ipld"

	bitswap "github.com/ipfs/go-bitswap"
	bserv "github.com/ipfs/go-blockservice"

	"errors"
)

var log = logging.Logger("core")

var ProtocolID = protocol.ID("/tch/0.0.0")

type ToychainNode struct {
	Host host.Host

	Addresses []types.Address

	bsub, txsub *floodsub.Subscription
	pubsub      *floodsub.PubSub

	Lookup *lookup.LookupEngine

	DAG     ipld.DAGService
	Bitswap *bitswap.Bitswap
	cs      *hamt.CborIpldStore

	StateMgr *state.StateManager
}

func NewToychainNode(h host.Host, fs *floodsub.PubSub, dag ipld.DAGService, bs bserv.BlockService, bswap *bitswap.Bitswap) (*ToychainNode, error) {
	le, err := lookup.NewLookupEngine(fs, h.ID())
	if err != nil {
		return nil, err
	}

	tch := &ToychainNode{
		Host:    h,
		DAG:     dag,
		Bitswap: bswap,
		cs:      &hamt.CborIpldStore{bs},
		Lookup:  le,
	}

	s := state.NewStateManager(tch.cs, tch.DAG)

	tch.StateMgr = s

	baseAddr := CreateNewAddress()
	tch.Lookup.AddAddress(baseAddr)
	tch.Addresses = []types.Address{baseAddr}
	fmt.Println("my mining address is ", baseAddr)

	genesis, err := CreateGenesisBlock(tch.cs)
	if err != nil {
		return nil, err
	}
	s.SetBestBlock(genesis)

	if err := tch.DAG.Add(context.TODO(), genesis.ToNode()); err != nil {
		return nil, err
	}
	fmt.Println("genesis block cid is: ", genesis.Cid())
	s.KnownGoodBlocks.Add(genesis.Cid())

	st, err := contract.LoadState(context.Background(), tch.cs, genesis.StateRoot)
	if err != nil {
		return nil, err
	}
	s.StateRoot = st

	// TODO: better miner construction and delay start until synced
	s.Miner = state.NewMiner(tch.SendNewBlock, s.TxPool, genesis, baseAddr, tch.cs)
	s.Miner.StateMgr = s

	// Run miner
	go s.Miner.Run(context.Background())

	txsub, err := fs.Subscribe(TxsTopic)
	if err != nil {
		return nil, err
	}

	blksub, err := fs.Subscribe(BlocksTopic)
	if err != nil {
		return nil, err
	}

	go tch.processNewBlocks(blksub)
	go tch.processNewTransactions(txsub)

	h.SetStreamHandler(HelloProtocol, tch.handleHelloStream)
	h.Network().Notify((*tchNotifiee)(tch))

	tch.txsub = txsub
	tch.bsub = blksub
	tch.pubsub = fs

	return tch, nil
}

func (tch *ToychainNode) processNewTransactions(txsub *floodsub.Subscription) {
	// TODO: this function should really just be a validator function for the pubsub subscription
	for {
		msg, err := txsub.Next(context.Background())
		if err != nil {
			panic(err)
		}

		var txmsg types.Transaction
		if err := txmsg.Unmarshal(msg.GetData()); err != nil {
			panic(err)
		}

		tch.StateMgr.InformTx(&txmsg)
	}
}

func CreateNewAddress() types.Address {
	buf := make([]byte, 20)
	rand.Read(buf)
	return types.Address(buf)
}

func (tch *ToychainNode) processNewBlocks(blksub *floodsub.Subscription) {
	// TODO: this function should really just be a validator function for the pubsub subscription
	for {
		msg, err := blksub.Next(context.Background())
		if err != nil {
			panic(err)
		}
		if msg.GetFrom() == tch.Host.ID() {
			continue
		}

		blk, err := types.DecodeBlock(msg.GetData())
		if err != nil {
			panic(err)
		}

		tch.StateMgr.Inform(msg.GetFrom(), blk)
	}
}

func (tch *ToychainNode) SendNewBlock(b *types.Block) error {
	nd := b.ToNode()
	if err := tch.DAG.Add(context.TODO(), nd); err != nil {
		return err
	}

	if err := tch.StateMgr.ProcessNewBlock(context.Background(), b); err != nil {
		return err
	}

	return tch.pubsub.Publish(BlocksTopic, nd.RawData())
}

func (tch *ToychainNode) SendNewTransaction(tx *types.Transaction) error {
	//TODO: do some validation here.
	// If the user sends an invalid transaction (bad nonce, etc) it will simply
	// get dropped by the network, with no indication of what happened. This is
	// generally considered to be bad UX
	data, err := tx.Marshal()
	if err != nil {
		return errors.Wrap(err, "marshaling transaction failed")
	}

	var b types.Block

	var newblock types.Block
	newblock.Parent = b.Cid()

	return tch.pubsub.Publish(TxsTopic, data)
}

type TxResult struct {
	Block   *types.Block
	Receipt *types.Receipt
}

func (tch *ToychainNode) SendNewTransactionAndWait(ctx context.Context, tx *types.Transaction) (*TxResult, error) {
	notifs := tch.StateMgr.BlockNotifications(ctx)

	data, err := tx.Marshal()
	if err != nil {
		return nil, err
	}

	if err := tch.pubsub.Publish(TxsTopic, data); err != nil {
		return nil, err
	}

	c, err := tx.Cid()
	if err != nil {
		return nil, err
	}

	for {
		select {
		case blk, ok := <-notifs:
			if !ok {
				continue
			}
			fmt.Printf("processing block... searching for tx... (%d txs)\n", len(blk.Txs))
			for i, tx := range blk.Txs {
				oc, err := tx.Cid()
				if err != nil {
					return nil, err
				}
				fmt.Println("checking equality... ", c, oc)

				if c.Equals(oc) {
					return &TxResult{
						Block:   blk,
						Receipt: blk.Receipts[i],
					}, nil
				}
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (tch *ToychainNode) IsOurAddress(chk types.Address) bool {
	for _, a := range tch.Addresses {
		if a == chk {
			return true
		}
	}
	return false
}
