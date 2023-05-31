package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"time"

	"github.com/OCAX-labs/rfqrelayer/common"
	"github.com/OCAX-labs/rfqrelayer/core/types"
	cryptoocax "github.com/OCAX-labs/rfqrelayer/crypto/ocax"
)

type TransactionWrapper struct {
	Time  time.Time         `json:"time"`
	Inner RFQRequestWrapper `json:"inner"`
	// Include other fields from Transaction as needed
}

type RFQRequestWrapper struct {
	From string                `json:"from"`
	Data types.SignableRFQData `json:"data"`
	V    *big.Int              `json:"v"`
	R    *big.Int              `json:"r"`
	S    *big.Int              `json:"s"`
}

func main() {
	// Your private key for testing (for example purposes only!)
	privateKey := cryptoocax.GeneratePrivateKey()

	publicKey := privateKey.PublicKey()

	addr := publicKey.Address()
	checkSumAddr := addr.Hex()
	addr = common.HexToAddress(checkSumAddr)

	// Create an instance of SignableRFQData
	signableData := types.SignableRFQData{
		RequestorId:     "119",
		BaseTokenAmount: "1000000",
		BaseToken: types.BaseToken{
			Address:  common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"),
			Symbol:   "VFG",
			Decimals: 18,
		},
		QuoteToken: types.QuoteToken{
			Address:  common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"),
			Symbol:   "USDC",
			Decimals: 6,
		},
		RFQDurationMs: 60000,
	}

	rfqRequest := types.NewTransaction(addr, signableData)
	tx := types.NewTx(rfqRequest)
	signedTx, err := tx.Sign(privateKey)
	if err != nil {
		log.Fatalf("Failed to sign data: %v", err)
	}

	txWrapper := NewTransactionWrapper(signedTx)
	txWrapper.Inner.Data = signableData

	signedTxJson, err := json.Marshal(txWrapper)
	if err != nil {
		log.Fatalf("Failed to marshal signed transaction to JSON: %v", err)
	}

	fmt.Printf("Signed transaction: %s\n", string(signedTxJson))

	resp, err := http.Post("http://127.0.0.1:9999/tx", "application/json", bytes.NewBuffer(signedTxJson))
	if err != nil {
		log.Fatalf("Failed to send request: %v", err)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	defer resp.Body.Close()
	fmt.Println(result)

	fmt.Printf("Response: %+v\n", resp)
	fmt.Printf("Response status: %s\n", resp.Status)
}

func NewTransactionWrapper(tx *types.Transaction) *TransactionWrapper {
	v, r, s := tx.RawSignatureValues()
	return &TransactionWrapper{
		Time: time.Now(),
		Inner: RFQRequestWrapper{
			From: tx.From().Hex(),
			V:    v,
			R:    r,
			S:    s,
		},
	}
}
