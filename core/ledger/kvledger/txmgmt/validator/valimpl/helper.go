/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package valimpl

import (
	"bytes"
	"fmt"

	"github.com/hyperledger/fabric/core/ledger"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/privacyenabledstate"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/rwsetutil"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/validator"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/validator/valinternal"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/version"
	"github.com/hyperledger/fabric/core/ledger/util"
	"github.com/hyperledger/fabric/protos/common"
	"github.com/hyperledger/fabric/protos/peer"
	"github.com/hyperledger/fabric/protos/utils"
)

// validateAndPreparePvtBatch pulls out the private write-set from transient store for the transactions that are marked as valid
// by the internal public data validator. Finally, it validates (if not already self-endorsed) the pvt rwset against the
// corresponding hash present in the public rwset
func validateAndPreparePvtBatch(block *valinternal.Block, pvtdata map[uint64]*ledger.TxPvtData) (*privacyenabledstate.PvtUpdateBatch, error) {
	pvtUpdates := privacyenabledstate.NewPvtUpdateBatch()
	for _, tx := range block.Txs {
		if tx.ValidationCode != peer.TxValidationCode_VALID {
			continue
		}
		if !tx.ContainsPvtWrites() {
			continue
		}
		txPvtdata := pvtdata[uint64(tx.IndexInBlock)]
		if txPvtdata == nil {
			return nil,
				&validator.ErrMissingPvtdata{Msg: fmt.Sprintf("Pvt data missing for the transaction tx num [%d]", tx.IndexInBlock)}
		}
		if requiresPvtdataValidation(txPvtdata) {
			if err := validatePvtdata(tx, txPvtdata); err != nil {
				return nil, err
			}
		}
		var pvtRWSet *rwsetutil.TxPvtRwSet
		var err error
		if pvtRWSet, err = rwsetutil.TxPvtRwSetFromProtoMsg(txPvtdata.WriteSet); err != nil {
			return nil, err
		}
		addPvtRWSetToPvtUpdateBatch(pvtRWSet, pvtUpdates, version.NewHeight(block.Num, uint64(tx.IndexInBlock)))
	}
	return pvtUpdates, nil
}

// requiresPvtdataValidation returns whether or not a hashes of the collection should be computed
// for the collections of present in the private data
// TODO for now always return true. Add capabilty of checking if this data was produced by
// the validating peer itself during similation and in that case return false
func requiresPvtdataValidation(tx *ledger.TxPvtData) bool {
	return true
}

// validPvtdata returns true if hashes of all the collections writeset present in the pvt data
// match with the corresponding hashes present in the public read-write set
func validatePvtdata(tx *valinternal.Transaction, pvtdata *ledger.TxPvtData) error {
	if pvtdata.WriteSet == nil {
		return nil
	}

	for _, nsPvtdata := range pvtdata.WriteSet.NsPvtRwset {
		for _, collPvtdata := range nsPvtdata.CollectionPvtRwset {
			collPvtdataHash := util.ComputeHash(collPvtdata.Rwset)
			hashInPubdata := tx.RetrieveHash(nsPvtdata.Namespace, collPvtdata.CollectionName)
			if !bytes.Equal(collPvtdataHash, hashInPubdata) {
				return &validator.ErrPvtdataHashMissmatch{
					Msg: fmt.Sprintf(`Hash of pvt data for collection [%s:%s] does not match with the corresponding hash in the public data. 
					public hash = [%#v], pvt data hash = [%#v]`, nsPvtdata.Namespace, collPvtdata.CollectionName, hashInPubdata, collPvtdataHash),
				}
			}
		}
	}
	return nil
}

// preprocessProtoBlock parses the proto instance of block into 'Block' structure.
// The retuned 'Block' structure contains only transactions that are endorser transactions and are not alredy marked as invalid
func preprocessProtoBlock(block *common.Block) (*valinternal.Block, error) {
	b := &valinternal.Block{Num: block.Header.Number}
	// Committer validator has already set validation flags based on well formed tran checks
	txsFilter := util.TxValidationFlags(block.Metadata.Metadata[common.BlockMetadataIndex_TRANSACTIONS_FILTER])
	// Precaution in case committer validator has not added validation flags yet
	if len(txsFilter) == 0 {
		txsFilter = util.NewTxValidationFlags(len(block.Data.Data))
		block.Metadata.Metadata[common.BlockMetadataIndex_TRANSACTIONS_FILTER] = txsFilter
	}
	for txIndex, envBytes := range block.Data.Data {
		if txsFilter.IsInvalid(txIndex) {
			// Skiping invalid transaction
			logger.Warningf("Block [%d] Transaction index [%d] marked as invalid by committer. Reason code [%d]",
				block.Header.Number, txIndex, txsFilter.Flag(txIndex))
			continue
		}
		env, err := utils.GetEnvelopeFromBlock(envBytes)
		if err != nil {
			return nil, err
		}
		payload, err := utils.GetPayload(env)
		if err != nil {
			return nil, err
		}
		chdr, err := utils.UnmarshalChannelHeader(payload.Header.ChannelHeader)
		if err != nil {
			return nil, err
		}
		txType := common.HeaderType(chdr.Type)
		if txType != common.HeaderType_ENDORSER_TRANSACTION {
			logger.Debugf("Skipping mvcc validation for Block [%d] Transaction index [%d] because, the transaction type is [%s]",
				block.Header.Number, txIndex, txType)
			continue
		}
		// extract actions from the envelope message
		respPayload, err := utils.GetActionFromEnvelope(envBytes)
		if err != nil {
			txsFilter.SetFlag(txIndex, peer.TxValidationCode_NIL_TXACTION)
			continue
		}
		//preparation for extracting RWSet from transaction
		txRWSet := &rwsetutil.TxRwSet{}
		// Get the Result from the Action
		// and then Unmarshal it into a TxReadWriteSet using custom unmarshalling
		if err = txRWSet.FromProtoBytes(respPayload.Results); err != nil {
			txsFilter.SetFlag(txIndex, peer.TxValidationCode_INVALID_OTHER_REASON)
			continue
		}
		b.Txs = append(b.Txs, &valinternal.Transaction{IndexInBlock: txIndex, ID: chdr.TxId, RWSet: txRWSet})
	}
	return b, nil
}

// postprocessProtoBlock updates the proto block's validation flags (in metadata) by the results of validation process
func postprocessProtoBlock(block *common.Block, validatedBlock *valinternal.Block) {
	txsFilter := util.TxValidationFlags(block.Metadata.Metadata[common.BlockMetadataIndex_TRANSACTIONS_FILTER])
	for _, tx := range validatedBlock.Txs {
		txsFilter.SetFlag(tx.IndexInBlock, tx.ValidationCode)
	}
	block.Metadata.Metadata[common.BlockMetadataIndex_TRANSACTIONS_FILTER] = txsFilter
}

func addPvtRWSetToPvtUpdateBatch(pvtRWSet *rwsetutil.TxPvtRwSet, pvtUpdateBatch *privacyenabledstate.PvtUpdateBatch, ver *version.Height) {
	for _, ns := range pvtRWSet.NsPvtRwSet {
		for _, coll := range ns.CollPvtRwSets {
			for _, kvwrite := range coll.KvRwSet.Writes {
				if !kvwrite.IsDelete {
					pvtUpdateBatch.Put(ns.NameSpace, coll.CollectionName, kvwrite.Key, kvwrite.Value, ver)
				} else {
					pvtUpdateBatch.Delete(ns.NameSpace, coll.CollectionName, kvwrite.Key, ver)
				}
			}
		}
	}
}
