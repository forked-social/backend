package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deso-protocol/core/lib"
	"github.com/gorilla/mux"
)

// TestPublicKeyParamRegexMatchesForkAndUpstreamKeys verifies that mux routes
// built with makePublicKeyParamRegex match fork network public keys
// (FS1…/tFS…, 54 chars) as well as upstream keys (BC… 55 chars, tBC… 54
// chars), while still rejecting malformed or garbage values.
func TestPublicKeyParamRegexMatchesForkAndUpstreamKeys(t *testing.T) {
	// Build a router mirroring the production route definitions that embed
	// public keys in the path.
	var matchedVars map[string]string
	captureVars := http.HandlerFunc(func(ww http.ResponseWriter, req *http.Request) {
		matchedVars = mux.Vars(req)
		ww.WriteHeader(http.StatusOK)
	})
	router := mux.NewRouter().StrictSlash(true)
	router.NewRoute().
		Path(RoutePathValidators + "/" + makePublicKeyParamRegex(publicKeyBase58CheckKey)).
		Handler(captureVars)
	router.NewRoute().
		Path(RoutePathStake + "/" + makePublicKeyParamRegex(validatorPublicKeyBase58CheckKey) + "/" +
			makePublicKeyParamRegex(stakerPublicKeyBase58CheckKey)).
		Handler(captureVars)

	// Keys committed in core/lib/fork_params.go (fork mainnet, FS1…, 54 chars).
	forkMainnetKeys := []string{
		lib.FounderRootPubKeyBase58Check,
		lib.MinerPubKeyBase58Check,
		lib.BlockProducerPubKeyBase58Check,
		lib.StarterDeSoPubKeyBase58Check,
	}

	// A deterministic key encoded against each network's params.
	deterministicPk := append([]byte{0x02}, make([]byte, 32)...)
	forkTestnetKey := lib.PkToString(deterministicPk, &lib.ForkTestnetParams)     // tFS2…, 54 chars
	upstreamMainnetKey := lib.PkToString(deterministicPk, &lib.DeSoMainnetParams) // BC1YL…, 55 chars
	upstreamTestnetKey := lib.PkToString(deterministicPk, &lib.DeSoTestnetParams) // tBCK…, 54 chars

	assertMatches := func(t *testing.T, path, wantParam, wantVarValue string) {
		t.Helper()
		matchedVars = nil
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected route match for %q, got status %d", path, rec.Code)
		}
		if got := matchedVars[wantParam]; got != wantVarValue {
			t.Fatalf("expected %s=%q, got %q", wantParam, wantVarValue, got)
		}
	}
	assertNoMatch := func(t *testing.T, path string) {
		t.Helper()
		matchedVars = nil
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if matchedVars != nil || rec.Code == http.StatusOK {
			t.Fatalf("expected no route match for %q, got status %d", path, rec.Code)
		}
	}

	validKeys := append([]string{forkTestnetKey, upstreamMainnetKey, upstreamTestnetKey}, forkMainnetKeys...)
	for _, key := range validKeys {
		assertMatches(t, RoutePathValidators+"/"+key, publicKeyBase58CheckKey, key)
	}
	// The two-param stake route accepts fork and upstream keys in any mix.
	assertMatches(t, RoutePathStake+"/"+forkMainnetKeys[0]+"/"+forkTestnetKey, stakerPublicKeyBase58CheckKey, forkTestnetKey)
	assertMatches(t, RoutePathStake+"/"+upstreamMainnetKey+"/"+upstreamTestnetKey, validatorPublicKeyBase58CheckKey, upstreamMainnetKey)
	assertMatches(t, RoutePathStake+"/"+forkTestnetKey+"/"+upstreamMainnetKey, validatorPublicKeyBase58CheckKey, forkTestnetKey)

	junkKeys := []string{
		"not-a-public-key",
		strings.Repeat("A", 54),                     // right length, wrong prefix
		lib.FounderRootPubKeyBase58Check[:53],       // fork key, too short (53 chars)
		lib.FounderRootPubKeyBase58Check + "z",      // fork key, too long (55 chars)
		lib.FounderRootPubKeyBase58Check[:53] + "0", // invalid base58 char '0'
		forkTestnetKey[:53] + "I",                   // invalid base58 char 'I'
	}
	for _, key := range junkKeys {
		assertNoMatch(t, RoutePathValidators+"/"+key)
	}
}
