package routes

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/deso-protocol/core/lib"
	"github.com/stretchr/testify/require"
)

// The $DESO-market trading fee must route to the fork's founder-treasury (not
// upstream Openfund treasuries) at an unchanged total of 10 basis points.
const founderTreasuryPublicKeyBase58Check = "FS13zB3hyv7V3nnMx5Dikp8NMGpDfrcQ1MWVz8nptRygd446NTVawA"

func TestTradingFeesForDesoMarketRouteToFounderTreasury(t *testing.T) {
	// The DESO special case in GetTradingFeesForMarket returns before the
	// utxoView is ever touched, so a nil view is fine for this path. The
	// profile key must first pass Base58CheckDecode, so exercise the special
	// case with the zero-PKID encoding of $DESO (one of the identifiers
	// IsDesoPkid accepts) rather than the "DESO" string literal.
	feeMap, tradingFeeUpdateDisabled, err := GetTradingFeesForMarket(
		nil, &lib.ForkMainnetParams, "", DeSoZeroPkidMainnetBase58)
	require.NoError(t, err)
	require.False(t, tradingFeeUpdateDisabled)

	// Single recipient: the fork's founder-treasury — no upstream Openfund
	// key and no multi-recipient split.
	require.Len(t, feeMap, 1)
	require.Contains(t, feeMap, founderTreasuryPublicKeyBase58Check)

	// Total fee economics are unchanged: exactly 10 basis points (0.1%).
	var totalBasisPoints uint64
	for _, feeBasisPoints := range feeMap {
		totalBasisPoints += feeBasisPoints
	}
	require.Equal(t, uint64(10), totalBasisPoints)
	require.Equal(t, uint64(10), feeMap[founderTreasuryPublicKeyBase58Check])
}

func TestFounderTreasuryPublicKeyDecodes(t *testing.T) {
	// Guard against typos in the hardcoded recipient: it must decode to a
	// valid compressed public key, exactly like the keys the generic
	// per-market fee path produces.
	pkBytes, _, err := lib.Base58CheckDecode(founderTreasuryPublicKeyBase58Check)
	require.NoError(t, err)
	require.Len(t, pkBytes, btcec.PubKeyBytesLenCompressed)
}
