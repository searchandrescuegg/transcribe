package slackctl

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/searchandrescuegg/transcribe/internal/config"
	"github.com/searchandrescuegg/transcribe/internal/dragonfly"
	"github.com/searchandrescuegg/transcribe/internal/transcribe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// SlackctlSuite covers the state-mutation halves of CancelTAC and ExtendTAC against a real
// Dragonfly. The Slack-side responses (chat.update, ephemeral replies) live behind the
// slack.Client and are exercised manually; the value here is proving the Dragonfly mutations
// do exactly what cancel/extend promise.

type SlackctlSuite struct {
	suite.Suite

	ctx context.Context

	dragonflyContainer testcontainers.Container
	redisAddr          string
	rdb                *redis.Client

	dfly       *dragonfly.DragonflyClient
	controller *Controller
}

func TestSlackctlSuite(t *testing.T) {
	suite.Run(t, new(SlackctlSuite))
}

func (s *SlackctlSuite) SetupSuite() {
	s.ctx = context.Background()
	dfReq := testcontainers.ContainerRequest{
		Image:        "docker.dragonflydb.io/dragonflydb/dragonfly:latest",
		ExposedPorts: []string{"6379/tcp"},
		// Constrain memory + threads so the test container can co-exist with a running
		// docker-compose Dragonfly. See the matching note in transcribe/integration_test.go.
		Cmd: []string{
			"--proactor_threads=2",
			"--maxmemory=512mb",
		},
		WaitingFor: wait.ForListeningPort("6379/tcp").WithStartupTimeout(60 * time.Second),
	}
	df, err := testcontainers.GenericContainer(s.ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: dfReq,
		Started:          true,
	})
	s.Require().NoError(err)
	s.dragonflyContainer = df

	host, err := df.Host(s.ctx)
	s.Require().NoError(err)
	port, err := df.MappedPort(s.ctx, "6379")
	s.Require().NoError(err)
	s.redisAddr = fmt.Sprintf("%s:%s", host, port.Port())

	s.rdb = redis.NewClient(&redis.Options{Addr: s.redisAddr})
	s.Require().NoError(s.rdb.Ping(s.ctx).Err())
}

func (s *SlackctlSuite) TearDownSuite() {
	if s.rdb != nil {
		_ = s.rdb.Close()
	}
	if s.dragonflyContainer != nil {
		_ = s.dragonflyContainer.Terminate(s.ctx)
	}
}

func (s *SlackctlSuite) SetupTest() {
	s.Require().NoError(s.rdb.FlushDB(s.ctx).Err())

	dfly, err := dragonfly.NewClient(s.ctx, 2*time.Second, &redis.Options{Addr: s.redisAddr})
	s.Require().NoError(err)
	s.dfly = dfly

	// Bypass slackctl.New (which requires SlackAppToken) and construct the controller
	// directly with just the dependencies the state-mutation methods need.
	s.controller = &Controller{
		dfly: dfly,
		cfg: &config.Config{
			TacticalChannelActivationDuration: 30 * time.Minute,
		},
	}
}

// preloadActiveTAC sets up Dragonfly state to mirror what processDispatchCall produces:
// allowed_talkgroups SADDEX, tg:<TGID> Set, active_tacs ZAdd, tac_meta:<TGID> Set.
func (s *SlackctlSuite) preloadActiveTAC(tgid, tac, threadTS string) {
	dur := 30 * time.Minute
	expiresAt := time.Now().Add(dur)

	s.Require().NoError(s.dfly.SAddEx(s.ctx, allowedTalkgroupsKey, dur, tgid))
	s.Require().NoError(s.dfly.Set(s.ctx, fmt.Sprintf(tgRoutingKeyFmt, tgid), dur, threadTS))

	meta := transcribe.ClosureMeta{
		TGID:            tgid,
		TACChannel:      tac,
		ThreadTS:        threadTS,
		SourceTalkgroup: "1399",
		MessageTS:       threadTS,
	}
	payload, _ := json.Marshal(meta)
	s.Require().NoError(s.dfly.Set(s.ctx, fmt.Sprintf(tacMetaKeyFmt, tgid), 24*time.Hour, string(payload)))
	s.Require().NoError(s.dfly.ZAdd(s.ctx, activeTACsKey, float64(expiresAt.Unix()), tgid))
}

// ============================================================================
// CancelTAC
// ============================================================================

func (s *SlackctlSuite) TestCancelTAC_ClearsAllStateAndReturnsMeta() {
	s.preloadActiveTAC("1389", "TAC1", "ts-rescue-1")

	meta, ok, err := s.controller.CancelTAC(s.ctx, "1389")
	s.Require().NoError(err)
	s.True(ok)
	s.Equal("TAC1", meta.TACChannel)
	s.Equal("ts-rescue-1", meta.ThreadTS)

	// allowed_talkgroups should no longer contain 1389 → IsObjectAllowed will reject TAC1 traffic.
	mem, err := s.rdb.SIsMember(s.ctx, allowedTalkgroupsKey, "1389").Result()
	s.Require().NoError(err)
	s.False(mem, "1389 must be removed from allowed_talkgroups")

	// Routing key gone → processNonDispatchCall will report empty thread.
	exists, err := s.rdb.Exists(s.ctx, fmt.Sprintf(tgRoutingKeyFmt, "1389")).Result()
	s.Require().NoError(err)
	s.EqualValues(0, exists, "tg:1389 must be deleted")

	// active_tacs ZSET no longer contains 1389 → sweeper won't post a closure.
	score, err := s.rdb.ZScore(s.ctx, activeTACsKey, "1389").Result()
	s.Equal(redis.Nil, err)
	s.Zero(score)

	// Metadata key cleaned up.
	exists, err = s.rdb.Exists(s.ctx, fmt.Sprintf(tacMetaKeyFmt, "1389")).Result()
	s.Require().NoError(err)
	s.EqualValues(0, exists, "tac_meta:1389 must be deleted")
}

func (s *SlackctlSuite) TestCancelTAC_AlreadyExpired_ReturnsNotOk() {
	// Nothing preloaded — simulate a TAC that already auto-expired before the click landed.
	_, ok, err := s.controller.CancelTAC(s.ctx, "1389")
	s.Require().NoError(err)
	s.False(ok, "no metadata means the TAC is no longer active; caller surfaces this as 'no longer active'")
}

func (s *SlackctlSuite) TestCancelTAC_EmptyTGID_ReturnsError() {
	_, _, err := s.controller.CancelTAC(s.ctx, "")
	s.Require().Error(err)
}

// ============================================================================
// ExtendTAC
// ============================================================================

func (s *SlackctlSuite) TestExtendTAC_RefreshesTTLsAndScore() {
	s.preloadActiveTAC("1389", "TAC1", "ts-rescue-1")

	originalScore, err := s.rdb.ZScore(s.ctx, activeTACsKey, "1389").Result()
	s.Require().NoError(err)

	// Sleep just long enough that the score difference is unambiguously >= 1 second.
	time.Sleep(1100 * time.Millisecond)

	newExpiry, meta, ok, err := s.controller.ExtendTAC(s.ctx, "1389")
	s.Require().NoError(err)
	s.True(ok)
	s.Equal("TAC1", meta.TACChannel)

	// New ZSET score should be strictly greater than the original.
	newScore, err := s.rdb.ZScore(s.ctx, activeTACsKey, "1389").Result()
	s.Require().NoError(err)
	s.Greater(newScore, originalScore, "ExtendTAC must push the closure score forward")
	s.InDelta(float64(newExpiry.Unix()), newScore, 2, "score must match the returned new expiry within rounding")

	// allowed_talkgroups still contains the TGID (extension reaffirms membership).
	mem, err := s.rdb.SIsMember(s.ctx, allowedTalkgroupsKey, "1389").Result()
	s.Require().NoError(err)
	s.True(mem, "extension must keep 1389 in allowed_talkgroups")

	// Routing key still resolves to the original thread_ts (extending must NOT lose context).
	thread, err := s.rdb.Get(s.ctx, fmt.Sprintf(tgRoutingKeyFmt, "1389")).Result()
	s.Require().NoError(err)
	s.Equal("ts-rescue-1", thread)
}

func (s *SlackctlSuite) TestExtendTAC_AlreadyExpired_ReturnsNotOk() {
	_, _, ok, err := s.controller.ExtendTAC(s.ctx, "1389")
	s.Require().NoError(err)
	s.False(ok)
}

// ============================================================================
// SwitchTAC
// ============================================================================

func (s *SlackctlSuite) TestSwitchTAC_MovesAllStateToNewTGID() {
	// Active rescue on TAC1 (1389). Leadership corrects it to TAC8 (1963).
	s.preloadActiveTAC("1389", "TAC1", "ts-rescue-1")

	newMeta, newExpiry, ok, err := s.controller.SwitchTAC(s.ctx, "1389", "1963")
	s.Require().NoError(err)
	s.True(ok)
	s.Equal("1963", newMeta.TGID)
	s.Equal("TAC8", newMeta.TACChannel)
	s.Equal("ts-rescue-1", newMeta.ThreadTS, "thread context must be preserved across switches")
	s.WithinDuration(time.Now().Add(30*time.Minute), newExpiry, 2*time.Second, "new expiry should be a fresh activation window from now")

	// allowed_talkgroups should contain new TGID, not old.
	oldMember, err := s.rdb.SIsMember(s.ctx, allowedTalkgroupsKey, "1389").Result()
	s.Require().NoError(err)
	s.False(oldMember, "old TGID 1389 must be removed from allowed_talkgroups")
	newMember, err := s.rdb.SIsMember(s.ctx, allowedTalkgroupsKey, "1963").Result()
	s.Require().NoError(err)
	s.True(newMember, "new TGID 1963 must be added to allowed_talkgroups")

	// Routing keys: old gone, new present with original thread_ts.
	exists, err := s.rdb.Exists(s.ctx, fmt.Sprintf(tgRoutingKeyFmt, "1389")).Result()
	s.Require().NoError(err)
	s.EqualValues(0, exists, "tg:1389 must be deleted")
	thread, err := s.rdb.Get(s.ctx, fmt.Sprintf(tgRoutingKeyFmt, "1963")).Result()
	s.Require().NoError(err)
	s.Equal("ts-rescue-1", thread, "tg:1963 must point at the original thread_ts")

	// active_tacs ZSET: old gone, new present.
	_, err = s.rdb.ZScore(s.ctx, activeTACsKey, "1389").Result()
	s.Equal(redis.Nil, err, "active_tacs must no longer contain 1389")
	newScore, err := s.rdb.ZScore(s.ctx, activeTACsKey, "1963").Result()
	s.Require().NoError(err)
	s.InDelta(float64(newExpiry.Unix()), newScore, 2)

	// Metadata keys: old gone, new present and decodable.
	exists, err = s.rdb.Exists(s.ctx, fmt.Sprintf(tacMetaKeyFmt, "1389")).Result()
	s.Require().NoError(err)
	s.EqualValues(0, exists, "tac_meta:1389 must be deleted")
	exists, err = s.rdb.Exists(s.ctx, fmt.Sprintf(tacMetaKeyFmt, "1963")).Result()
	s.Require().NoError(err)
	s.EqualValues(1, exists, "tac_meta:1963 must be written")
}

func (s *SlackctlSuite) TestSwitchTAC_SameTGID_ReturnsErrSwitchSameTAC() {
	s.preloadActiveTAC("1389", "TAC1", "ts-rescue-1")
	_, _, _, err := s.controller.SwitchTAC(s.ctx, "1389", "1389")
	s.Require().ErrorIs(err, ErrSwitchSameTAC)

	// State must be untouched.
	mem, err := s.rdb.SIsMember(s.ctx, allowedTalkgroupsKey, "1389").Result()
	s.Require().NoError(err)
	s.True(mem, "state must be untouched on same-TAC pick")
}

func (s *SlackctlSuite) TestSwitchTAC_AlreadyExpired_ReturnsNotOk() {
	// Nothing preloaded.
	_, _, ok, err := s.controller.SwitchTAC(s.ctx, "1389", "1963")
	s.Require().NoError(err)
	s.False(ok, "switching a no-longer-active rescue is a non-error 'no-op' so the user gets a clean ephemeral message")
}

func (s *SlackctlSuite) TestSwitchTAC_UnknownNewTGID_Errors() {
	s.preloadActiveTAC("1389", "TAC1", "ts-rescue-1")
	_, _, _, err := s.controller.SwitchTAC(s.ctx, "1389", "9999")
	s.Require().Error(err)
}

func TestParseOldTGIDFromBlockID(t *testing.T) {
	cases := map[string]struct {
		in   string
		want string
		ok   bool
	}{
		"happy":         {in: "rescue_actions:1389", want: "1389", ok: true},
		"missing colon": {in: "rescue_actions", want: "", ok: false},
		"empty TGID":    {in: "rescue_actions:", want: "", ok: false},
		"wrong prefix":  {in: "other:1389", want: "", ok: false},
		"empty input":   {in: "", want: "", ok: false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := parseOldTGIDFromBlockID(c.in)
			assert.Equal(t, c.want, got)
			assert.Equal(t, c.ok, ok)
		})
	}
}

// ============================================================================
// CloseTAC
// ============================================================================

func (s *SlackctlSuite) TestCloseTAC_TriggersSweeperAndPreservesContext() {
	s.preloadActiveTAC("1389", "TAC1", "ts-rescue-1")

	// Sidecars the sweeper preserves through the close path. summary_data in particular
	// is read by the feedback URL builder during the parent-alert rewrite — premature
	// deletion (the way Cancel does it) would silently strip the prefill.
	s.Require().NoError(s.dfly.Set(s.ctx, fmt.Sprintf(summaryDataKeyFmt, "1389"), 30*time.Minute, `{"headline":"hiker fall","situation_summary":"x"}`))
	s.Require().NoError(s.dfly.Set(s.ctx, fmt.Sprintf(summaryTSKeyFmt, "1389"), 30*time.Minute, "ts-summary-1"))

	meta, ok, err := s.controller.CloseTAC(s.ctx, "1389")
	s.Require().NoError(err)
	s.True(ok)
	s.Equal("TAC1", meta.TACChannel)
	s.Equal("ts-rescue-1", meta.ThreadTS)

	// Routing cut off immediately so further TAC traffic doesn't leak through during the
	// sweeper-tick lag (the per-member SADDEX TTL would otherwise outlive the click).
	mem, err := s.rdb.SIsMember(s.ctx, allowedTalkgroupsKey, "1389").Result()
	s.Require().NoError(err)
	s.False(mem, "1389 must be removed from allowed_talkgroups immediately")
	exists, err := s.rdb.Exists(s.ctx, fmt.Sprintf(tgRoutingKeyFmt, "1389")).Result()
	s.Require().NoError(err)
	s.EqualValues(0, exists, "tg:1389 must be deleted immediately")

	// active_tacs entry now has a past score → sweeper claims it on its next tick.
	score, err := s.rdb.ZScore(s.ctx, activeTACsKey, "1389").Result()
	s.Require().NoError(err)
	s.LessOrEqual(score, float64(time.Now().Unix()), "score must be in the past for the sweeper to claim it")

	// Sidecars MUST survive — sweeper owns these deletions, and the feedback URL builder
	// reads summary_data:<TGID> when it rewrites the parent alert.
	exists, err = s.rdb.Exists(s.ctx, fmt.Sprintf(tacMetaKeyFmt, "1389")).Result()
	s.Require().NoError(err)
	s.EqualValues(1, exists, "tac_meta:1389 must survive — sweeper reads it to post Channel Closed")
	exists, err = s.rdb.Exists(s.ctx, fmt.Sprintf(summaryDataKeyFmt, "1389")).Result()
	s.Require().NoError(err)
	s.EqualValues(1, exists, "summary_data:1389 must survive — feedback URL builder reads it")
	exists, err = s.rdb.Exists(s.ctx, fmt.Sprintf(summaryTSKeyFmt, "1389")).Result()
	s.Require().NoError(err)
	s.EqualValues(1, exists, "summary_ts:1389 must survive until sweeper cleanup")
}

func (s *SlackctlSuite) TestCloseTAC_AlreadyExpired_ReturnsNotOk() {
	_, ok, err := s.controller.CloseTAC(s.ctx, "1389")
	s.Require().NoError(err)
	s.False(ok, "no metadata means the TAC is no longer active; caller surfaces 'no longer active'")
}

func (s *SlackctlSuite) TestCloseTAC_EmptyTGID_ReturnsError() {
	_, _, err := s.controller.CloseTAC(s.ctx, "")
	s.Require().Error(err)
}

// ============================================================================
// Authorization
// ============================================================================

func (s *SlackctlSuite) TestIsAuthorized_AllowAndDeny() {
	c := &Controller{allowed: map[string]struct{}{"U_LEAD": {}, "U_ALSO_LEAD": {}}}
	s.True(c.isAuthorized("U_LEAD"))
	s.True(c.isAuthorized("U_ALSO_LEAD"))
	s.False(c.isAuthorized("U_RANDOM"))
	s.False(c.isAuthorized(""))
}

// FIX (open-to-all option): allowAny=true bypasses the per-user check entirely. Any user
// ID — including the empty string and IDs not in the explicit allowed map — is authorized.
func (s *SlackctlSuite) TestIsAuthorized_AllowAny() {
	c := &Controller{
		allowed:  map[string]struct{}{"U_LEAD": {}}, // residual entries are fine; wildcard wins
		allowAny: true,
	}
	s.True(c.isAuthorized("U_LEAD"))
	s.True(c.isAuthorized("U_RANDOM"))
	s.True(c.isAuthorized("U_ANYONE_AT_ALL"))
}
