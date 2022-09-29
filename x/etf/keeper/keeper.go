package keeper

import (
	"fmt"

	"github.com/defund-labs/defund/x/etf/types"
	"github.com/tendermint/tendermint/libs/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	capabilitykeeper "github.com/cosmos/cosmos-sdk/x/capability/keeper"
	capabilitytypes "github.com/cosmos/cosmos-sdk/x/capability/types"
	icacontrollerkeeper "github.com/cosmos/ibc-go/v4/modules/apps/27-interchain-accounts/controller/keeper"
	icatypes "github.com/cosmos/ibc-go/v4/modules/apps/27-interchain-accounts/types"
	transferkeeper "github.com/cosmos/ibc-go/v4/modules/apps/transfer/keeper"
	clienttypes "github.com/cosmos/ibc-go/v4/modules/core/02-client/types"
	porttypes "github.com/cosmos/ibc-go/v4/modules/core/05-port/types"
	brokertypes "github.com/defund-labs/defund/x/broker/types"
	osmosisgammtypes "github.com/osmosis-labs/osmosis/v8/x/gamm/types"
)

type (
	Keeper struct {
		cdc      codec.BinaryCodec
		storeKey sdk.StoreKey
		memKey   sdk.StoreKey

		scopedKeeper        capabilitykeeper.ScopedKeeper
		accountKeeper       types.AccountKeeper
		bankKeeper          types.BankKeeper
		brokerKeeper        types.BrokerKeeper
		queryKeeper         types.InterqueryKeeper
		channelKeeper       types.ChannelKeeper
		ics4Wrapper         porttypes.ICS4Wrapper
		connectionKeeper    types.ConnectionKeeper
		clientKeeper        types.ClientKeeper
		icaControllerKeeper icacontrollerkeeper.Keeper
		transferKeeper      transferkeeper.Keeper
	}

	Surplus struct {
		BaseDenom      sdk.Coin
		HoldingDenom   sdk.Coin
		Holding        types.Holding
		SurplusPercent sdk.Int
	}
)

func NewKeeper(
	cdc codec.BinaryCodec,
	storeKey,
	memKey sdk.StoreKey,

	accountKeeper types.AccountKeeper,
	bankKeeper types.BankKeeper,
	channelKeeper types.ChannelKeeper,
	interqueryKeeper types.InterqueryKeeper,
	brokerKeeper types.BrokerKeeper,
	connectionKeeper types.ConnectionKeeper,
	clientKeeper types.ClientKeeper,
	iaKeeper icacontrollerkeeper.Keeper,
	transferKeeper transferkeeper.Keeper,
) *Keeper {
	return &Keeper{
		cdc:      cdc,
		storeKey: storeKey,
		memKey:   memKey,

		accountKeeper:       accountKeeper,
		bankKeeper:          bankKeeper,
		channelKeeper:       channelKeeper,
		queryKeeper:         interqueryKeeper,
		brokerKeeper:        brokerKeeper,
		connectionKeeper:    connectionKeeper,
		clientKeeper:        clientKeeper,
		icaControllerKeeper: iaKeeper,
		transferKeeper:      transferKeeper,
	}
}

// ClaimCapability claims the channel capability passed via the OnOpenChanInit callback
func (k *Keeper) ClaimCapability(ctx sdk.Context, cap *capabilitytypes.Capability, name string) error {
	return k.scopedKeeper.ClaimCapability(ctx, cap, name)
}

// SetICS4Wrapper sets the ICS4 wrapper to the keeper.
// It panics if already set
func (k *Keeper) SetICS4Wrapper(ics4Wrapper porttypes.ICS4Wrapper) {
	if k.ics4Wrapper != nil {
		panic("ICS4 wrapper already set")
	}

	k.ics4Wrapper = ics4Wrapper
}

// Logger returns the module logger
func (k Keeper) Logger(ctx sdk.Context) log.Logger {
	return ctx.Logger().With("module", fmt.Sprintf("x/%s", types.ModuleName))
}

// helper function to check if a osmosis pool contains denom specified
func containsAssets(assets []osmosisgammtypes.PoolAsset, denom string) bool {
	for _, asset := range assets {
		if asset.Token.Denom == denom {
			return true
		}
	}

	return false
}

func sum(items []sdk.Dec) sdk.Dec {
	sum := sdk.NewDec(0)
	for _, item := range items {
		sum = sum.Add(item)
	}
	return sum
}

func sumInts(items []sdk.Int) sdk.Int {
	sum := sdk.NewInt(0)
	for _, item := range items {
		sum = sum.Add(item)
	}
	return sum
}

// CreateShares send an IBC transfer to all the brokers for each holding with the proportion of tokenIn
// represented in baseDenom that the broker will then rebalance on the next rebalance.
func (k Keeper) CreateShares(ctx sdk.Context, fund types.Fund, channel string, tokenIn sdk.Coin, creator string, timeoutHeight clienttypes.Height, timeoutTimestamp uint64) error {
	creatorAcc, err := sdk.AccAddressFromBech32(creator)
	if err != nil {
		return err
	}
	fundAcc, err := sdk.AccAddressFromBech32(fund.Address)
	if err != nil {
		return err
	}

	// send the tokenIn to the Defund fund account to ensure that we receive the
	// tokens correctly and instantly to proceed.
	err = k.bankKeeper.SendCoins(ctx, creatorAcc, fundAcc, sdk.NewCoins(tokenIn))
	if err != nil {
		return err
	}

	// for each holding send proportional tokenIn to the holdings broker chain. logic continues in
	// ibc callbacks
	for _, holding := range fund.Holdings {
		broker, found := k.brokerKeeper.GetBroker(ctx, holding.BrokerId)
		if !found {
			return sdkerrors.Wrap(types.ErrWrongBroker, fmt.Sprintf("broker %s not found", holding.BrokerId))
		}

		// get the ica account for the fund on the broker chain
		portID, err := icatypes.NewControllerPortID(fund.Address)
		if err != nil {
			return err
		}
		fundBrokerAddress, found := k.brokerKeeper.GetBrokerAccount(ctx, broker.ConnectionId, portID)
		if !found {
			return sdkerrors.Wrapf(brokertypes.ErrIBCAccountNotExist, "failed to find ica account for owner %s on connection %s and port %s", fund.Address, broker.ConnectionId, portID)
		}

		// Multiply the tokenIn by the % this holding should represent
		sendAmt := tokenIn.Amount.Mul(sdk.NewInt(holding.Percent)).Quo(sdk.NewInt(100))
		sendCoin := sdk.NewCoin(tokenIn.Denom, sendAmt)

		sequence, err := k.SendTransfer(ctx, channel, sendCoin, fund.Address, fundBrokerAddress, timeoutHeight, timeoutTimestamp)
		if err != nil {
			return err
		}
		transfer := brokertypes.Transfer{
			Id:       fmt.Sprintf("%s-%d", channel, sequence),
			Channel:  channel,
			Sequence: sequence,
			Status:   "tranferring",
			Token:    &sendCoin,
			Sender:   fund.Address,
			Receiver: fundBrokerAddress,
		}
		k.brokerKeeper.SetTransfer(ctx, transfer)
	}

	// compute the amount of etf shares this creator is given
	numETFShares, err := k.GetAmountETFSharesForToken(ctx, fund, tokenIn)
	if err != nil {
		return err
	}
	newETFCoins := sdk.NewCoins(numETFShares)

	// finally mint coins (to module account) and then send them to the creator of the create
	err = k.bankKeeper.MintCoins(ctx, types.ModuleName, newETFCoins)
	if err != nil {
		return err
	}
	err = k.bankKeeper.SendCoinsFromModuleToAccount(ctx, types.ModuleName, creatorAcc, newETFCoins)
	if err != nil {
		return err
	}

	// finally reflect the new shares in the fund store for shares
	fund.Shares = fund.Shares.Add(numETFShares)
	k.SetFund(ctx, fund)

	return nil
}

// RedeemShares sends an ICA Send message to each broker chain for each holding to be run on that chain.
// Initializes the redemption of shares process which continues in Broker module in OnAckRec.
func (k Keeper) RedeemShares(ctx sdk.Context, creator string, id string, fund types.Fund, channel string, amount sdk.Coin, fundAccount string, addressMap types.AddressMap) error {
	// Placeholder for current coin to be set below
	currentCoin := sdk.Coin{}
	// Map for holding all the messages for each broker to send later
	msgs := make(map[string][]*banktypes.MsgSend)

	fundDefundAcc, err := sdk.AccAddressFromBech32(fund.Address)
	if err != nil {
		return err
	}

	// get the amount of tokens that these shares represent
	ownership, err := k.GetOwnershipSharesInFund(ctx, fund, amount)
	if err != nil {
		return err
	}

	// Create the redeem store
	redeem := types.Redeem{
		Id:        id,
		Creator:   creator,
		Fund:      &fund,
		Amount:    &amount,
		Status:    "pending",
		Transfers: []brokertypes.Transfer{},
	}

	for _, holding := range fund.Holdings {

		// get the token to redeem for the current holding in loop
		for i, coin := range ownership {
			if coin.Denom == holding.Token {
				currentCoin = coin
				break
			}
			// if we are at the end of the list and it has not broke, we are missing the token. Return error
			if (len(ownership) - 1) == i {
				return sdkerrors.Wrapf(types.ErrInvalidDenom, "could not find supplied token representing holding denom: %s", holding.Token)
			}
		}

		broker, found := k.brokerKeeper.GetBroker(ctx, holding.BrokerId)
		if !found {
			return sdkerrors.Wrap(types.ErrWrongBroker, fmt.Sprintf("broker %s not found", holding.BrokerId))
		}

		portID, err := icatypes.NewControllerPortID(fund.Address)
		if err != nil {
			ctx.Logger().Debug(err.Error())
		}
		addr, found := k.icaControllerKeeper.GetInterchainAccountAddress(ctx, broker.ConnectionId, portID)
		if !found {
			err := status.Errorf(codes.NotFound, "no account found for portID %s on connection %s", portID, broker.ConnectionId)
			return err
		}
		fundICAAddress, err := sdk.AccAddressFromBech32(addr)
		if err != nil {
			return err
		}

		receiveAddress, err := sdk.AccAddressFromBech32(addressMap.OsmosisAddress)
		if err != nil {
			return err
		}

		msg := banktypes.NewMsgSend(fundICAAddress, receiveAddress, sdk.NewCoins(currentCoin))
		if err != nil {
			return err
		}

		msgs[holding.BrokerId] = append(msgs[holding.BrokerId], msg)

		// take the fund etf shares and escrow them in the fund account. in the ack callback, on success
		// of all sequences we will burn these shares. If unsuccessful we will send them back to the user (same on timeout).
		err = k.bankKeeper.SendCoinsFromAccountToModule(ctx, fundDefundAcc, types.ModuleName, sdk.NewCoins(amount))
		if err != nil {
			return err
		}
	}

	// send each ICA message and add it to the redeem which will be used in end blocker
	// to check status of ICA message
	for brokerId, msg := range msgs {
		// get the broker
		broker, found := k.brokerKeeper.GetBroker(ctx, brokerId)
		if !found {
			return sdkerrors.Wrap(types.ErrWrongBroker, fmt.Sprintf("broker %s not found", broker.Id))
		}
		// create the ica multi send message
		sequence, channelICA, err := k.SendIBCSend(ctx, msg, fund.Address, broker.ConnectionId)
		if err != nil {
			return err
		}
		// get the ica account address port
		portID, err := icatypes.NewControllerPortID(fund.Address)
		if err != nil {
			return err
		}
		// get the ica account address
		accAddress, found := k.icaControllerKeeper.GetInterchainAccountAddress(ctx, broker.ConnectionId, portID)
		if !found {
			err := status.Errorf(codes.NotFound, "no account found for portID %s on connection %s", portID, broker.ConnectionId)
			return err
		}
		transfer := brokertypes.Transfer{
			Id:       fmt.Sprintf("%s-%d", channelICA, sequence),
			Channel:  channelICA,
			Sequence: sequence,
			Status:   "tranferring",
			Token:    &currentCoin,
			Sender:   accAddress,
			Receiver: fund.Address,
		}
		redeem.Transfers = append(redeem.Transfers, transfer)
	}

	k.SetRedeem(ctx, redeem)

	return nil
}

// CheckHoldings checks to make sure the specified holdings and the pool for each holding are valid
// by checking the interchain queried pools for the broker specified
func (k Keeper) CheckHoldings(ctx sdk.Context, holdings []types.Holding) error {
	percentCheck := uint64(0)
	for _, holding := range holdings {
		// Add percent composition to percentCheck to later confirm adds to 100%
		percentCheck = percentCheck + uint64(holding.Percent)
		pool, err := k.brokerKeeper.GetOsmosisPool(ctx, holding.PoolId)
		if err != nil {
			return err
		}
		// Checks to see if the holding pool contains the holding token specified and if not returns error
		if !containsAssets(pool.GetAllPoolAssets(), holding.Token) {
			return sdkerrors.Wrapf(types.ErrInvalidDenom, "invalid/unsupported denom (%s) in pool (%d)", holding.Token, holding.PoolId)
		}
	}
	// Make sure all fund holdings add up to 100%
	if percentCheck != uint64(100) {
		return sdkerrors.Wrapf(types.ErrPercentComp, "percent composition must add up to 100 percent")
	}
	return nil
}

// getOsmosisRoutes is a helper function that looks up the Osmosis broker, takes in the currentDenom, needDenom
// and returns a list of the best routes to go through. It does this by first checking if a currentDenom
// has a direct pool with uosmo, if it does not, it then finds a curentDenom -> usomo with uosmo -> needDenom
// pair to create the routes needed to go from currentDenom -> needDenom.
func (k Keeper) getOsmosisRoutes(ctx sdk.Context, currentDenom string, needDenom string) (routes []osmosisgammtypes.SwapAmountInRoute, err error) {
	broker, found := k.brokerKeeper.GetBroker(ctx, "osmosis")
	if !found {
		return nil, sdkerrors.Wrapf(brokertypes.ErrBrokerNotFound, "broker %s not found", "osmosis")
	}
	// for loop to check if there is a direct pool between currentDenom and needDenom
	for _, pool := range broker.Pools {
		osmoPool, err := k.brokerKeeper.GetOsmosisPool(ctx, pool.PoolId)
		if err != nil {
			return routes, err
		}
		poolAssets := osmoPool.GetAllPoolAssets()

		currentDenomCheck := containsAssets(poolAssets, currentDenom)
		wantDenomCheck := containsAssets(poolAssets, needDenom)

		if currentDenomCheck && wantDenomCheck {
			route := osmosisgammtypes.SwapAmountInRoute{
				PoolId:        osmoPool.GetId(),
				TokenOutDenom: needDenom,
			}
			routes = append(routes, route)
			return routes, nil
		}
	}
	// for loop to create a multi pool route. will run if no direct pool can be found. should only
	// contain no more than two routes
	for _, pool := range broker.Pools {
		osmoPool, err := k.brokerKeeper.GetOsmosisPool(ctx, pool.PoolId)
		if err != nil {
			return routes, err
		}
		poolAssets := osmoPool.GetAllPoolAssets()

		currentDenomCheck := containsAssets(poolAssets, currentDenom)
		wantDenomCheck := containsAssets(poolAssets, needDenom)

		if currentDenomCheck {
			route := osmosisgammtypes.SwapAmountInRoute{
				PoolId:        osmoPool.GetId(),
				TokenOutDenom: currentDenom,
			}
			routes = append(routes, route)
		}
		if wantDenomCheck {
			route := osmosisgammtypes.SwapAmountInRoute{
				PoolId:        osmoPool.GetId(),
				TokenOutDenom: needDenom,
			}
			routes = append(routes, route)
		}
		if len(routes) >= 2 {
			break
		}
	}
	return routes, nil
}

// CreateRebalanceMsgs creates the rebalance messages and returns them for the fund in standard interface
// format.
func (k Keeper) CreateRebalanceMsgs(ctx sdk.Context, fund types.Fund) (map[string][]interface{}, error) {

	msgs := make(map[string][]interface{})

	// slice to store the amount of each holding in the base denom as a coin type
	allHoldingsInBaseDenom := []sdk.Coin{}
	// slice to store all the holdings in base denom amounts so we can add them to get total for etf
	allHoldingsInBaseDenomAmount := []sdk.Int{}
	// slice to store all the holdings in holding denom as coin type
	allHoldingsInHoldingDenom := []sdk.Coin{}

	for _, holding := range fund.Holdings {
		broker, found := k.brokerKeeper.GetBroker(ctx, holding.BrokerId)
		if !found {
			return msgs, sdkerrors.Wrap(types.ErrWrongBroker, fmt.Sprintf("broker %s not found", holding.BrokerId))
		}
		// get the ica account for the fund on the broker chain
		portID, err := icatypes.NewControllerPortID(fund.Address)
		if err != nil {
			return msgs, err
		}
		fundBrokerAddress, found := k.brokerKeeper.GetBrokerAccount(ctx, broker.ConnectionId, portID)
		if !found {
			return msgs, sdkerrors.Wrapf(brokertypes.ErrIBCAccountNotExist, "failed to find ica account for owner %s on connection %s and port %s", fund.Address, broker.ConnectionId, portID)
		}
		var balances banktypes.Balance
		// create the balances based on broker
		switch broker.Id {
		case "osmosis":
			// get the account balances for the fund account on the broker chain
			balances, err = k.brokerKeeper.GetOsmosisBalance(ctx, fundBrokerAddress)
			if err != nil {
				return msgs, err
			}
		}
		// get amount of holding token from balances
		amount := balances.GetCoins().AmountOf(holding.Token)
		// get the price of the asset in base denom
		priceInBaseDenom, err := k.brokerKeeper.CalculateOsmosisSpotPrice(ctx, holding.PoolId, holding.Token, fund.BaseDenom)
		if err != nil {
			return msgs, err
		}
		// calculate the amount held of holding in base denom by taking amount and multiplying by price in base denom
		amountInBaseDenom := amount.Quo(priceInBaseDenom.RoundInt())

		// add the pricing/amounts to the slices needed
		allHoldingsInBaseDenomAmount = append(allHoldingsInBaseDenomAmount, amountInBaseDenom)
		allHoldingsInBaseDenom = append(allHoldingsInBaseDenom, sdk.NewCoin(fund.BaseDenom, amountInBaseDenom))
		allHoldingsInHoldingDenom = append(allHoldingsInHoldingDenom, sdk.NewCoin(holding.Token, amount))
	}

	// conversion of slices for convenience
	allHoldingsInBaseDenomCoins := sdk.NewCoins(allHoldingsInBaseDenom...)
	allHoldingsInHoldingDenomCoins := sdk.NewCoins(allHoldingsInHoldingDenom...)
	totalInBaseDenom := sumInts(allHoldingsInBaseDenomAmount)

	// for loop that creates the ICA messages to swap all proportioned surplus holdings to base denom
	for _, holding := range fund.Holdings {
		broker, found := k.brokerKeeper.GetBroker(ctx, holding.BrokerId)
		if !found {
			return msgs, status.Errorf(codes.NotFound, "broker %s not found", holding.BrokerId)
		}
		// get the ica account for the fund on the broker chain
		portID, err := icatypes.NewControllerPortID(fund.Address)
		if err != nil {
			return msgs, err
		}
		fundBrokerAddress, found := k.brokerKeeper.GetBrokerAccount(ctx, broker.ConnectionId, portID)
		if !found {
			return msgs, sdkerrors.Wrapf(brokertypes.ErrIBCAccountNotExist, "failed to find ica account for owner %s on connection %s and port %s", fund.Address, broker.ConnectionId, portID)
		}
		// get the routes needed to swap for from this current denom to base denom
		routes, err := k.getOsmosisRoutes(ctx, holding.Token, fund.BaseDenom)
		if err != nil {
			return msgs, err
		}
		// use some math to get the current composition % for this holding in the fund
		// (holding in base denom / total in base denom)
		currentComposition := allHoldingsInBaseDenomCoins.AmountOf(holding.Token).Quo(totalInBaseDenom)
		// get the surplus composition % by subtracting the current composition % from what its supposed to be
		overUnderCompPerc := currentComposition.Sub(sdk.NewInt(holding.Percent / 100))

		// if we over own the asset
		if overUnderCompPerc.IsPositive() && !overUnderCompPerc.IsZero() {
			// compute the % needed to swap into by multiplying % overUnderCompPerc by the balance of
			// this holding
			needToSwapTokenInHoldingDenom := overUnderCompPerc.Mul(allHoldingsInHoldingDenomCoins.Sort().AmountOf(holding.Token))
			needToSwapTokenInBaseDenomDenom := overUnderCompPerc.Mul(allHoldingsInBaseDenomCoins.Sort().AmountOf(holding.Token))
			// create the tokenIn coin
			tokenIn := sdk.NewCoin(holding.Token, needToSwapTokenInHoldingDenom)
			// create the min amount out by using the current holding amount in base denom and then
			// NOTE: creating a 2% slippage on it (potentially add this as fund param?)
			tokenOut := needToSwapTokenInBaseDenomDenom.Mul(sdk.NewInt(98)).Quo(sdk.NewInt(100))
			// create holder for msg in switch statement
			var msg interface{}
			switch holding.BrokerId {
			case "osmosis":
				msg, err = k.brokerKeeper.CreateOsmosisTrade(ctx, fundBrokerAddress, routes, tokenIn, tokenOut)
				if err != nil {
					return msgs, err
				}
			}
			// append the new message for the broker
			msgs[holding.BrokerId] = append(msgs[holding.BrokerId], msg)
		}
	}

	// for loop that creates the ICA messages to swap from base denom to needed/under owned proportioned holding.
	// We must do this after the positive for loop (above) that creates swaps to base denom so we can
	// run these swaps after the swap to base denom.
	for _, holding := range fund.Holdings {
		broker, found := k.brokerKeeper.GetBroker(ctx, holding.BrokerId)
		if !found {
			return msgs, status.Errorf(codes.NotFound, "broker %s not found", holding.BrokerId)
		}
		// get the ica account for the fund on the broker chain
		portID, err := icatypes.NewControllerPortID(fund.Address)
		if err != nil {
			return msgs, err
		}
		fundBrokerAddress, found := k.brokerKeeper.GetBrokerAccount(ctx, broker.ConnectionId, portID)
		if !found {
			return msgs, sdkerrors.Wrapf(brokertypes.ErrIBCAccountNotExist, "failed to find ica account for owner %s on connection %s and port %s", fund.Address, broker.ConnectionId, portID)
		}
		// get the routes needed to swap for from this current denom to base denom
		routes, err := k.getOsmosisRoutes(ctx, holding.Token, fund.BaseDenom)
		if err != nil {
			return msgs, err
		}
		// use some math to get the current composition % for this holding in the fund
		// (holding in base denom / total in base denom)
		currentComposition := allHoldingsInBaseDenomCoins.AmountOf(holding.Token).Quo(totalInBaseDenom)
		// get the surplus composition % by subtracting the current composition % from what its supposed to be
		overUnderCompPerc := currentComposition.Sub(sdk.NewInt(holding.Percent / 100))

		if overUnderCompPerc.IsNegative() && !overUnderCompPerc.IsZero() {
			// compute the % needed to swap into by multiplying % overUnderCompPerc by the balance of
			// this holding
			needToSwapTokenInHoldingDenom := overUnderCompPerc.Mul(allHoldingsInHoldingDenomCoins.Sort().AmountOf(holding.Token))
			needToSwapTokenInBaseDenomDenom := overUnderCompPerc.Mul(allHoldingsInBaseDenomCoins.Sort().AmountOf(holding.Token))
			// create the tokenIn coin
			tokenIn := sdk.NewCoin(fund.BaseDenom, needToSwapTokenInBaseDenomDenom)
			// create the min amount out by using the current holding amount in base denom and then
			// NOTE: creating a 2% slippage on it (potentially add this as fund param?)
			tokenOut := needToSwapTokenInHoldingDenom.Mul(sdk.NewInt(98)).Quo(sdk.NewInt(100))
			// create holder for msg in switch statement
			var msg interface{}
			switch holding.BrokerId {
			case "osmosis":
				msg, err = k.brokerKeeper.CreateOsmosisTrade(ctx, fundBrokerAddress, routes, tokenIn, tokenOut)
				if err != nil {
					return msgs, err
				}
			}
			// append the new message for the broker
			msgs[holding.BrokerId] = append(msgs[holding.BrokerId], msg)
		}
	}

	return msgs, nil
}

// SendRebalanceTx sends an ICA tx to each broker chain for each msg that will rebalance the underlying
// holdings for the fund
func (k Keeper) SendRebalanceTx(ctx sdk.Context, fund types.Fund) error {

	// Create the rebalance messages
	msgs, err := k.CreateRebalanceMsgs(ctx, fund)
	if err != nil {
		return err
	}

	// send msgs for each broker
	for brokerId, msg := range msgs {
		broker, found := k.brokerKeeper.GetBroker(ctx, brokerId)
		if !found {
			return sdkerrors.Wrap(types.ErrWrongBroker, fmt.Sprintf("broker %s not found", brokerId))
		}
		// send messages depending on the broker
		switch brokerId {
		case "osmosis":
			// convert the standard interface to osmosis messages
			osmoMsgs := []*osmosisgammtypes.MsgSwapExactAmountIn{}
			for i, m := range msg {
				newMsg, ok := m.(osmosisgammtypes.MsgSwapExactAmountIn)
				if !ok {
					return sdkerrors.Wrap(types.ErrMarshallingError, fmt.Sprintf("SendRebalanceTx: could not convert message at index %d to osmosis msg", i))
				}
				osmoMsgs = append(osmoMsgs, &newMsg)
			}
			// send the trade through ICA
			_, err := k.brokerKeeper.SendOsmosisTrades(ctx, osmoMsgs, fund.Address, broker.ConnectionId)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
