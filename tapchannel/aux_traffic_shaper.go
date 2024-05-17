package tapchannel

import (
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/fn"
	"github.com/lightninglabs/taproot-assets/rfq"
	"github.com/lightninglabs/taproot-assets/rfqmsg"
	cmsg "github.com/lightninglabs/taproot-assets/tapchannelmsg"
	lfn "github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// DefaultOnChainHtlcAmount is the default amount that we consider
	// as the smallest HTLC amount that can be sent on-chain. This needs to
	// be greater than the dust limit for an HTLC.
	DefaultOnChainHtlcAmount = lnwallet.DustLimitForSize(
		input.UnknownWitnessSize,
	)
)

// TrafficShaperConfig defines the configuration for the auxiliary traffic
// shaper.
type TrafficShaperConfig struct {
	ChainParams *address.ChainParams

	RfqManager *rfq.Manager
}

// AuxTrafficShaper is a Taproot Asset auxiliary traffic shaper that can be used
// to make routing decisions for Taproot Asset channels.
type AuxTrafficShaper struct {
	startOnce sync.Once
	stopOnce  sync.Once

	cfg *TrafficShaperConfig

	// ContextGuard provides a wait group and main quit channel that can be
	// used to create guarded contexts.
	*fn.ContextGuard
}

// NewAuxTrafficShaper creates a new Taproot Asset auxiliary traffic shaper
// based on the passed config.
func NewAuxTrafficShaper(cfg *TrafficShaperConfig) *AuxTrafficShaper {
	return &AuxTrafficShaper{
		cfg: cfg,
		ContextGuard: &fn.ContextGuard{
			DefaultTimeout: DefaultTimeout,
			Quit:           make(chan struct{}),
		},
	}
}

// Start attempts to start a new aux traffic shaper.
func (s *AuxTrafficShaper) Start() error {
	var startErr error
	s.startOnce.Do(func() {
		log.Info("Starting aux traffic shaper")
	})
	return startErr
}

// Stop signals for an aux traffic shaper to gracefully exit.
func (s *AuxTrafficShaper) Stop() error {
	var stopErr error
	s.stopOnce.Do(func() {
		log.Info("Stopping aux traffic shaper")

		close(s.Quit)
		s.Wg.Wait()
	})

	return stopErr
}

// A compile-time check to ensure that AuxTrafficShaper fully implements the
// routing.TlvTrafficShaper interface.
var _ routing.TlvTrafficShaper = (*AuxTrafficShaper)(nil)

// HandleTraffic is called in order to check if the channel identified by the
// provided channel ID is handled by the traffic shaper implementation. If it
// is handled by the traffic shaper, then the normal bandwidth calculation can
// be skipped and the bandwidth returned by PaymentBandwidth should be used
// instead.
func (s *AuxTrafficShaper) HandleTraffic(_ lnwire.ShortChannelID,
	fundingBlob lfn.Option[tlv.Blob]) (bool, error) {

	// If there is no auxiliary blob in the channel, it's not a custom
	// channel, and we don't need to handle it.
	if fundingBlob.IsNone() {
		return false, nil
	}

	// If we can successfully decode the channel blob as a channel capacity
	// information, we know that this is a custom channel.
	err := lfn.MapOptionZ(fundingBlob, func(blob tlv.Blob) error {
		_, err := cmsg.DecodeOpenChannel(blob)
		return err
	})
	if err != nil {
		return false, err
	}

	// No error, so this is a custom channel, we'll want to decide.
	return true, nil
}

// PaymentBandwidth returns the available bandwidth for a custom channel decided
// by the given channel aux blob and HTLC blob. A return value of 0 means there
// is no bandwidth available. To find out if a channel is a custom channel that
// should be handled by the traffic shaper, the HandleTraffic method should be
// called first.
func (s *AuxTrafficShaper) PaymentBandwidth(htlcBlob,
	commitmentBlob lfn.Option[tlv.Blob]) (lnwire.MilliSatoshi, error) {

	// This method shouldn't be called if we don't have a commitment blob
	// available (i.e., the channel is not a custom channel).
	if commitmentBlob.IsNone() || htlcBlob.IsNone() {
		return 0, fmt.Errorf("no commitment or TLV blob available " +
			"for custom channel bandwidth estimation")
	}

	commitmentBytes := commitmentBlob.UnsafeFromSome()
	htlcBytes := htlcBlob.UnsafeFromSome()

	commitment, err := cmsg.DecodeCommitment(commitmentBytes)
	if err != nil {
		return 0, fmt.Errorf("error decoding commitment blob: %w", err)
	}

	htlc, err := rfqmsg.DecodeHtlc(htlcBytes)
	if err != nil {
		return 0, fmt.Errorf("error decoding HTLC blob: %w", err)
	}

	localBalance := cmsg.OutputSum(commitment.LocalOutputs())

	// There either already is an amount set in the HTLC (which would
	// indicate it to be a direct-channel keysend payment that just sends
	// assets to the direct peer with no conversion), in which case we don't
	// need an RFQ ID as we can just compare the local balance and the
	// required HTLC amount. If there is no amount set, we need to look up
	// the RFQ ID in the HTLC blob and use the accepted quote to determine
	// the amount.
	htlcAssetAmount := htlc.Amounts.Val.Sum()
	if htlcAssetAmount != 0 && htlcAssetAmount <= localBalance {
		// We signal "infinite" bandwidth by returning a very high
		// value (number of Satoshis ever in existence), since we might
		// not have a quote available to know what the asset amount
		// means in terms of satoshis. But the satoshi amount doesn't
		// matter too much here, we just want to signal that this
		// channel _does_ have available bandwidth.
		return lnwire.NewMSatFromSatoshis(btcutil.MaxSatoshi), nil
	}

	// If the HTLC doesn't have an asset amount and RFQ ID, it's incomplete,
	// and we cannot determine what channel to use.
	if htlc.RfqID.ValOpt().IsNone() {
		return 0, nil
	}

	// For every other use case (i.e. a normal payment with a negotiated
	// quote or a multi-hop keysend that also uses a quote), we need to look
	// up the accepted quote and determine the outgoing bandwidth in
	// satoshis based on the local asset balance.
	rfqID := htlc.RfqID.ValOpt().UnsafeFromSome()
	acceptedQuotes := s.cfg.RfqManager.PeerAcceptedSellQuotes()
	quote, ok := acceptedQuotes[rfqID.Scid()]
	if !ok {
		return 0, fmt.Errorf("no accepted quote found for RFQ ID "+
			"%x (SCID %d)", rfqID[:], rfqID.Scid())
	}

	mSatPerAssetUnit := quote.BidPrice

	// The available balance is the local asset unit expressed in
	// milli-satoshis.
	return lnwire.MilliSatoshi(localBalance) * mSatPerAssetUnit, nil
}

// ProduceHtlcExtraData is a function that, based on the previous custom record
// blob of an HTLC, may produce a different blob or modify the amount of bitcoin
// this HTLC should carry.
func (s *AuxTrafficShaper) ProduceHtlcExtraData(totalAmount lnwire.MilliSatoshi,
	htlcCustomRecords lnwire.CustomRecords) (lnwire.MilliSatoshi, tlv.Blob,
	error) {

	if len(htlcCustomRecords) == 0 {
		return totalAmount, nil, nil
	}

	// We need to do a round trip to convert the custom records to a blob
	// that we can then parse into the correct struct again.
	htlcBlob, err := htlcCustomRecords.Serialize()
	if err != nil {
		return 0, nil, fmt.Errorf("error serializing HTLC blob: %w",
			err)
	}

	htlc, err := rfqmsg.DecodeHtlc(htlcBlob)
	if err != nil {
		return 0, nil, fmt.Errorf("error decoding HTLC blob: %w", err)
	}

	// If we already have an asset amount in the HTLC, we assume this is a
	// keysend payment and don't need to do anything. We even return the
	// original on-chain amount as we don't want to change it.
	if htlc.Amounts.Val.Sum() > 0 {
		return totalAmount, htlcBlob, nil
	}

	if htlc.RfqID.ValOpt().IsNone() {
		return 0, nil, fmt.Errorf("no RFQ ID present in HTLC blob")
	}

	rfqID := htlc.RfqID.ValOpt().UnsafeFromSome()
	acceptedQuotes := s.cfg.RfqManager.PeerAcceptedSellQuotes()
	quote, ok := acceptedQuotes[rfqID.Scid()]
	if !ok {
		return 0, nil, fmt.Errorf("no accepted quote found for RFQ ID "+
			"%x (SCID %d)", rfqID[:], rfqID.Scid())
	}

	// Now that we've queried the accepted quote, we know how many asset
	// units we need to send. This is the main purpose of this method: We
	// convert the BTC amount originally intended to be sent out into the
	// corresponding number of assets, then reduce the number of satoshis of
	// the HTLC to the bare minimum that can be materialized on chain.
	// The bid price is in milli-satoshis per asset unit. We round to the
	// nearest 10 units to avoid more than half an asset unit of rounding
	// error that we would get if we did normal integer division (rounding
	// down).
	mSatPerAssetUnit := quote.BidPrice
	numAssetUnits := uint64(totalAmount*10/mSatPerAssetUnit) / 10

	// We now know how many units we need. We take the asset ID from the
	// RFQ so the recipient can match it back to the quote.
	if quote.AssetID == nil {
		return 0, nil, fmt.Errorf("quote has no asset ID")
	}

	log.Debugf("Producing HTLC extra data for RFQ ID %x (SCID %d): "+
		"asset ID %x, asset amount %d", rfqID[:], rfqID.Scid(),
		quote.AssetID[:], numAssetUnits)

	htlc.Amounts.Val.Balances = []*rfqmsg.AssetBalance{
		rfqmsg.NewAssetBalance(*quote.AssetID, numAssetUnits),
	}

	// Encode the updated HTLC TLV back into a blob and return it with the
	// amount that should be sent on-chain, which is a value in satoshi that
	// is just above the dust limit.
	htlcAmountMSat := lnwire.NewMSatFromSatoshis(DefaultOnChainHtlcAmount)
	return htlcAmountMSat, htlc.Bytes(), nil
}
