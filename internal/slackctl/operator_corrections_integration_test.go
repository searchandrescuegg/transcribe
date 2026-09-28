package slackctl

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/searchandrescuegg/transcribe/internal/transcribe"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// buildCorrectionSocketEvent constructs a socketmode Events API envelope carrying a single
// `message` inner event — the shape socketmode's eventAPIDispatcher hands to a registered
// EventTypeEventsAPI handler. Request is left nil (no ack attempted), matching how a handler
// invoked directly in a test — rather than through the real WebSocket loop — is exercised.
func buildCorrectionSocketEvent(channel, eventID, userID, text, ts, threadTS string) *socketmode.Event {
	return &socketmode.Event{
		Type: socketmode.EventTypeEventsAPI,
		Data: slackevents.EventsAPIEvent{
			Data: &slackevents.EventsAPICallbackEvent{EventID: eventID},
			InnerEvent: slackevents.EventsAPIInnerEvent{
				Data: &slackevents.MessageEvent{
					Channel: channel,
					Message: &slack.Msg{
						User:            userID,
						Text:            text,
						Timestamp:       ts,
						ThreadTimestamp: threadTS,
					},
				},
			},
		},
	}
}

// reactionRecorder is a minimal reactions.add stand-in: it records the `name` form value of
// every request and always replies ok so AddReactionContext returns cleanly.
type reactionRecorder struct {
	mu    sync.Mutex
	names []string
}

func newReactionServer() (*httptest.Server, *reactionRecorder) {
	rec := &reactionRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		rec.mu.Lock()
		rec.names = append(rec.names, r.FormValue("name"))
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	return srv, rec
}

func (r *reactionRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.names...)
}

// ============================================================================
// dispatchEvent: dedup
// ============================================================================

func (s *SlackctlSuite) TestDispatchEvent_DedupsBySlackEventID() {
	const ch = "C1"
	s.controller.cfg.SlackChannelID = ch
	s.controller.allowAny = true // authorization isn't what this test is about

	fake := &fakeCorrections{upsertTGID: "1389", upsertCreated: false}
	s.controller.corrections = fake

	evt := buildCorrectionSocketEvent(ch, "Ev1", "U_LEAD", "correction: fixed ankle", "200.100", "100.000")

	// dispatchEvent runs its actual work in a goroutine (see operator_corrections.go), so both
	// calls return immediately — the redelivery must be caught by the SETNX dedup guard, not
	// by anything synchronous in dispatchEvent itself.
	s.controller.dispatchEvent(evt, nil)
	s.controller.dispatchEvent(evt, nil)

	s.Eventually(func() bool {
		return fake.upsertCallCount() == 1
	}, 2*time.Second, 20*time.Millisecond, "expected the redelivered event to be deduped, not processed twice")

	// Give a redelivery a fair chance to sneak in before declaring victory.
	time.Sleep(150 * time.Millisecond)
	s.Equal(1, fake.upsertCallCount(), "a second dispatchEvent call for the same event_id must be a no-op")

	ttl, err := s.rdb.TTL(s.ctx, fmt.Sprintf(slackEventDedupKeyFmt, "Ev1")).Result()
	s.Require().NoError(err)
	s.InDelta(float64(slackEventDedupTTL), float64(ttl), float64(30*time.Second), "dedup key TTL must be ~10m")
}

func (s *SlackctlSuite) TestDispatchEvent_UnauthorizedUser_ServiceNeverCalled() {
	const ch = "C1"
	s.controller.cfg.SlackChannelID = ch
	s.controller.allowed = map[string]struct{}{"U_LEAD": {}}
	s.controller.allowAny = false

	fake := &fakeCorrections{}
	s.controller.corrections = fake

	evt := buildCorrectionSocketEvent(ch, "Ev2", "U_RANDOM", "correction: fixed ankle", "200.100", "100.000")

	// The authorization check runs synchronously in dispatchEvent before any goroutine is
	// spawned, so — unlike the dedup test — there is nothing async to wait out here.
	s.controller.dispatchEvent(evt, nil)
	time.Sleep(100 * time.Millisecond)

	s.Equal(0, fake.upsertCallCount(), "an unauthorized user's correction must never reach CorrectionService")
}

// ============================================================================
// handleCorrectionOp: upsert branching
// ============================================================================

func (s *SlackctlSuite) TestHandleCorrectionOp_Upsert_Created_ReactsAndRefreshesAdditively() {
	const ch = "C1"
	s.controller.cfg.SlackChannelID = ch
	srv, rec := newReactionServer()
	defer srv.Close()
	s.controller.slackClient = slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))

	fake := &fakeCorrections{upsertTGID: "1389", upsertCreated: true}
	s.controller.corrections = fake

	op := correctionOp{Kind: correctionOpUpsert, ThreadTS: "100.0", MessageTS: "200.1", UserID: "U1", Text: "fixed ankle"}
	s.controller.handleCorrectionOp(s.ctx, op)

	upserts, _, refreshes := fake.snapshot()
	s.Require().Len(upserts, 1)
	s.Equal("100.0", upserts[0].threadTS)
	s.Equal("200.1", upserts[0].slackTS)
	s.Equal("U1", upserts[0].userID)
	s.Equal("fixed ankle", upserts[0].text)

	s.Require().Len(refreshes, 1)
	s.Equal("1389", refreshes[0].tgid)
	s.False(refreshes[0].rewrite, "a brand-new note is additive — the summary must not be force-rewritten")

	s.Eventually(func() bool {
		return len(rec.snapshot()) == 1
	}, time.Second, 10*time.Millisecond)
	s.Equal([]string{"white_check_mark"}, rec.snapshot())
}

func (s *SlackctlSuite) TestHandleCorrectionOp_Upsert_Amended_RefreshesWithRewriteAndNoReaction() {
	const ch = "C1"
	s.controller.cfg.SlackChannelID = ch
	srv, rec := newReactionServer()
	defer srv.Close()
	s.controller.slackClient = slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))

	fake := &fakeCorrections{upsertTGID: "1389", upsertCreated: false}
	s.controller.corrections = fake

	op := correctionOp{Kind: correctionOpUpsert, ThreadTS: "100.0", MessageTS: "200.1", UserID: "U1", Text: "amended text"}
	s.controller.handleCorrectionOp(s.ctx, op)

	_, _, refreshes := fake.snapshot()
	s.Require().Len(refreshes, 1)
	s.Equal("1389", refreshes[0].tgid)
	s.True(refreshes[0].rewrite, "amending an existing note may retract facts, so the summary must re-derive")

	// Give any (wrongly fired) reaction request a moment to land, then confirm none did.
	time.Sleep(150 * time.Millisecond)
	s.Empty(rec.snapshot(), "an amendment to an already-live note must not re-fire the check-mark reaction")
}

func (s *SlackctlSuite) TestHandleCorrectionOp_Upsert_RescueNotActive_NoRefreshNoReaction() {
	const ch = "C1"
	s.controller.cfg.SlackChannelID = ch
	srv, rec := newReactionServer()
	defer srv.Close()
	s.controller.slackClient = slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))

	fake := &fakeCorrections{upsertErr: transcribe.ErrRescueNotActive}
	s.controller.corrections = fake

	op := correctionOp{Kind: correctionOpUpsert, ThreadTS: "100.0", MessageTS: "200.1", UserID: "U1", Text: "fixed ankle"}
	s.controller.handleCorrectionOp(s.ctx, op)

	_, _, refreshes := fake.snapshot()
	s.Empty(refreshes, "a thread that no longer maps to an active rescue must not trigger a refresh")

	time.Sleep(150 * time.Millisecond)
	s.Empty(rec.snapshot(), "no reaction should be posted when the rescue is no longer active")
}

func (s *SlackctlSuite) TestHandleCorrectionOp_Upsert_OtherError_WarningReactionNoRefresh() {
	const ch = "C1"
	s.controller.cfg.SlackChannelID = ch
	srv, rec := newReactionServer()
	defer srv.Close()
	s.controller.slackClient = slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))

	fake := &fakeCorrections{upsertErr: errors.New("dragonfly is on fire")}
	s.controller.corrections = fake

	op := correctionOp{Kind: correctionOpUpsert, ThreadTS: "100.0", MessageTS: "200.1", UserID: "U1", Text: "fixed ankle"}
	s.controller.handleCorrectionOp(s.ctx, op)

	_, _, refreshes := fake.snapshot()
	s.Empty(refreshes, "a genuine store failure must not trigger a refresh")

	s.Eventually(func() bool {
		return len(rec.snapshot()) == 1
	}, time.Second, 10*time.Millisecond)
	s.Equal([]string{"warning"}, rec.snapshot())
}

// ============================================================================
// handleCorrectionOp: remove branching
// ============================================================================

func (s *SlackctlSuite) TestHandleCorrectionOp_Remove_Removed_Refreshes() {
	fake := &fakeCorrections{removeTGID: "1389", removeRemoved: true}
	s.controller.corrections = fake

	op := correctionOp{Kind: correctionOpRemove, ThreadTS: "100.0", MessageTS: "200.1", UserID: "U1"}
	s.controller.handleCorrectionOp(s.ctx, op)

	_, removes, refreshes := fake.snapshot()
	s.Require().Len(removes, 1)
	s.Equal("100.0", removes[0].threadTS)
	s.Equal("200.1", removes[0].slackTS)
	s.Equal("U1", removes[0].userID)

	s.Require().Len(refreshes, 1)
	s.Equal("1389", refreshes[0].tgid)
	s.True(refreshes[0].rewrite, "removing a live note must always force a re-derived summary")
}

func (s *SlackctlSuite) TestHandleCorrectionOp_Remove_NothingToRemove_NoRefresh() {
	fake := &fakeCorrections{removeTGID: "1389", removeRemoved: false}
	s.controller.corrections = fake

	op := correctionOp{Kind: correctionOpRemove, ThreadTS: "100.0", MessageTS: "200.1", UserID: "U1"}
	s.controller.handleCorrectionOp(s.ctx, op)

	_, _, refreshes := fake.snapshot()
	s.Empty(refreshes, "removing an already-tombstoned (or never-live) note must not refresh")
}

// An edit whose text is unchanged (Slack link unfurl etc.) is a no-op: no refresh, no reaction.
func (s *SlackctlSuite) TestHandleCorrectionOp_Upsert_Unchanged_NoRefreshNoReaction() {
	const ch = "C1"
	s.controller.cfg.SlackChannelID = ch
	srv, rec := newReactionServer()
	defer srv.Close()
	s.controller.slackClient = slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))

	fake := &fakeCorrections{upsertTGID: "1389", upsertErr: transcribe.ErrOperatorCorrectionUnchanged}
	s.controller.corrections = fake

	op := correctionOp{Kind: correctionOpUpsert, ThreadTS: "100.0", MessageTS: "200.1", UserID: "U1", Text: "same text"}
	s.controller.handleCorrectionOp(s.ctx, op)

	_, _, refreshes := fake.snapshot()
	s.Empty(refreshes, "an unchanged edit must not force a summary rewrite")
	time.Sleep(150 * time.Millisecond)
	s.Empty(rec.snapshot(), "an unchanged edit must not react (not even a warning)")
}

// Two operations on the same Slack message are serialized: the second must not enter the
// service until the first returns (else an edit's stale read can resurrect a retracted note).
func (s *SlackctlSuite) TestHandleCorrectionOp_SameMessage_Serialized() {
	fake := &fakeCorrections{upsertTGID: "1389", upsertCreated: false,
		upsertEntered: make(chan struct{}, 2), upsertRelease: make(chan struct{})}
	s.controller.corrections = fake
	op := correctionOp{Kind: correctionOpUpsert, ThreadTS: "100.0", MessageTS: "200.1", UserID: "U1", Text: "x"}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.controller.handleCorrectionOp(s.ctx, op) }()
	select {
	case <-fake.upsertEntered:
	case <-time.After(2 * time.Second):
		s.FailNow("first op never entered the service")
	}
	go func() { defer wg.Done(); s.controller.handleCorrectionOp(s.ctx, op) }()

	select {
	case <-fake.upsertEntered:
		s.Fail("second op entered the service while the first was still inside it")
	case <-time.After(200 * time.Millisecond):
	}
	s.Equal(1, fake.upsertCallCount())

	close(fake.upsertRelease)
	select {
	case <-fake.upsertEntered:
	case <-time.After(2 * time.Second):
		s.Fail("second op never ran after the first returned")
	}
	wg.Wait()
	s.Equal(2, fake.upsertCallCount())
}
