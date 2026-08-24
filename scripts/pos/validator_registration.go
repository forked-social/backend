// validator_registration.go is the single-validator manual registration
// helper for the forked-social network (re-registrations / day-2 ops).
// For the automated launch-day fund/register/stake of validators 1-3 in one
// pass, use the sibling sub-package instead:
//
//	go run ./scripts/pos/validator_bootstrap
//
// (validator_bootstrap lives in its own sub-package because this file
// already declares `package main` with its main() in scripts/pos/.)
//
// ALL inputs — every secret included — are read from environment variables
// at runtime. No mnemonic is ever committed here, printed, or included in an
// error message. If a required variable is unset or empty, the tool prints
// the usage block below (naming the missing variables) and exits 1.
//
// # Environment variables (naming mirrors validator_bootstrap)
//
//	MINER_SEED          REQUIRED. Mnemonic of a funded key — the fork miner
//	                    holds the entire genesis supply — that funds the
//	                    validator with 5,125,000 coins before registration.
//	VALIDATOR_SEED      REQUIRED. The validator's DeSo mnemonic. Its derived
//	                    key (index 0) signs the register-as-validator and
//	                    stake transactions.
//	VALIDATOR_BLS_SEED  REQUIRED. The validator's BLS mnemonic. The voting
//	                    keypair + voting authorization are derived in-process
//	                    via lib.NewBLSKeystore — the same call path as the
//	                    node, validator_bootstrap, and validator-key-generator
//	                    — so identical mnemonics produce identical keys
//	                    everywhere. It must be the SAME mnemonic later given to
//	                    the validator container's POS_VALIDATOR_SEED.
//	VALIDATOR_DOMAIN    Optional. Registration domain.
//	                    Default: node.forked.social:42100
//	API_URL             Optional. API base URL used when --api-url is not
//	                    passed. Default: https://node.forked.social
//
// Amounts match validator_bootstrap: fund 5,125,000 coins (stake + 2.5%
// fee/burn buffer), stake 5,000,000 coins.
//
// # Usage (from backend/ inside the forked-builder container)
//
//	MINER_SEED=... VALIDATOR_SEED=... VALIDATOR_BLS_SEED=... \
//	  go run ./scripts/pos --api-url http://127.0.0.1:42001
//
// Post-launch timing: this tool does NOT enforce a block-height deadline —
// for time-critical launch-day registrations use validator_bootstrap, whose
// deadline guard protects the block-~145 epoch-1 snapshot.
package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/deso-protocol/backend/routes"
	"github.com/deso-protocol/core/bls"
	"github.com/deso-protocol/core/lib"
	"github.com/deso-protocol/uint256"
	"github.com/pkg/errors"
	"github.com/tyler-smith/go-bip39"
)

var params = &lib.ForkMainnetParams

const (
	fundingNanos         uint64 = 5_125_000 * lib.NanosPerUnit // 5,125,000 coins (stake + 2.5% fee buffer)
	stakeNanos           uint64 = 5_000_000 * lib.NanosPerUnit // 5,000,000 coins staked
	minFeeRateNanosPerKB uint64 = 1000

	// Manual day-2 tool: fixed waits between submissions (2s blocks).
	submissionSettleDelay = 5 * time.Second
	httpTimeout           = 30 * time.Second

	defaultAPIURL          = "https://node.forked.social"
	defaultValidatorDomain = "node.forked.social:42100"
)

// registrationConfig holds the env-derived inputs. Seed values stay in
// memory only and are never printed or included in errors.
type registrationConfig struct {
	apiURL           string
	funderSeed       string // MINER_SEED            — SECRET, never printed
	validatorSeed    string // VALIDATOR_SEED        — SECRET, never printed
	validatorBLSSeed string // VALIDATOR_BLS_SEED    — SECRET, never printed
	validatorDomain  string
}

const usageText = `validator_registration — single-validator manual registration
(fund -> register-as-validator -> stake) for the forked-social network.

Required environment variables:
  MINER_SEED           funded key that pays for the registration (the fork
                       miner holds the entire genesis supply)
  VALIDATOR_SEED       the validator's DeSo mnemonic (signs register + stake)
  VALIDATOR_BLS_SEED   the validator's BLS mnemonic (voting key; must match
                       the validator container's POS_VALIDATOR_SEED)

Optional environment variables:
  VALIDATOR_DOMAIN     registration domain (default: node.forked.social:42100)
  API_URL              API base URL (default: https://node.forked.social)

Flags:
  --api-url URL        override API_URL

Example (from backend/ inside the forked-builder container):
  MINER_SEED=... VALIDATOR_SEED=... VALIDATOR_BLS_SEED=... \
    go run ./scripts/pos --api-url http://127.0.0.1:42001

For the launch-day registration of validators 1-3 use instead:
  backend/validators/register-validators.sh  (or go run ./scripts/pos/validator_bootstrap)`

func printUsage(to *os.File) {
	fmt.Fprintln(to, usageText)
}

// loadConfigFromEnv reads and validates the env surface. Missing required
// variables are reported together (names only — values are never printed).
func loadConfigFromEnv(apiURLFlag string) (*registrationConfig, error) {
	var missing []string
	for _, name := range []string{"MINER_SEED", "VALIDATOR_SEED", "VALIDATOR_BLS_SEED"} {
		if os.Getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		printUsage(os.Stderr)
		fmt.Fprintln(os.Stderr)
		return nil, errors.Errorf(
			"required environment variable(s) not set (or empty): %s — see usage above",
			strings.Join(missing, ", "))
	}

	apiURL := apiURLFlag
	if apiURL == "" {
		apiURL = os.Getenv("API_URL")
	}
	if apiURL == "" {
		apiURL = defaultAPIURL
	}

	domain := os.Getenv("VALIDATOR_DOMAIN")
	if domain == "" {
		domain = defaultValidatorDomain
	}

	return &registrationConfig{
		apiURL:           strings.TrimSuffix(apiURL, "/"),
		funderSeed:       os.Getenv("MINER_SEED"),
		validatorSeed:    os.Getenv("VALIDATOR_SEED"),
		validatorBLSSeed: os.Getenv("VALIDATOR_BLS_SEED"),
		validatorDomain:  domain,
	}, nil
}

// ---------------------------------------------------------------------------
// HTTP helpers (same API surface as validator_bootstrap).
// ---------------------------------------------------------------------------

type nodeClient struct {
	baseURL string
	http    *http.Client
}

func makePostRequest[TPayload any, TResponse any](client *nodeClient, path string, payload TPayload) (TResponse, error) {
	var zero TResponse
	postBody, err := json.Marshal(payload)
	if err != nil {
		return zero, errors.Wrap(err, "makePostRequest: could not marshal payload")
	}

	resp, err := client.http.Post(client.baseURL+path, "application/json", bytes.NewBuffer(postBody))
	if err != nil {
		return zero, errors.Wrapf(err, "makePostRequest: request to %s%s failed", client.baseURL, path)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return zero, errors.Errorf("makePostRequest: %s returned status %d: %s",
			path, resp.StatusCode, string(bodyBytes))
	}

	var decodedResponse TResponse
	if err := json.NewDecoder(resp.Body).Decode(&decodedResponse); err != nil {
		return zero, errors.Wrapf(err, "makePostRequest: failed to decode response from %s", path)
	}
	return decodedResponse, nil
}

func signAndSubmitTxn(client *nodeClient, txn *lib.MsgDeSoTxn, privKey *btcec.PrivateKey) (string, error) {
	signature, err := txn.Sign(privKey)
	if err != nil {
		return "", errors.Wrap(err, "signAndSubmitTxn: could not sign txn")
	}
	txn.Signature.SetSignature(signature)

	txnBytes, err := txn.ToBytes(false)
	if err != nil {
		return "", errors.Wrap(err, "signAndSubmitTxn: could not serialize txn")
	}

	submitResponse, err := makePostRequest[routes.SubmitTransactionRequest, routes.SubmitTransactionResponse](
		client, routes.RoutePathSubmitTransaction,
		routes.SubmitTransactionRequest{TransactionHex: hex.EncodeToString(txnBytes)},
	)
	if err != nil {
		return "", errors.Wrap(err, "signAndSubmitTxn: submit-transaction rejected the txn")
	}
	return submitResponse.TxnHashHex, nil
}

// ---------------------------------------------------------------------------
// Key derivation — identical to validator_bootstrap / validator-key-generator.
// Error messages never include mnemonic contents.
// ---------------------------------------------------------------------------

func generatePubAndPrivKeys(seedPhrase string) (*lib.PublicKey, *btcec.PrivateKey, error) {
	seedBytes, err := bip39.NewSeedWithErrorChecking(seedPhrase, "")
	if err != nil {
		return nil, nil, errors.New("generatePubAndPrivKeys: invalid mnemonic (contents not logged)")
	}
	pubKey, privKey, _, err := lib.ComputeKeysFromSeed(seedBytes, 0, params)
	if err != nil {
		return nil, nil, errors.New("generatePubAndPrivKeys: could not derive DeSo key pair")
	}
	return lib.NewPublicKey(pubKey.SerializeCompressed()), privKey, nil
}

// getBLSVotingAuthorizationAndPublicKey derives the BLS voting key from the
// validator's BLS mnemonic and signs the voting authorization over the DeSo
// transactor key (lib.NewBLSKeystore + CreateValidatorVotingAuthorizationPayload
// — the exact derivation validator_bootstrap uses).
func getBLSVotingAuthorizationAndPublicKey(
	blsSeed string, transactorPublicKey *lib.PublicKey,
) (*bls.PublicKey, *bls.Signature, error) {
	blsKeyStore, err := lib.NewBLSKeystore(blsSeed)
	if err != nil {
		return nil, nil, errors.New("getBLSVotingAuthorizationAndPublicKey: could not generate BLS keystore")
	}
	votingAuthPayload := lib.CreateValidatorVotingAuthorizationPayload(transactorPublicKey.ToBytes())
	votingAuthorization, err := blsKeyStore.GetSigner().Sign(votingAuthPayload)
	if err != nil {
		return nil, nil, errors.New("getBLSVotingAuthorizationAndPublicKey: signing failed")
	}
	return blsKeyStore.GetSigner().GetPublicKey(), votingAuthorization, nil
}

// ---------------------------------------------------------------------------
// Transaction construction (same API surface as validator_bootstrap).
// ---------------------------------------------------------------------------

func constructSendDESOTxn(client *nodeClient, senderPubKey, recipientPubKey *lib.PublicKey) (routes.SendDeSoResponse, error) {
	request := routes.SendDeSoRequest{
		SenderPublicKeyBase58Check:   lib.PkToString(senderPubKey.ToBytes(), params),
		RecipientPublicKeyOrUsername: lib.PkToString(recipientPubKey.ToBytes(), params),
		AmountNanos:                  int64(fundingNanos),
		MinFeeRateNanosPerKB:         minFeeRateNanosPerKB,
	}
	return makePostRequest[routes.SendDeSoRequest, routes.SendDeSoResponse](client, routes.RoutePathSendDeSo, request)
}

func constructRegisterAsValidatorTxn(
	client *nodeClient, validatorPubKey *lib.PublicKey, domain string,
	votingPubKey *bls.PublicKey, votingAuthorization *bls.Signature,
) (routes.ValidatorTxnResponse, error) {
	request := routes.RegisterAsValidatorRequest{
		TransactorPublicKeyBase58Check: lib.PkToString(validatorPubKey.ToBytes(), params),
		Domains:                        []string{domain},
		DisableDelegatedStake:          false,
		VotingPublicKey:                votingPubKey.ToString(),
		VotingAuthorization:            votingAuthorization.ToString(),
		ExtraData:                      map[string]string{},
		MinFeeRateNanosPerKB:           minFeeRateNanosPerKB,
		TransactionFees:                []routes.TransactionFee{},
	}
	return makePostRequest[routes.RegisterAsValidatorRequest, routes.ValidatorTxnResponse](
		client, routes.RoutePathValidators+"/register", request)
}

func constructStakeTxn(client *nodeClient, validatorPubKey *lib.PublicKey) (routes.StakeTxnResponse, error) {
	validatorPublicKeyString := lib.PkToString(validatorPubKey.ToBytes(), params)
	request := routes.StakeRequest{
		TransactorPublicKeyBase58Check: validatorPublicKeyString,
		ValidatorPublicKeyBase58Check:  validatorPublicKeyString,
		RewardMethod:                   routes.PayToBalance,
		StakeAmountNanos:               uint256.NewInt(stakeNanos),
		ExtraData:                      map[string]string{},
		MinFeeRateNanosPerKB:           minFeeRateNanosPerKB,
		TransactionFees:                []routes.TransactionFee{},
	}
	return makePostRequest[routes.StakeRequest, routes.StakeTxnResponse](client, routes.RoutePathStake, request)
}

// ---------------------------------------------------------------------------
// main.
// ---------------------------------------------------------------------------

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\n❌ REGISTRATION FAILED: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	apiURL := flag.String("api-url", "",
		"Base URL of the forknet backend API. Default: $API_URL if set, else https://node.forked.social. "+
			"On the seed host use http://127.0.0.1:42001.")
	flag.Parse()

	if flag.NArg() > 0 {
		printUsage(os.Stderr)
		return errors.Errorf("unexpected positional arguments: %v (see usage above)", flag.Args())
	}

	config, err := loadConfigFromEnv(*apiURL)
	if err != nil {
		return err
	}
	client := &nodeClient{baseURL: config.apiURL, http: &http.Client{Timeout: httpTimeout}}

	fmt.Println("==================================================================")
	fmt.Printf("forked-social single-validator registration | network: %s (fork mainnet)\n",
		params.NetworkType.String())
	fmt.Printf("api-url: %s | domain: %s\n", config.apiURL, config.validatorDomain)
	fmt.Println("==================================================================")

	// Derive keys. funder <- MINER_SEED; validator identity <- VALIDATOR_SEED;
	// voting keys <- VALIDATOR_BLS_SEED (secrets stay in memory).
	funderPubKey, funderPrivKey, err := generatePubAndPrivKeys(config.funderSeed)
	if err != nil {
		return errors.Wrap(err, "MINER_SEED")
	}
	validatorPubKey, validatorPrivKey, err := generatePubAndPrivKeys(config.validatorSeed)
	if err != nil {
		return errors.Wrap(err, "VALIDATOR_SEED")
	}
	votingPubKey, votingAuthorization, err := getBLSVotingAuthorizationAndPublicKey(
		config.validatorBLSSeed, validatorPubKey)
	if err != nil {
		return errors.Wrap(err, "VALIDATOR_BLS_SEED")
	}

	validatorPubKeyString := lib.PkToString(validatorPubKey.ToBytes(), params)
	fmt.Printf("funder pubkey:              %s\n", lib.PkToString(funderPubKey.ToBytes(), params))
	fmt.Printf("validator pubkey (DeSo):    %s\n", validatorPubKeyString)
	fmt.Printf("validator voting (BLS) key: %s\n", votingPubKey.ToString())

	// (1) Fund the validator from the funder (5,125,000 coins).
	fundingTxn, err := constructSendDESOTxn(client, funderPubKey, validatorPubKey)
	if err != nil {
		return errors.Wrap(err, "construct funding txn")
	}
	fundingTxnHash, err := signAndSubmitTxn(client, fundingTxn.Transaction, funderPrivKey)
	if err != nil {
		return errors.Wrap(err, "submit funding txn")
	}
	fmt.Printf("[1/3] funding submitted (%d coins)  txid %s\n", fundingNanos/lib.NanosPerUnit, fundingTxnHash)

	time.Sleep(submissionSettleDelay)

	// (2) Register as validator, signed by the validator's own DeSo key.
	registrationTxn, err := constructRegisterAsValidatorTxn(
		client, validatorPubKey, config.validatorDomain, votingPubKey, votingAuthorization)
	if err != nil {
		return errors.Wrap(err, "construct register-as-validator txn")
	}
	registrationTxnHash, err := signAndSubmitTxn(client, registrationTxn.Transaction, validatorPrivKey)
	if err != nil {
		return errors.Wrap(err, "submit register-as-validator txn")
	}
	fmt.Printf("[2/3] register-as-validator submitted (domain %s)  txid %s\n",
		config.validatorDomain, registrationTxnHash)

	time.Sleep(submissionSettleDelay)

	// (3) Stake to itself (5,000,000 coins).
	stakeTxn, err := constructStakeTxn(client, validatorPubKey)
	if err != nil {
		return errors.Wrap(err, "construct stake txn")
	}
	stakeTxnHash, err := signAndSubmitTxn(client, stakeTxn.Transaction, validatorPrivKey)
	if err != nil {
		return errors.Wrap(err, "submit stake txn")
	}
	fmt.Printf("[3/3] stake submitted (%d coins)  txid %s\n", stakeNanos/lib.NanosPerUnit, stakeTxnHash)

	// Summary — pubkeys, txids, domain only; never secrets.
	fmt.Println()
	fmt.Println("REGISTRATION SUMMARY (pubkeys + txids only — no secrets)")
	fmt.Printf("  validator pubkey        %s\n", validatorPubKeyString)
	fmt.Printf("  voting (BLS) pubkey     %s\n", votingPubKey.ToString())
	fmt.Printf("  domain                  %s\n", config.validatorDomain)
	fmt.Printf("  funding txn             %s\n", fundingTxnHash)
	fmt.Printf("  registration txn        %s\n", registrationTxnHash)
	fmt.Printf("  stake txn               %s\n", stakeTxnHash)
	fmt.Println()
	fmt.Printf("verify with:\n"+
		"  curl -s %s/api/v0/validators/%s\n"+
		"  curl -s %s/api/v0/stake/%s/%s\n"+
		"  curl -s %s/api/v0/current-epoch-progress\n",
		config.apiURL, validatorPubKeyString,
		config.apiURL, validatorPubKeyString, validatorPubKeyString,
		config.apiURL)
	return nil
}
