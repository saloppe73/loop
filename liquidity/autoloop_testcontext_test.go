package liquidity

import (
	"context"
	"testing"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/ticker"
	"github.com/stretchr/testify/assert"
)

type autoloopTestCtx struct {
	t         *testing.T
	manager   *Manager
	lnd       *test.LndMockServices
	testClock *clock.TestClock

	// quoteRequests is a channel that requests for quotes are pushed into.
	quoteRequest chan *loop.LoopOutQuoteRequest

	// quotes is a channel that we get loop out quote requests on.
	quotes chan *loop.LoopOutQuote

	// loopOutRestrictions is a channel that we get the server's
	// restrictions on.
	loopOutRestrictions chan *Restrictions

	// loopOuts is a channel that we get existing loop out swaps on.
	loopOuts chan []*loopdb.LoopOut

	// loopIns is a channel that we get existing loop in swaps on.
	loopIns chan []*loopdb.LoopIn

	// restrictions is a channel that we get swap restrictions on.
	restrictions chan *Restrictions

	// outRequest is a channel that requests to dispatch loop outs are
	// pushed into.
	outRequest chan *loop.OutRequest

	// loopOut is a channel that we return loop out responses on.
	loopOut chan *loop.LoopOutSwapInfo

	// errChan is a channel that we send run errors into.
	errChan chan error

	// cancelCtx cancels the context that our liquidity manager is run with.
	// This can be used to cleanly shutdown the test. Note that this will be
	// nil until the test context has been started.
	cancelCtx func()
}

// newAutoloopTestCtx creates a test context with custom liquidity manager
// parameters and lnd channels.
func newAutoloopTestCtx(t *testing.T, parameters Parameters,
	channels []lndclient.ChannelInfo,
	server *Restrictions) *autoloopTestCtx {

	// Create a mock lnd and set our expected fee rate for sweeps to our
	// sweep fee rate limit value.
	lnd := test.NewMockLnd()
	lnd.SetFeeEstimate(
		defaultParameters.SweepConfTarget,
		defaultParameters.SweepFeeRateLimit,
	)

	testCtx := &autoloopTestCtx{
		t:         t,
		testClock: clock.NewTestClock(testTime),
		lnd:       lnd,

		quoteRequest:        make(chan *loop.LoopOutQuoteRequest),
		quotes:              make(chan *loop.LoopOutQuote),
		loopOutRestrictions: make(chan *Restrictions),
		loopOuts:            make(chan []*loopdb.LoopOut),
		loopIns:             make(chan []*loopdb.LoopIn),
		restrictions:        make(chan *Restrictions),
		outRequest:          make(chan *loop.OutRequest),
		loopOut:             make(chan *loop.LoopOutSwapInfo),

		errChan: make(chan error, 1),
	}

	// Set lnd's channels to equal the set of channels we want for our
	// test.
	testCtx.lnd.Channels = channels

	cfg := &Config{
		AutoloopTicker: ticker.NewForce(DefaultAutoloopTicker),
		Restrictions: func(context.Context, swap.Type) (*Restrictions,
			error) {

			return <-testCtx.loopOutRestrictions, nil
		},
		ListLoopOut: func() ([]*loopdb.LoopOut, error) {
			return <-testCtx.loopOuts, nil
		},
		ListLoopIn: func() ([]*loopdb.LoopIn, error) {
			return <-testCtx.loopIns, nil
		},
		LoopOutQuote: func(_ context.Context,
			req *loop.LoopOutQuoteRequest) (*loop.LoopOutQuote,
			error) {

			testCtx.quoteRequest <- req

			return <-testCtx.quotes, nil
		},
		LoopOut: func(_ context.Context,
			req *loop.OutRequest) (*loop.LoopOutSwapInfo,
			error) {

			testCtx.outRequest <- req

			return <-testCtx.loopOut, nil
		},
		MinimumConfirmations: loop.DefaultSweepConfTarget,
		Lnd:                  &testCtx.lnd.LndServices,
		Clock:                testCtx.testClock,
	}

	// SetParameters needs to make a call to our mocked restrictions call,
	// which will block, so we push our test values in a goroutine.
	done := make(chan struct{})
	go func() {
		testCtx.loopOutRestrictions <- server
		close(done)
	}()

	// Create a manager with our test config and set our starting set of
	// parameters.
	testCtx.manager = NewManager(cfg)
	err := testCtx.manager.SetParameters(context.Background(), parameters)
	assert.NoError(t, err)
	<-done
	return testCtx
}

// start starts our liquidity manager's run loop in a goroutine. Tests should
// be run with test.Guard() to ensure that this does not leak.
func (c *autoloopTestCtx) start() {
	ctx := context.Background()
	ctx, c.cancelCtx = context.WithCancel(ctx)

	go func() {
		c.errChan <- c.manager.Run(ctx)
	}()
}

// stop shuts down our test context and asserts that we have exited with a
// context-cancelled error.
func (c *autoloopTestCtx) stop() {
	c.cancelCtx()
	assert.Equal(c.t, context.Canceled, <-c.errChan)
}

// quoteRequestResp pairs an expected swap quote request with the response we
// would like to provide the liquidity manager with.
type quoteRequestResp struct {
	request *loop.LoopOutQuoteRequest
	quote   *loop.LoopOutQuote
}

// loopOutRequestResp pairs an expected loop out request with the response we
// would like the server to respond with.
type loopOutRequestResp struct {
	request  *loop.OutRequest
	response *loop.LoopOutSwapInfo
}

// autoloop walks our test context through the process of triggering our
// autoloop functionality, providing mocked values as required. The set of
// quotes provided indicates that we expect swap suggestions to be made (since
// we will query for a quote for each suggested swap). The set of expected
// swaps indicates whether we expect any of these swap suggestions to actually
// be dispatched by the autolooper.
func (c *autoloopTestCtx) autoloop(minAmt, maxAmt btcutil.Amount,
	existingOut []*loopdb.LoopOut, quotes []quoteRequestResp,
	expectedSwaps []loopOutRequestResp) {

	// Tick our autoloop ticker to force assessing whether we want to loop.
	c.manager.cfg.AutoloopTicker.Force <- testTime

	// Send a mocked response from the server with the swap size limits.
	c.loopOutRestrictions <- NewRestrictions(minAmt, maxAmt)

	// Provide the liquidity manager with our desired existing set of swaps.
	c.loopOuts <- existingOut
	c.loopIns <- nil

	// Assert that we query the server for a quote for each of our
	// recommended swaps. Note that this differs from our set of expected
	// swaps because we may get quotes for suggested swaps but then just
	// log them.
	for _, expected := range quotes {
		request := <-c.quoteRequest
		assert.Equal(
			c.t, expected.request.Amount, request.Amount,
		)
		assert.Equal(
			c.t, expected.request.SweepConfTarget,
			request.SweepConfTarget,
		)
		c.quotes <- expected.quote
	}

	// Assert that we dispatch the expected set of swaps.
	for _, expected := range expectedSwaps {
		actual := <-c.outRequest

		// Set our destination address to nil so that we do not need to
		// provide the address that is obtained by the mock wallet kit.
		actual.DestAddr = nil

		assert.Equal(c.t, expected.request, actual)
		c.loopOut <- expected.response
	}
}
