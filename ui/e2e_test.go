//go:build e2e

// Mandatory browser smoke for Slice 2a (run: make e2e-ui). It is the only test that
// proves the embed + SPA + ELK render path actually works in a browser -- Go API tests
// are blind to the frontend. Per the eng review it FAILS (not skips) if no headless
// browser is available: the slice's value is unproven without it. It also builds the
// harness Slice 2b's live-overlay E2E reuses.
package ui

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/valbaudo/awf/engine"
	"github.com/valbaudo/awf/state"
)

func TestE2ERenderAndSnapshotOverlay(t *testing.T) {
	dir := t.TempDir()
	// A completed run so the overlay paints node "build" green (data-state=completed).
	writeRunLog(t, dir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{RunID: "r1", WorkflowDigest: testDigest})},
		state.Event{Type: engine.EventNodeStarted, Path: "build", Data: mustData(engine.NodeStartedData{Kind: "code"})},
		state.Event{Type: engine.EventNodeCompleted, Path: "build", Data: mustData(engine.NodeCompletedData{Outcome: "ok"})},
		state.Event{Type: engine.EventRunFinished, Data: mustData(engine.RunFinishedData{Outcome: "ok"})},
	)
	ts := httptest.NewServer(New(demoWorkflow(), testDigest, dir).Handler())
	defer ts.Close()

	// Default allocator is headless and locates Chrome/Chromium; if none is present
	// chromedp.Run errors and we fail (not skip) per the review decision.
	ctx, cancel := chromedp.NewContext(context.Background())
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	err := chromedp.Run(ctx,
		chromedp.Navigate(ts.URL+"/?run=r1"),
		// graph rendered: at least one node carries the testable data-node-path attr.
		chromedp.WaitVisible(`[data-node-path]`, chromedp.ByQuery),
		// overlay applied via the restyle path: "build" reached its terminal state.
		chromedp.WaitVisible(`[data-node-path="build"][data-state="completed"]`, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("browser E2E failed (need a headless Chrome/Chromium; this is a ship gate, not a skip): %v", err)
	}
}

// TestE2ELiveOverlay is the MANDATORY live test for Slice 2b: with the browser open and
// NO manual refresh, a node must transition running -> completed when the run log changes,
// driven purely by the SSE stream. This is the whole point of the slice.
func TestE2ELiveOverlay(t *testing.T) {
	fastPoll(t) // shorten the server's SSE poll so the test isn't slow
	dir := t.TempDir()
	// Start with build in-flight (running): node.started, no terminal event.
	writeRunLog(t, dir, "r1",
		state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{RunID: "r1", WorkflowDigest: testDigest})},
		state.Event{Type: engine.EventNodeStarted, Path: "build", Data: mustData(engine.NodeStartedData{Kind: "code"})},
	)
	ts := httptest.NewServer(New(demoWorkflow(), testDigest, dir).Handler())
	defer ts.Close()

	ctx, cancel := chromedp.NewContext(context.Background())
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	err := chromedp.Run(ctx,
		chromedp.Navigate(ts.URL+"/?run=r1"),
		chromedp.WaitVisible(`[data-node-path="build"][data-state="running"]`, chromedp.ByQuery),
		// Mutate the run while the page is open; the SSE stream must restyle the node.
		chromedp.ActionFunc(func(context.Context) error {
			writeRunLog(t, dir, "r1",
				state.Event{Type: engine.EventRunStarted, Data: mustData(engine.RunStartedData{RunID: "r1", WorkflowDigest: testDigest})},
				state.Event{Type: engine.EventNodeStarted, Path: "build", Data: mustData(engine.NodeStartedData{Kind: "code"})},
				state.Event{Type: engine.EventNodeCompleted, Path: "build", Data: mustData(engine.NodeCompletedData{Outcome: "ok"})},
			)
			return nil
		}),
		// No reload, no Refresh click: the SSE push alone flips the node to completed.
		chromedp.WaitVisible(`[data-node-path="build"][data-state="completed"]`, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatalf("live overlay E2E failed (ship gate, not a skip): %v", err)
	}
}
