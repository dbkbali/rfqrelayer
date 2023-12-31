package main

import (
	"fmt"
	"log"
	"math/big"
	"math/rand"

	"github.com/OCAX-labs/rfqrelayer/common"
	"github.com/OCAX-labs/rfqrelayer/core/types"
	cryptoocax "github.com/OCAX-labs/rfqrelayer/crypto/ocax"
	"github.com/OCAX-labs/rfqrelayer/utils"
)

type RFQRequest struct {
	From common.Address     `json:"from"`
	Data types.SignableData `json:"data"`
	V    *big.Int           `json:"v"`
	R    *big.Int           `json:"r"`
	S    *big.Int           `json:"s"`
}

func main() {
	privateKey := cryptoocax.GeneratePrivateKey()
	publicKey := privateKey.PublicKey()

	addr := publicKey.Address()
	checkSumAddr := addr.Hex()
	addr = common.HexToAddress(checkSumAddr)

	// generate a random number between 200 and 1000
	num := rand.Intn(1000-200) + 200
	amountTokens := big.NewInt(int64(num))
	amountTokens = amountTokens.Mul(amountTokens, big.NewInt(1e18)) // add 18 decimals

	uid := utils.GenerateRandomStringID(10)

	signableData := types.SignableData{
		RequestorId:     uid,
		BaseTokenAmount: amountTokens,
		BaseToken: &types.BaseToken{
			Address:  common.HexToAddress("0x9f8F72aA9304c8B593d555F12eF6589cC3A579A2"),
			Symbol:   "MKR",
			Decimals: 18,
		},
		QuoteToken: &types.QuoteToken{
			Address:  common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"),
			Symbol:   "USDC",
			Decimals: 6,
		},
		RFQDurationMs: 90000,
	}

	rfqRequest := types.NewRFQRequest(addr, &signableData)
	tx := types.NewTx(rfqRequest)
	signedTx, err := tx.Sign(privateKey)
	if err != nil {
		log.Fatalf("Failed to sign data: %v", err)
	}
	v, r, s := signedTx.RawSignatureValues()
	vStr := fmt.Sprintf("0x%x", v)
	rStr := fmt.Sprintf("0x%x", r)
	sStr := fmt.Sprintf("0x%x", s)
	fmt.Printf(`{
    "from": "%s",
    "data": {
        "requestorId": "%s",
        "baseTokenAmount": %s,
        "baseToken": {
            "Address": "%s",
            "Symbol": "%s",
            "Decimals": %d
        },
        "quoteToken": {
            "Address": "%s",
            "Symbol": "%s",
            "Decimals": %d
        },
        "rfqDurationMs": %d
    },
    "v": "%s",
    "r": "%s",
    "s": "%s"
}\n`,
		addr.Hex(),
		uid,
		amountTokens.String(),
		signableData.BaseToken.Address.Hex(),
		signableData.BaseToken.Symbol,
		signableData.BaseToken.Decimals,
		signableData.QuoteToken.Address.Hex(),
		signableData.QuoteToken.Symbol,
		signableData.QuoteToken.Decimals,
		signableData.RFQDurationMs,
		vStr,
		rStr,
		sStr,
	)
}
