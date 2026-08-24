// validator_bootstrap is the launch-day helper for the forked-social PoS
// validator set. It funds, registers, and stakes validators 1-3 on the fork
// mainnet in one sequential pass, using only runtime-injected secrets.
//
// PATH NOTE: this tool lives in its own sub-package because the sibling file
// ../validator_registration.go already declares `package main` with its own
// main() — Go does not allow two main()s in one package directory. Build with:
//
//	go build ./scripts/pos/validator_bootstrap     (from backend/)
//
// It follows the fork-wired pattern of validator_registration.go (identical
// lib route helpers and ForkMainnetParams), extended with: 3-validator
// sequencing, env-only secret handling, per-step block confirmation polling, a
// block-height deadline guard, and a --dry-run mode.
//
// # Environment variables (ALL secrets are read from ENV at runtime)
//
//	MINER_SEED             REQUIRED. The fork miner mnemonic. It holds the
//	                       entire genesis supply (0x-supply UTXO to
//	                       FS13xm5oxJvGKd7194u5f2paA9WaaQpZSvz7Tzu8uXDngxromi3Lnv,
//	                       30,000,000 coins at genesis) and funds every
//	                       validator with 5,125,000 coins.
//	VALIDATOR{N}_SEED      Per validator N∈{1,2,3}: the validator's DeSo
//	                       mnemonic. Its derived key (index 0) signs the
//	                       register-as-validator and stake transactions and is
//	                       expected to derive these launch identities:
//	                         v1 FS13wSsnnLhYeVfjNyvc8HE6f7iWGG6bHJx6Kb5wKR513QJrNQdq5W
//	                         v2 FS13vueAs3CmmPBjm5PnsyjbiUHegNHyNXmCcLdu1A6hh9tc2qHB74
//	                         v3 FS13w9xhKm1vbvDWQKSvZPCqNkQMLFDurJ2TRNw5zjsod4kM1DS1wE
//	VALIDATOR{N}_BLS_SEED  Per validator N: the validator's BLS mnemonic. The
//	                       BLS key pair + voting authorization are derived
//	                       IN-PROCESS with the same lib functions the node and
//	                       validator-key-generator use (lib.NewBLSKeystore +
//	                       lib.CreateValidatorVotingAuthorizationPayload), so
//	                       node / generator / script are guaranteed consistent.
//	                       This SAME mnemonic must later be injected as the
//	                       validator container's POS_VALIDATOR_SEED.
//	VALIDATOR{N}_DOMAIN    Optional. Registration domain for validator N.
//	                       Defaults: node.forked.social:42100,
//	                         node.forked.social:42200,
//	                         node.forked.social:42300
//	                       (the three validators run on the SAME server as
//	                       the seed, so the existing DNS record for
//	                       node.forked.social covers everything — the
//	                       DISTINCT PORTS — matching the published P2P ports
//	                       in backend/validators/docker-compose.validatorN.yml
//	                       — keep the mesh connections apart).
//	API_URL                Optional. Used when --api-url is not passed.
//
// A validator with BOTH seeds unset is skipped; a validator with exactly one
// of the two seeds set is a fatal config error. No mnemonic value is ever
// printed or logged — only public keys, txids, and heights.
//
// # Deadline
//
// Fork params snapshot the epoch-1 validator set at block ~145 (~5 minutes
// after the seed starts mining 2s blocks) and cut over to PoS at block 300.
// Validators not registered+staked before the snapshot miss the first PoS
// validator set and the chain can stall at cutover. Before submitting, the
// script fetches the current height; if it exceeds --deadline-block (default
// 120) it aborts unless --continue-past-deadline is passed.
//
// # Usage (see backend/validators/README.md for the full launch-day runbook)
//
//	MINER_SEED=... VALIDATOR1_SEED=... VALIDATOR1_BLS_SEED=... ... \
//	  go run ./scripts/pos/validator_bootstrap \
//	  --api-url http://127.0.0.1:42001            # on the seed host (fast, no TLS)
//	# or from anywhere: --api-url https://node.forked.social
//	# rehearsal without submitting anything: add --dry-run
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
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/deso-protocol/backend/routes"
	"github.com/deso-protocol/core/bls"
	"github.com/deso-protocol/core/lib"
	"github.com/deso-protocol/uint256"
	"github.com/pkg/errors"
	"github.com/tyler-smith/go-bip39"
)

// ---------------------------------------------------------------------------
// Fixed launch values (public — no secrets).
// ---------------------------------------------------------------------------

var params = &lib.ForkMainnetParams

// 1 coin = 1e9 nanos. Launch-security posture: 5M-coin stakes protect the
// young chain against 50%+ attacks; they are intended to be lowered via
// normal unstake transactions as the network grows. 3 validators ×
// 5,125,000 funded = 15,375,000 coins (15,000,000 staked), leaving
// ~14,625,000 of the 30M genesis supply with the miner.
const (
	fundingNanos         uint64 = 5_125_000 * lib.NanosPerUnit // 5,125,000 coins per validator (stake + 2.5% fee/burn buffer)
	stakeNanos                  = 5_000_000 * lib.NanosPerUnit // 5,000,000 coins staked per validator
	minFeeRateNanosPerKB uint64 = 1000

	// Epoch-1 snapshot is taken at block 145 (DefaultEpochDurationNumBlocks =
	// 144, epoch 1 spans blocks 2..145). The guard aborts when the chain is
	// already past --deadline-block (default 120: ~25 blocks ≈ 50s of margin).
	defaultDeadlineBlockHeight = uint32(120)
	epochSnapshotBlockHeight   = uint32(145)

	// How we wait for confirmation: poll the height until at least this many
	// fresh blocks cover the submitted txn (2s blocks → fast).
	blockPollInterval = 1 * time.Second
	blockPollTimeout  = 2 * time.Minute
	httpTimeout       = 30 * time.Second
)

// Launch identities (public keys only — the corresponding mnemonics are
// runtime-injected env vars; they are NEVER written to code or logs).
const expectedMinerPubKey = "FS13xm5oxJvGKd7194u5f2paA9WaaQpZSvz7Tzu8uXDngxromi3Lnv"

var expectedValidatorPubKeys = map[int]string{
	1: "FS13wSsnnLhYeVfjNyvc8HE6f7iWGG6bHJx6Kb5wKR513QJrNQdq5W",
	2: "FS13vueAs3CmmPBjm5PnsyjbiUHegNHyNXmCcLdu1A6hh9tc2qHB74",
	3: "FS13w9xhKm1vbvDWQKSvZPCqNkQMLFDurJ2TRNw5zjsod4kM1DS1wE",
}

// defaultValidatorDomains must match the published P2P ports
// (PROTOCOL_PORT / ports:) in backend/validators/docker-compose.validatorN.yml
// — the validator mesh dials these registered domains, so a mismatch means it
// would dial the seed's port 42000 instead of this validator's own port. All
// three share the single node.forked.social host: the distinct ports are what
// keep the mesh endpoints unique.
var defaultValidatorDomains = map[int]string{
	1: "node.forked.social:42100",
	2: "node.forked.social:42200",
	3: "node.forked.social:42300",
}

// ---------------------------------------------------------------------------
// Validator runtime config (secrets stay in memory only).
// ---------------------------------------------------------------------------

type validatorConfig struct {
	index   int
	domain  string
	seed    string // DeSo mnemonic (VALIDATOR{N}_SEED)      — SECRET, never printed
	blsSeed string // BLS mnemonic (VALIDATOR{N}_BLS_SEED)    — SECRET, never printed

	// Derived below; public material only.
	pubKey       *lib.PublicKey
	privKey      *btcec.PrivateKey
	votingPubKey *bls.PublicKey
	votingAuth   *bls.Signature
}

type validatorResult struct {
	config          *validatorConfig
	fundTxnHash     string
	registerTxnHash string
	stakeTxnHash    string
	err             error
}

// ---------------------------------------------------------------------------
// HTTP helpers (adapted from validator_registration.go; errors not panics).
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
		return zero, errors.Wrapf(err, "makePostRequest: request to %s failed", client.baseURL+path)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return zero, errors.Errorf("makePostRequest: %s returned status %d: %s",
			path, resp.StatusCode, string(bodyBytes))
	}

	var decodedResponse TResponse
	if err = json.NewDecoder(resp.Body).Decode(&decodedResponse); err != nil {
		return zero, errors.Wrapf(err, "makePostRequest: failed to decode response from %s", path)
	}
	return decodedResponse, nil
}

// getBlockHeight fetches the current block height via get-app-state.
func (client *nodeClient) getBlockHeight() (uint32, error) {
	response, err := makePostRequest[routes.GetAppStateRequest, routes.GetAppStateResponse](
		client, routes.RoutePathGetAppState, routes.GetAppStateRequest{})
	if err != nil {
		return 0, err
	}
	return response.BlockHeight, nil
}

// waitForBlockAdvance polls the height until it has advanced by at least
// `required` blocks past baseline (the height observed at submission), so the
// UTXOs/balances the next transaction depends on are mined.
func (client *nodeClient) waitForBlockAdvance(label string, baseline uint32, required uint32) (uint32, error) {
	deadline := time.Now().Add(blockPollTimeout)
	lastHeight := baseline
	for time.Now().Before(deadline) {
		time.Sleep(blockPollInterval)
		height, err := client.getBlockHeight()
		if err != nil {
			continue // transient API hiccup — keep polling until the timeout
		}
		if height != lastHeight {
			fmt.Printf("    [confirm] %s: height %d -> %d\n", label, lastHeight, height)
			lastHeight = height
		}
		if height >= baseline+required {
			return height, nil
		}
	}
	return lastHeight, errors.Errorf(
		"waitForBlockAdvance: timed out waiting for height >= %d for %s (stuck at %d)",
		baseline+required, label, lastHeight)
}

// ---------------------------------------------------------------------------
// Key derivation (identical to validator_registration.go / validator-key-generator).
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

// getBLSVotingAuthorizationAndPublicKey derives the validator's BLS voting key
// and signs the voting-authorization payload over its DeSo transactor key —
// the same lib call path the node itself and validator-key-generator use.
func getBLSVotingAuthorizationAndPublicKey(
	blsKeyStore *lib.BLSKeystore, transactorPublicKey *lib.PublicKey,
) (*bls.PublicKey, *bls.Signature, error) {
	votingAuthPayload := lib.CreateValidatorVotingAuthorizationPayload(transactorPublicKey.ToBytes())
	votingAuthorization, err := blsKeyStore.GetSigner().Sign(votingAuthPayload)
	if err != nil {
		return nil, nil, errors.New("getBLSVotingAuthorizationAndPublicKey: signing failed")
	}
	return blsKeyStore.GetSigner().GetPublicKey(), votingAuthorization, nil
}

// ---------------------------------------------------------------------------
// Transaction construction + submission (same API surface as the node/routes).
// ---------------------------------------------------------------------------

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

func constructSendDESOTxn(client *nodeClient, senderPubKey, recipientPubKey *lib.PublicKey, amountNanos uint64) (routes.SendDeSoResponse, error) {
	request := routes.SendDeSoRequest{
		SenderPublicKeyBase58Check:   lib.PkToString(senderPubKey.ToBytes(), params),
		RecipientPublicKeyOrUsername: lib.PkToString(recipientPubKey.ToBytes(), params),
		AmountNanos:                  int64(amountNanos),
		MinFeeRateNanosPerKB:         minFeeRateNanosPerKB,
	}
	return makePostRequest[routes.SendDeSoRequest, routes.SendDeSoResponse](
		client, routes.RoutePathSendDeSo, request)
}

func constructRegisterAsValidatorTxn(client *nodeClient, cfg *validatorConfig) (routes.ValidatorTxnResponse, error) {
	request := routes.RegisterAsValidatorRequest{
		TransactorPublicKeyBase58Check: lib.PkToString(cfg.pubKey.ToBytes(), params),
		Domains:                        []string{cfg.domain},
		DisableDelegatedStake:          false,
		VotingPublicKey:                cfg.votingPubKey.ToString(),
		VotingAuthorization:            cfg.votingAuth.ToString(),
		ExtraData:                      map[string]string{},
		MinFeeRateNanosPerKB:           minFeeRateNanosPerKB,
		TransactionFees:                []routes.TransactionFee{},
	}
	return makePostRequest[routes.RegisterAsValidatorRequest, routes.ValidatorTxnResponse](
		client, routes.RoutePathValidators+"/register", request)
}

func constructStakeTxn(client *nodeClient, cfg *validatorConfig, amountNanos uint64) (routes.StakeTxnResponse, error) {
	validatorPublicKeyString := lib.PkToString(cfg.pubKey.ToBytes(), params)
	request := routes.StakeRequest{
		TransactorPublicKeyBase58Check: validatorPublicKeyString,
		ValidatorPublicKeyBase58Check:  validatorPublicKeyString,
		RewardMethod:                   routes.PayToBalance,
		StakeAmountNanos:               uint256.NewInt(amountNanos),
		ExtraData:                      map[string]string{},
		MinFeeRateNanosPerKB:           minFeeRateNanosPerKB,
		TransactionFees:                []routes.TransactionFee{},
	}
	return makePostRequest[routes.StakeRequest, routes.StakeTxnResponse](client, routes.RoutePathStake, request)
}

// ---------------------------------------------------------------------------
// Config loading.
// ---------------------------------------------------------------------------

func mustGetEnv(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return "", errors.Errorf("required environment variable %s is not set", name)
	}
	return value, nil
}

func loadValidatorConfigs() ([]*validatorConfig, error) {
	var configs []*validatorConfig
	for index := 1; index <= 3; index++ {
		seed := os.Getenv(fmt.Sprintf("VALIDATOR%d_SEED", index))
		blsSeed := os.Getenv(fmt.Sprintf("VALIDATOR%d_BLS_SEED", index))

		if seed == "" && blsSeed == "" {
			fmt.Printf("validator %d: no VALIDATOR%d_SEED / VALIDATOR%d_BLS_SEED set — skipping\n",
				index, index, index)
			continue
		}
		if seed == "" || blsSeed == "" {
			return nil, errors.Errorf(
				"validator %d: VALIDATOR%d_SEED and VALIDATOR%d_BLS_SEED must be set TOGETHER "+
					"(exactly one of them is set)", index, index, index)
		}

		domain := os.Getenv(fmt.Sprintf("VALIDATOR%d_DOMAIN", index))
		if domain == "" {
			domain = defaultValidatorDomains[index]
		}

		configs = append(configs, &validatorConfig{
			index:   index,
			domain:  domain,
			seed:    seed,
			blsSeed: blsSeed,
		})
	}
	return configs, nil
}

// deriveKeysAndCrossCheck derives the DeSo keys, the BLS voting keys, and
// cross-checks the derived DeSo identities against the launch-day public keys.
func (cfg *validatorConfig) deriveKeysAndCrossCheck() error {
	// DeSo identity keys (signs registration + stake).
	pubKey, privKey, err := generatePubAndPrivKeys(cfg.seed)
	if err != nil {
		return errors.Wrapf(err, "validator %d", cfg.index)
	}
	cfg.pubKey = pubKey
	cfg.privKey = privKey

	// BLS voting keys (votes / blocks after cutover).
	keyStore, err := lib.NewBLSKeystore(cfg.blsSeed)
	if err != nil {
		return errors.Wrapf(err, "validator %d: BLS keystore", cfg.index)
	}
	votingPubKey, votingAuth, err := getBLSVotingAuthorizationAndPublicKey(keyStore, cfg.pubKey)
	if err != nil {
		return errors.Wrapf(err, "validator %d", cfg.index)
	}
	cfg.votingPubKey = votingPubKey
	cfg.votingAuth = votingAuth

	// Cross-check the DeSo identity against the launch-day expectation.
	derived := lib.PkToString(cfg.pubKey.ToBytes(), params)
	if derived != expectedValidatorPubKeys[cfg.index] {
		fmt.Printf("  ⚠️  WARNING validator %d: derived DeSo pubkey %s does NOT match the launch "+
			"identity %s — double-check VALIDATOR%d_SEED before submitting real transactions!\n",
			cfg.index, derived, expectedValidatorPubKeys[cfg.index], cfg.index)
	}
	return nil
}

// ---------------------------------------------------------------------------
// main.
// ---------------------------------------------------------------------------

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\n❌ BOOTSTRAP FAILED: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	apiURL := flag.String("api-url", "",
		"Base URL of the forknet backend API. Default: $API_URL if set, else https://node.forked.social. "+
			"On the seed host use http://127.0.0.1:42001 (fast, no TLS dependency in the tight launch window).")
	dryRun := flag.Bool("dry-run", false,
		"Derive all keys and print the intended transactions WITHOUT submitting anything.")
	continuePastDeadline := flag.Bool("continue-past-deadline", false,
		"Proceed even if the chain height exceeds --deadline-block (registrations may miss the epoch-1 snapshot).")
	deadlineBlock := flag.Uint64("deadline-block", uint64(defaultDeadlineBlockHeight),
		"Abort when the current block height exceeds this value (epoch-1 snapshot lands at ~145).")
	flag.Parse()

	if *apiURL == "" {
		*apiURL = os.Getenv("API_URL")
	}
	if *apiURL == "" {
		*apiURL = "https://node.forked.social"
	}

	client := &nodeClient{baseURL: *apiURL, http: &http.Client{Timeout: httpTimeout}}

	fmt.Println("==================================================================")
	fmt.Printf("forked-social validator bootstrap  |  network: %s (fork mainnet)\n", params.NetworkType.String())
	fmt.Printf("api-url: %s  |  dry-run: %v\n", *apiURL, *dryRun)
	fmt.Println("==================================================================")

	// --- load + derive ------------------------------------------------------
	minerSeed, err := mustGetEnv("MINER_SEED")
	if err != nil {
		return err
	}
	minerPubKey, minerPrivKey, err := generatePubAndPrivKeys(minerSeed)
	if err != nil {
		return errors.Wrap(err, "MINER_SEED")
	}
	minerPubKeyString := lib.PkToString(minerPubKey.ToBytes(), params)
	fmt.Printf("miner (funder) pubkey: %s\n", minerPubKeyString)
	if minerPubKeyString != expectedMinerPubKey {
		fmt.Printf("  ⚠️  WARNING: derived miner pubkey does NOT match the launch miner key %s — "+
			"it does not hold the genesis supply; funding will fail.\n", expectedMinerPubKey)
	}

	configs, err := loadValidatorConfigs()
	if err != nil {
		return err
	}
	if len(configs) == 0 {
		return errors.New("no validators configured — set VALIDATOR{1,2,3}_SEED and VALIDATOR{1,2,3}_BLS_SEED")
	}

	for _, cfg := range configs {
		if err := cfg.deriveKeysAndCrossCheck(); err != nil {
			return err
		}
	}

	// --- deadline guard -----------------------------------------------------
	var height uint32
	height, heightErr := client.getBlockHeight()
	if heightErr != nil {
		if *dryRun {
			fmt.Printf("deadline guard: API unreachable in dry-run (%v) — skipping height check\n",
				heightErr)
			height = 0
		} else {
			return errors.Wrap(heightErr,
				"could not fetch the current block height — is the seed node up and reachable at --api-url?")
		}
	} else {
		fmt.Printf("current block height: %d (epoch-1 snapshot at ~%d)\n",
			height, epochSnapshotBlockHeight)
		if uint64(height) > *deadlineBlock {
			warning := fmt.Sprintf(
				"⚠️  DEADLINE RISK: height %d exceeds --deadline-block %d. The epoch-1 snapshot "+
					"lands at block ~%d (~%d seconds away at 2s blocks); registrations submitted now may "+
					"MISS the first PoS validator set and the chain can stall at the block-300 cutover.",
				height, *deadlineBlock, epochSnapshotBlockHeight,
				(epochSnapshotBlockHeight-height)*2)
			if !*continuePastDeadline {
				fmt.Fprintln(os.Stderr, warning)
				fmt.Fprintln(os.Stderr, "Aborting. Re-run with --continue-past-deadline to force.")
				return errors.Errorf("height %d past deadline %d", height, *deadlineBlock)
			}
			fmt.Println(warning)
			fmt.Println("continuing (--continue-past-deadline)")
		}
	}

	// --- dry-run: print intent, submit nothing ------------------------------
	if *dryRun {
		fmt.Println()
		fmt.Println("──────────────────────── DRY-RUN: intended transactions ────────────────────────")
		for _, cfg := range configs {
			printValidatorPlan(cfg)
		}
		fmt.Printf("fund  %d validators × %d coins = %d coins total\n",
			len(configs), fundingNanos/lib.NanosPerUnit, uint64(len(configs))*fundingNanos/lib.NanosPerUnit)
		fmt.Println("────────────────────────────────────────────────────────────────────────────────")
		fmt.Println("DRY-RUN complete — nothing was submitted.")
		return nil
	}

	// --- real run: fund → register → stake, per validator, sequentially ------
	results := make([]*validatorResult, 0, len(configs))
	for _, cfg := range configs {
		result := &validatorResult{config: cfg}
		results = append(results, result)
		fmt.Printf("\n=== validator %d === %s\n", cfg.index, lib.PkToString(cfg.pubKey.ToBytes(), params))
		fmt.Printf("domain %s | voting (BLS) pubkey %s\n", cfg.domain, cfg.votingPubKey.ToString())

		if err := bootstrapValidator(client, minerPubKey, minerPrivKey, cfg, result); err != nil {
			result.err = err
			fmt.Printf("  ❌ validator %d failed: %v (continuing with the next validator)\n",
				cfg.index, err)
		}
		// Track the height we got to for the deadline sanity of later validators.
		if h, err := client.getBlockHeight(); err == nil {
			height = h
		}
	}

	// --- summary -------------------------------------------------------------
	fmt.Println()
	fmt.Println("==================================================================")
	fmt.Println("BOOTSTRAP SUMMARY (pubkeys + txids only — no secrets)")
	fmt.Println("==================================================================")
	anyFailed := false
	for _, result := range results {
		cfg := result.config
		validatorPubKey := lib.PkToString(cfg.pubKey.ToBytes(), params)
		fmt.Printf("validator %d | domain %s\n", cfg.index, cfg.domain)
		fmt.Printf("  DeSo pubkey                %s\n", validatorPubKey)
		fmt.Printf("  voting (BLS) pubkey        %s\n", cfg.votingPubKey.ToString())
		if result.err != nil {
			anyFailed = true
			fmt.Printf("  status                     FAILED: %v\n", result.err)
			continue
		}
		fmt.Printf("  funding txn (%d coins)     %s\n", fundingNanos/lib.NanosPerUnit, result.fundTxnHash)
		fmt.Printf("  register-as-validator txn  %s\n", result.registerTxnHash)
		fmt.Printf("  stake txn (%d coins)       %s\n", stakeNanos/lib.NanosPerUnit, result.stakeTxnHash)
		fmt.Printf("  status                     ✓ registered + staked\n")
	}
	fmt.Println()
	fmt.Printf("verify with (any synced node):\n"+
		"  curl -s %s/api/v0/validators/{DeSoPubKey}\n"+
		"  curl -s %s/api/v0/stake/{DeSoPubKey}/{DeSoPubKey}\n"+
		"  curl -s %s/api/v0/current-epoch-progress\n",
		*apiURL, *apiURL, *apiURL)
	fmt.Println("next: start the validator containers with matching POS_VALIDATOR_SEED values")
	fmt.Println("      (see backend/validators/README.md)")

	if anyFailed {
		return errors.New("one or more validators failed — see summary above")
	}
	return nil
}

func printValidatorPlan(cfg *validatorConfig) {
	validatorPubKey := lib.PkToString(cfg.pubKey.ToBytes(), params)
	fmt.Printf("validator %d | domain %s\n", cfg.index, cfg.domain)
	fmt.Printf("  DeSo identity        %s\n", validatorPubKey)
	fmt.Printf("  voting (BLS) pubkey  %s\n", cfg.votingPubKey.ToString())
	fmt.Printf("  voting authorization %s\n", cfg.votingAuth.ToString())
	fmt.Printf("  1. send-deso         %s -> %s : %d coins (funding + fee buffer)\n",
		expectedMinerPubKey, validatorPubKey, fundingNanos/lib.NanosPerUnit)
	fmt.Printf("  2. register-as-validator  transactor %s | Domains [%s] | DisableDelegatedStake false\n",
		validatorPubKey, cfg.domain)
	fmt.Printf("  3. stake             %s stakes %d coins to itself (PAY_TO_BALANCE)\n",
		validatorPubKey, stakeNanos/lib.NanosPerUnit)
	fmt.Println()
}

// bootstrapValidator executes the fund → register → stake sequence for one
// validator, waiting for the height to advance between steps so UTXOs and
// balances are available to the next transaction. submitAndWait captures the
// height right before submitting (the true baseline for the confirmation
// wait) and polls until at least one fresh block covers the txn.
func (client *nodeClient) submitAndWait(
	label string, txn *lib.MsgDeSoTxn, privKey *btcec.PrivateKey,
) (string, error) {
	baseline, err := client.getBlockHeight()
	if err != nil {
		return "", errors.Wrapf(err, "%s: pre-submit height check", label)
	}
	txnHash, err := signAndSubmitTxn(client, txn, privKey)
	if err != nil {
		return "", errors.Wrapf(err, "%s: submit", label)
	}
	fmt.Printf("  submitted  txid %s\n", txnHash)
	if _, err = client.waitForBlockAdvance(label, baseline, 1); err != nil {
		return txnHash, err
	}
	return txnHash, nil
}

func bootstrapValidator(
	client *nodeClient, minerPubKey *lib.PublicKey, minerPrivKey *btcec.PrivateKey,
	cfg *validatorConfig, result *validatorResult,
) error {
	// (a) Fund from the miner: 5,125,000 coins covering the 5,000,000-coin
	// stake plus the 2.5% fee/burn buffer.
	fundingTxn, err := constructSendDESOTxn(client, minerPubKey, cfg.pubKey, fundingNanos)
	if err != nil {
		return errors.Wrap(err, "construct funding txn")
	}
	fmt.Printf("  [1/3] funding: %d coins from the miner\n", fundingNanos/lib.NanosPerUnit)
	if result.fundTxnHash, err = client.submitAndWait("funding", fundingTxn.Transaction, minerPrivKey); err != nil {
		return err
	}

	// (b) Register as validator, signed by the validator's own DeSo key.
	registrationTxn, err := constructRegisterAsValidatorTxn(client, cfg)
	if err != nil {
		return errors.Wrap(err, "construct register-as-validator txn")
	}
	fmt.Printf("  [2/3] register-as-validator: domain %s\n", cfg.domain)
	if result.registerTxnHash, err = client.submitAndWait(
		"registration", registrationTxn.Transaction, cfg.privKey); err != nil {
		return err
	}

	// (c) Stake exactly 5,000,000 coins to itself.
	stakeTxn, err := constructStakeTxn(client, cfg, stakeNanos)
	if err != nil {
		return errors.Wrap(err, "construct stake txn")
	}
	fmt.Printf("  [3/3] stake: %d coins to itself\n", stakeNanos/lib.NanosPerUnit)
	if result.stakeTxnHash, err = client.submitAndWait("stake", stakeTxn.Transaction, cfg.privKey); err != nil {
		return err
	}
	return nil
}
