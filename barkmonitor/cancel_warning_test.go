package barkmonitor

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	"go.viam.com/rdk/components/audioout"
	"go.viam.com/rdk/logging"
	rutils "go.viam.com/rdk/utils"

	"github.com/katie-viam/oreo-watcher/filteredmic"
)

// countingSpeaker counts sounds played and stop commands received.
type countingSpeaker struct {
	audioout.AudioOut
	plays atomic.Int32
	stops atomic.Int32
}

func (s *countingSpeaker) Play(context.Context, []byte, *rutils.AudioInfo, map[string]interface{}) error {
	s.plays.Add(1)
	return nil
}

func (s *countingSpeaker) DoCommand(_ context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if cmd["stop"] == true {
		s.stops.Add(1)
	}
	return nil, nil
}

type neverMutedMic struct{ filteredmic.AudioCapture }

func (neverMutedMic) Mute(time.Duration) {}

func newSessionTestMonitor(t *testing.T) (*barkMonitor, *countingSpeaker) {
	t.Helper()
	speaker := &countingSpeaker{}
	b := &barkMonitor{
		speaker:             speaker,
		audioCache:          neverMutedMic{},
		camerasAllowed:      func() bool { return false },
		recordingDir:        t.TempDir(),
		logger:              logging.NewTestLogger(t),
		defaultWarnSound:    leaveItSound,
		defaultGoodJobSound: goodJobSound,
		gapDur:              40 * time.Millisecond,
		warnDur:             time.Hour,
		actionDur:           time.Hour,
		clipPad:             time.Millisecond,
		closed:              make(chan struct{}),
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return b, speaker
}

func waitForCompletedSession(t *testing.T, b *barkMonitor) *completedSession {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b.mu.Lock()
		session := b.pending
		b.mu.Unlock()
		if session != nil {
			return session
		}
		if time.Now().After(deadline) {
			t.Fatal("bark session never ended")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestSessionPlaysWarningThenGoodJob(t *testing.T) {
	b, speaker := newSessionTestMonitor(t)
	b.onBarkDetected(-20, time.Time{}, []string{"oreo"})

	if session := waitForCompletedSession(t, b); session.WarningsCancelled {
		t.Error("session reported as cancelled though nobody cancelled it")
	}
	deadline := time.Now().Add(5 * time.Second)
	for speaker.plays.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("speaker played %d sounds, want the warning and the good-job sound", speaker.plays.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestCancelWarningSilencesTheRestOfTheSession(t *testing.T) {
	b, speaker := newSessionTestMonitor(t)
	b.onBarkDetected(-20, time.Time{}, []string{"oreo"})

	resp, err := b.DoCommand(context.Background(), map[string]interface{}{"command": "cancel_warning"})
	if err != nil {
		t.Fatal(err)
	}
	if resp["cancelled"] != true {
		t.Errorf("resp = %v, want cancelled during an active session", resp)
	}
	if speaker.stops.Load() != 1 {
		t.Errorf("speaker received %d stop commands, want 1", speaker.stops.Load())
	}

	session := waitForCompletedSession(t, b)
	if !session.WarningsCancelled {
		t.Error("completed session does not record that its warnings were cancelled")
	}
	if sessionReading(session)["warnings_cancelled"] != true {
		t.Error("session reading does not report warnings_cancelled")
	}
	// The good-job sound is due gapDur/2 after the session ends; wait well past it.
	time.Sleep(5 * b.gapDur)
	if plays := speaker.plays.Load(); plays > 1 {
		t.Errorf("speaker played %d sounds after cancel, want only the warning that was already starting", plays)
	}
}

func TestCancelWarningStopsAGoodJobThatIsAlreadyDue(t *testing.T) {
	b, speaker := newSessionTestMonitor(t)
	b.gapDur = 400 * time.Millisecond
	b.onBarkDetected(-20, time.Time{}, []string{"oreo"})
	waitForCompletedSession(t, b)

	if _, err := b.DoCommand(context.Background(), map[string]interface{}{"command": "cancel_warning"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * b.gapDur)
	if plays := speaker.plays.Load(); plays > 1 {
		t.Errorf("speaker played %d sounds, want the good-job sound cancelled", plays)
	}
}

func TestNextSessionWarnsAgainAfterACancel(t *testing.T) {
	b, speaker := newSessionTestMonitor(t)
	b.onBarkDetected(-20, time.Time{}, []string{"oreo"})
	if _, err := b.DoCommand(context.Background(), map[string]interface{}{"command": "cancel_warning"}); err != nil {
		t.Fatal(err)
	}
	waitForCompletedSession(t, b)
	b.mu.Lock()
	b.pending = nil
	b.mu.Unlock()
	before := speaker.plays.Load()

	b.onBarkDetected(-20, time.Time{}, []string{"oreo"})
	if session := waitForCompletedSession(t, b); session.WarningsCancelled {
		t.Error("the cancel carried over into the next session")
	}
	deadline := time.Now().Add(5 * time.Second)
	for speaker.plays.Load() < before+2 {
		if time.Now().After(deadline) {
			t.Fatalf("next session played %d sounds, want its warning and good-job sound", speaker.plays.Load()-before)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// A working TTS engine must not replace the recorded good-job sound.
func TestGoodJobIsTheRecordingEvenWhenSpeechIsAvailable(t *testing.T) {
	installFakePiper(t, fakePiper)
	b := &barkMonitor{logger: logging.NewTestLogger(t)}
	if got := b.goodJobSoundFor([]string{"oreo"}); !bytes.Equal(got, goodJobSound) {
		t.Error("good-job sound is not the built-in recording")
	}

	custom := []byte("custom recording")
	b.defaultGoodJobSound = custom
	if got := b.goodJobSoundFor([]string{"oreo"}); !bytes.Equal(got, custom) {
		t.Error("configured good_job_sound_path recording was not used")
	}
}
