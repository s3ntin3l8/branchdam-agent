package eject

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestControlCodes pins the CTL_CODE values against the Windows SDK's
// published constants -- a wrong code is silently a different ioctl.
func TestControlCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  uint32
		want uint32
	}{
		{"FSCTL_LOCK_VOLUME", fsctlLockVolume, 0x00090018},
		{"FSCTL_DISMOUNT_VOLUME", fsctlDismountVolume, 0x00090020},
		{"IOCTL_STORAGE_MEDIA_REMOVAL", ioctlStorageMediaRemoval, 0x002D4804},
		{"IOCTL_STORAGE_EJECT_MEDIA", ioctlStorageEjectMedia, 0x002D4808},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %#08x, want %#08x", tc.name, tc.got, tc.want)
		}
	}
}

// fakeVolume records every control code issued and fails the codes named
// in fail (failing the lock only for its first lockFailures attempts when
// that is set).
type fakeVolume struct {
	calls        []uint32
	inputs       map[uint32][]byte
	fail         map[uint32]error
	lockFailures int
}

func (f *fakeVolume) ctl(code uint32, in []byte) error {
	f.calls = append(f.calls, code)
	if f.inputs == nil {
		f.inputs = map[uint32][]byte{}
	}
	f.inputs[code] = append([]byte(nil), in...)
	if code == fsctlLockVolume && f.lockFailures > 0 {
		f.lockFailures--
		return errors.New("access denied")
	}
	return f.fail[code]
}

func noSleep(time.Duration) {}

func TestEjectSequenceOrder(t *testing.T) {
	f := &fakeVolume{}
	if err := runEjectSequence(`E:\`, f.ctl, noSleep); err != nil {
		t.Fatalf("runEjectSequence: %v", err)
	}
	want := []uint32{fsctlLockVolume, fsctlDismountVolume, ioctlStorageMediaRemoval, ioctlStorageEjectMedia}
	if len(f.calls) != len(want) {
		t.Fatalf("calls = %#x, want %#x", f.calls, want)
	}
	for i := range want {
		if f.calls[i] != want[i] {
			t.Errorf("call %d = %#x, want %#x", i, f.calls[i], want[i])
		}
	}
	if in := f.inputs[ioctlStorageMediaRemoval]; len(in) != 1 || in[0] != 0 {
		t.Errorf("media-removal input = %v, want PreventMediaRemoval=FALSE ([0])", in)
	}
}

func TestEjectSequenceRetriesLock(t *testing.T) {
	var slept []time.Duration
	f := &fakeVolume{lockFailures: 3}
	if err := runEjectSequence(`E:\`, f.ctl, func(d time.Duration) { slept = append(slept, d) }); err != nil {
		t.Fatalf("runEjectSequence after 3 transient lock failures: %v", err)
	}
	if len(slept) != 3 {
		t.Errorf("slept %d times, want 3 (once per failed lock)", len(slept))
	}
}

func TestEjectSequenceStopsAtFirstFailure(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name       string
		failCode   uint32
		wantStep   string
		wantCalls  int
		dismounted bool
	}{
		{"dismount fails", fsctlDismountVolume, StepDismount, 2, false},
		{"media removal fails", ioctlStorageMediaRemoval, StepMediaRemoval, 3, true},
		{"eject fails", ioctlStorageEjectMedia, StepEject, 4, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeVolume{fail: map[uint32]error{tc.failCode: boom}}
			err := runEjectSequence(`E:\`, f.ctl, noSleep)
			var se *StepError
			if !errors.As(err, &se) || se.Step != tc.wantStep || !errors.Is(err, boom) {
				t.Fatalf("err = %v, want a %q StepError wrapping boom", err, tc.wantStep)
			}
			if len(f.calls) != tc.wantCalls {
				t.Errorf("issued %d control codes, want %d -- nothing may run after a failed step", len(f.calls), tc.wantCalls)
			}
			if se.Dismounted() != tc.dismounted {
				t.Errorf("Dismounted() = %v, want %v", se.Dismounted(), tc.dismounted)
			}
			if tc.dismounted && !strings.Contains(err.Error(), "safe to remove") {
				t.Errorf("message %q should tell the operator the card is safe to pull", err)
			}
		})
	}
}

// TestEjectSequenceNeverDismountsAnUnlockedVolume: when the lock never
// succeeds (a file stays open), the volume must not be dismounted or
// ejected -- that is exactly the unsafe pull the sequence exists to stop.
func TestEjectSequenceNeverDismountsAnUnlockedVolume(t *testing.T) {
	f := &fakeVolume{lockFailures: lockRetries}
	err := runEjectSequence(`E:\`, f.ctl, noSleep)
	var se *StepError
	if !errors.As(err, &se) || se.Step != StepLock {
		t.Fatalf("err = %v, want a lock StepError", err)
	}
	if len(f.calls) != lockRetries {
		t.Errorf("issued %d calls, want exactly %d lock attempts and nothing else", len(f.calls), lockRetries)
	}
	for _, c := range f.calls {
		if c != fsctlLockVolume {
			t.Fatalf("issued %#x after a failed lock", c)
		}
	}
	if !strings.Contains(err.Error(), "still open") {
		t.Errorf("message %q should say a file is probably still open", err)
	}
}

func TestVolumeDevicePath(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{`D:\`, `\\.\D:`, false},
		{`E:\DCIM\100EOSR5`, `\\.\E:`, false},
		{`F:`, `\\.\F:`, false},
		{`g:/DCIM`, `\\.\G:`, false},
		{`\\.\G:`, `\\.\G:`, false},
		{``, "", true},
		{`\\server\share\card`, "", true},
		{`/Volumes/CARD`, "", true},
		{`1:\`, "", true},
		{`EF:\`, "", true},
	} {
		got, err := volumeDevicePath(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("volumeDevicePath(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("volumeDevicePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
