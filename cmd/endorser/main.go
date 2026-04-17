package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"

	"github.com/offchainlabs/nitro/endorsement"
	"github.com/offchainlabs/nitro/endorsementpolicy"
)

func main() {
	var (
		endorserID  = flag.String("id", "", "endorser id, e.g. A/B/C")
		listenAddr  = flag.String("listen", ":9001", "http listen address")
		secretKey   = flag.String("bls-secret-key", "", "fixed BLS secret key hex")
		rejectTo    = flag.String("reject-to", "0x1111111111111111111111111111111111111111", "reject target address")
	)
	flag.Parse()

	if *endorserID == "" {
		panic("missing -id")
	}
	if *secretKey == "" {
		panic("missing -bls-secret-key")
	}

	id := endorsementpolicy.EndorserID(*endorserID)

	keyStore := endorsement.NewBLSSecretKeyStore()
	if err := keyStore.AddSecretKeyHex(id, *secretKey); err != nil {
		panic(fmt.Errorf("load BLS secret key: %w", err))
	}

	pubBytes, err := keyStore.GetPublicKeyBytes(id)
	if err != nil {
		panic(fmt.Errorf("get public key: %w", err))
	}

	rules := endorsement.EndorsementRejectRules{}
	if *rejectTo != "" {
		addr := common.HexToAddress(*rejectTo)
		rules.RejectByToAndEndorser = map[common.Address]map[endorsementpolicy.EndorserID]bool{
			addr: {
				id: true,
			},
		}
	}

	signer := &endorsement.BLSEndorsementClient{
		KeyStore: keyStore,
		Rules:    rules,
	}

	handler, err := endorsement.NewHTTPEndorserHandler(&endorsement.HTTPEndorserServer{
		EndorserID: id,
		Signer:     signer,
		PublicKey:  pubBytes,
	})
	if err != nil {
		panic(fmt.Errorf("create endorser handler: %w", err))
	}


	srv := &http.Server{
		Addr:              *listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Info("starting endorser server",
		"endorser", id,
		"listen", *listenAddr,
		"publicKeyHex", fmt.Sprintf("%x", pubBytes),
	)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			panic(fmt.Errorf("endorser server exited: %w", err))
		}
	}()

	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, syscall.SIGINT, syscall.SIGTERM)
	<-stopCh

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}