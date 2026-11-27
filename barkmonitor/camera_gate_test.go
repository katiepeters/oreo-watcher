package barkmonitor

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/services/video"
	"go.viam.com/rdk/testutils/inject"
)

func TestVideoServicesForClip(t *testing.T) {
	svcs := []namedVideoSvc{{name: "cam-video"}}
	for _, tc := range []struct {
		name               string
		startedWithCameras bool
		allowedNow         bool
		wantClips          bool
	}{
		{"away whole session", true, true, true},
		{"home whole session", false, false, false},
		{"switched to home mid-session", true, false, false},
		{"switched to away mid-session", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &barkMonitor{
				videoServices:  svcs,
				camerasAllowed: func() bool { return tc.allowedNow },
				logger:         logging.NewTestLogger(t),
			}
			got := b.videoServicesForClip(tc.startedWithCameras)
			if (len(got) > 0) != tc.wantClips {
				t.Fatalf("videoServicesForClip returned %d services, want clips=%v", len(got), tc.wantClips)
			}
		})
	}
}

// TestVideoServicesOnlyReadThroughGate fails if any function other than the
// constructor and videoServicesForClip reads barkMonitor.videoServices, so a
// new clip path can't bypass the home-mode camera check.
func TestVideoServicesOnlyReadThroughGate(t *testing.T) {
	allowed := map[string]bool{"newBarkMonitor": true, "videoServicesForClip": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || allowed[fn.Name.Name] {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "videoServices" {
					t.Errorf("%s: %s reads .videoServices directly; call b.videoServicesForClip instead so video is never saved outside away mode",
						fset.Position(sel.Pos()), fn.Name.Name)
				}
				return true
			})
		}
	}
}

// TestSessionClipsFollowModeAtStartAndEnd drives a real bark session through
// onBarkDetected and the end timer, so it checks that the mode seen at
// session start actually reaches the clip decision in onSessionEnd.
func TestSessionClipsFollowModeAtStartAndEnd(t *testing.T) {
	for _, tc := range []struct {
		name         string
		allowedStart bool
		allowedEnd   bool
		wantClip     bool
	}{
		{"away whole session", true, true, true},
		{"home whole session", false, false, false},
		{"home at start, away at end", false, true, false},
		{"away at start, home at end", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var allowed atomic.Bool
			allowed.Store(tc.allowedStart)

			getVideoCalls := make(chan struct{}, 1)
			vid := inject.NewVideo("cam-video")
			vid.GetVideoFunc = func(context.Context, time.Time, time.Time, string, string, map[string]interface{}) (chan *video.Chunk, error) {
				getVideoCalls <- struct{}{}
				ch := make(chan *video.Chunk)
				close(ch)
				return ch, nil
			}

			logger, logs := logging.NewObservedTestLogger(t)
			b := &barkMonitor{
				videoServices:       []namedVideoSvc{{name: "cam-video", svc: vid}},
				camerasAllowed:      allowed.Load,
				recordingDir:        t.TempDir(),
				logger:              logger,
				defaultWarnSound:    leaveItSound,
				defaultGoodJobSound: goodJobSound,
				gapDur:              40 * time.Millisecond,
				warnDur:             time.Hour,
				actionDur:           time.Hour,
				clipPad:             time.Millisecond,
				closed:              make(chan struct{}),
			}
			t.Cleanup(func() { _ = b.Close(context.Background()) })

			b.onBarkDetected(-20, time.Time{}, []string{"oreo"})
			allowed.Store(tc.allowedEnd)

			if tc.wantClip {
				select {
				case <-getVideoCalls:
				case <-time.After(5 * time.Second):
					t.Fatal("session ended in away mode but no video clip was fetched")
				}
				return
			}

			deadline := time.After(5 * time.Second)
			for logs.FilterMessageSnippet("not saving video clips").Len() == 0 {
				select {
				case <-getVideoCalls:
					t.Fatal("video clip fetched for a session that wasn't in away mode throughout")
				case <-deadline:
					t.Fatal("session never reached the clip decision")
				case <-time.After(5 * time.Millisecond):
				}
			}
			select {
			case <-getVideoCalls:
				t.Fatal("video clip fetched for a session that wasn't in away mode throughout")
			default:
			}
		})
	}
}
