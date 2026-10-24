//go:build linux

package storage

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type fakeDevice struct {
	closed       int
	errorDefault uint16
}

func (d *fakeDevice) Close() error   { d.closed++; return nil }
func (d *fakeDevice) fdPath() string { return "/proc/self/fd/11" }
func (d *fakeDevice) identity() deviceIdentity {
	return deviceIdentity{major: 252, minor: 0, uuid: "01020304-0506-0708-090a-0b0c0d0e0f10", errorDefault: d.errorDefault}
}

type fakeDirectory struct {
	path                 string
	info                 directoryInfo
	isEmpty              bool
	closed, secured      int
	inspectErr, emptyErr error
	secureErr            error
	onInspect            func()
}

func (d *fakeDirectory) Close() error   { d.closed++; return nil }
func (d *fakeDirectory) fdPath() string { return d.path }
func (d *fakeDirectory) inspect() (directoryInfo, error) {
	if d.onInspect != nil {
		d.onInspect()
	}
	return d.info, d.inspectErr
}
func (d *fakeDirectory) empty() (bool, error) { return d.isEmpty, d.emptyErr }
func (d *fakeDirectory) secureAndSync() error {
	d.secured++
	if d.secureErr != nil {
		return d.secureErr
	}
	d.info.mode = unix.S_IFDIR | 0700
	return nil
}

type mountCall struct {
	source, target, filesystem, data string
	flags                            uintptr
}

type fakeSystem struct {
	process, user              int
	clock                      time.Time
	device                     fakeDevice
	before, after              fakeDirectory
	mounted                    bool
	deviceCalls, readCalls     int
	deviceErrors               []error
	waits                      []time.Duration
	opens                      []bool
	mountCalls                 []mountCall
	entries                    []mountInfo
	mountErr, readErr, openErr error
	onRead                     func(int)
	onWait                     func()
	onMount                    func()
}

func newFakeSystem() *fakeSystem {
	return &fakeSystem{
		process: 1, user: 0, clock: time.Unix(1000, 0),
		before: fakeDirectory{path: "/proc/self/fd/12", isEmpty: true, info: directoryInfo{major: 0, minor: 1, mode: unix.S_IFDIR | 0755, mountID: 1, fsType: unix.TMPFS_MAGIC}},
		after:  fakeDirectory{path: "/proc/self/fd/13", info: directoryInfo{major: 252, minor: 0, mode: unix.S_IFDIR | 0755, mountID: 2, fsType: unix.EXT4_SUPER_MAGIC, fsFlags: unix.ST_NODEV | unix.ST_NOSUID}},
	}
}

func (f *fakeSystem) pid() int       { return f.process }
func (f *fakeSystem) euid() int      { return f.user }
func (f *fakeSystem) now() time.Time { return f.clock }
func (f *fakeSystem) wait(ctx context.Context, d time.Duration) error {
	f.waits = append(f.waits, d)
	f.clock = f.clock.Add(d)
	if f.onWait != nil {
		f.onWait()
	}
	return ctx.Err()
}
func (f *fakeSystem) openDevice(string) (blockDevice, error) {
	f.deviceCalls++
	if len(f.deviceErrors) > 0 {
		err := f.deviceErrors[0]
		f.deviceErrors = f.deviceErrors[1:]
		if err != nil {
			return nil, err
		}
	}
	return &f.device, nil
}
func (f *fakeSystem) readMounts() ([]mountInfo, error) {
	f.readCalls++
	if f.onRead != nil {
		f.onRead(f.readCalls)
	}
	if f.readErr != nil {
		return nil, f.readErr
	}
	if f.entries != nil {
		return append([]mountInfo(nil), f.entries...), nil
	}
	entries := []mountInfo{rootMount()}
	if f.mounted {
		entries = append(entries, stateMount())
	}
	return entries, nil
}
func (f *fakeSystem) openTarget(_ string, create bool) (directory, error) {
	f.opens = append(f.opens, create)
	if f.openErr != nil {
		return nil, f.openErr
	}
	if f.mounted {
		return &f.after, nil
	}
	return &f.before, nil
}
func (f *fakeSystem) mount(source, target, filesystem string, flags uintptr, data string) error {
	f.mountCalls = append(f.mountCalls, mountCall{source: source, target: target, filesystem: filesystem, flags: flags, data: data})
	if f.mountErr != nil {
		return f.mountErr
	}
	f.mounted = true
	if f.onMount != nil {
		f.onMount()
	}
	return nil
}

func TestMountNativeGuardCannotMountFromHostTests(t *testing.T) {
	if os.Getpid() == 1 {
		t.Skip("host guard test is not run as PID 1")
	}
	if status, err := Mount(context.Background(), testOptions()); !errors.Is(err, ErrForbidden) || status.Mounted {
		t.Fatalf("host call = %#v, %v", status, err)
	}
}

func TestConcurrentMountReturnsBusyWithoutTouchingSecondDevice(t *testing.T) {
	first, second := newFakeSystem(), newFakeSystem()
	entered, proceed := make(chan struct{}), make(chan struct{})
	first.onRead = func(n int) {
		if n == 1 {
			close(entered)
			<-proceed
		}
	}
	done := make(chan error, 1)
	go func() {
		_, err := mountVolume(context.Background(), testOptions(), first)
		done <- err
	}()
	<-entered
	_, err := mountVolume(context.Background(), testOptions(), second)
	close(proceed)
	firstErr := <-done
	if !errors.Is(err, ErrBusy) || second.deviceCalls != 0 || len(second.mountCalls) != 0 || firstErr != nil || len(first.mountCalls) != 1 {
		t.Fatalf("concurrent callers: second=%v first=%v", err, firstErr)
	}
}

func TestMountGuardsBeforeAnyIO(t *testing.T) {
	for _, name := range []string{"not PID1", "not root", "container PID1", "nil context", "canceled context", "busy"} {
		t.Run(name, func(t *testing.T) {
			f, o := newFakeSystem(), testOptions()
			ctx := context.Background()
			expected := ErrForbidden
			switch name {
			case "not PID1":
				f.process = 42
			case "not root":
				f.user = 1000
			case "container PID1":
				o.Platform = "container"
			case "nil context":
				ctx = nil
				expected = nil
			case "canceled context":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				expected = context.Canceled
			case "busy":
				mountMu.Lock()
				defer mountMu.Unlock()
				expected = ErrBusy
			}
			status, err := mountVolume(ctx, o, f)
			if err == nil || expected != nil && !errors.Is(err, expected) || status.Mounted || f.deviceCalls != 0 || f.readCalls != 0 || len(f.opens) != 0 || len(f.mountCalls) != 0 {
				t.Fatalf("guard failed: status=%#v err=%v backend=%#v", status, err, f)
			}
		})
	}
}

func TestMountNewVolumeUsesPinnedFDsAndVerifiesReopenedRoot(t *testing.T) {
	f := newFakeSystem()
	status, err := mountVolume(context.Background(), testOptions(), f)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Mounted || status.AlreadyMounted || status.MountID != 2 || status.DeviceMajor != 252 || status.UUID != f.device.identity().uuid {
		t.Fatalf("status: %#v", status)
	}
	want := []mountCall{{source: "/proc/self/fd/11", target: "/proc/self/fd/12", filesystem: "ext4", flags: unix.MS_NOSUID | unix.MS_NODEV | unix.MS_RELATIME, data: "errors=remount-ro,data=ordered,barrier=1"}}
	if !reflect.DeepEqual(f.mountCalls, want) || !reflect.DeepEqual(f.opens, []bool{true, false}) {
		t.Fatalf("mount calls %#v / opens %#v", f.mountCalls, f.opens)
	}
	if f.before.secured != 0 || f.after.secured != 1 || f.before.closed != 1 || f.after.closed != 1 || f.device.closed != 1 || f.after.info.mode&07777 != 0700 {
		t.Fatalf("incorrect directory ownership lifecycle: %#v", f)
	}
}

func TestMountExistingVolumeIsIdempotent(t *testing.T) {
	f := newFakeSystem()
	f.mounted = true
	status, err := mountVolume(context.Background(), testOptions(), f)
	if err != nil || !status.Mounted || !status.AlreadyMounted || status.MountID != 2 || len(f.mountCalls) != 0 || !reflect.DeepEqual(f.opens, []bool{false}) || f.after.secured != 1 {
		t.Fatalf("existing mount: %#v, %v; %#v", status, err, f)
	}
}

func TestMountExistingVolumeResolvesOmittedErrorPolicy(t *testing.T) {
	for _, behavior := range []uint16{1, 2, 3} {
		f := newFakeSystem()
		f.mounted = true
		f.device.errorDefault = behavior
		m := stateMount()
		m.superOptions = []string{"rw"} // Kernel omits values matching on-disk defaults.
		f.entries = []mountInfo{rootMount(), m}
		status, err := mountVolume(context.Background(), testOptions(), f)
		if behavior == 2 {
			if err != nil || !status.AlreadyMounted {
				t.Fatalf("safe default rejected: %#v, %v", status, err)
			}
		} else if !errors.Is(err, ErrConflict) || len(f.opens) != 0 {
			t.Fatalf("unsafe default %d accepted: %#v, %v", behavior, status, err)
		}
		if len(f.mountCalls) != 0 {
			t.Fatal("existing mount policy was silently changed")
		}
	}
}

func TestDeviceAppearanceBoundAndCancellation(t *testing.T) {
	t.Run("appear", func(t *testing.T) {
		f := newFakeSystem()
		f.deviceErrors = []error{unix.ENOENT, unix.ENXIO, nil}
		if _, err := mountVolume(context.Background(), testOptions(), f); err != nil || f.deviceCalls != 3 || len(f.waits) != 2 {
			t.Fatalf("appearance: calls=%d waits=%v err=%v", f.deviceCalls, f.waits, err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		f := newFakeSystem()
		f.deviceErrors = []error{unix.ENOENT, unix.ENODEV, unix.ENOENT, unix.ENOENT}
		if _, err := mountVolume(context.Background(), testOptions(), f); !errors.Is(err, ErrDeviceTimeout) || !reflect.DeepEqual(f.waits, []time.Duration{100 * time.Millisecond, 100 * time.Millisecond, 50 * time.Millisecond}) || len(f.opens) != 0 {
			t.Fatalf("timeout: waits=%v err=%v", f.waits, err)
		}
	})
	t.Run("cancel during wait", func(t *testing.T) {
		f := newFakeSystem()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.deviceErrors = []error{unix.ENOENT}
		f.onWait = cancel
		if _, err := mountVolume(ctx, testOptions(), f); !errors.Is(err, context.Canceled) || f.deviceCalls != 1 || len(f.opens) != 0 {
			t.Fatalf("cancel: %#v err=%v", f, err)
		}
	})
	for _, permanent := range []error{unix.ELOOP, unix.EACCES, ErrFilesystem, ErrConflict} {
		t.Run(permanent.Error(), func(t *testing.T) {
			f := newFakeSystem()
			f.deviceErrors = []error{permanent}
			if _, err := mountVolume(context.Background(), testOptions(), f); !errors.Is(err, permanent) || f.deviceCalls != 1 || len(f.waits) != 0 {
				t.Fatalf("permanent error retried: %#v, %v", f, err)
			}
		})
	}
}

func TestMountRejectsBeforeSyscall(t *testing.T) {
	cases := map[string]func(*fakeSystem){
		"nonempty target":      func(f *fakeSystem) { f.before.isEmpty = false },
		"foreign owner":        func(f *fakeSystem) { f.before.info.uid = 1000 },
		"foreign group":        func(f *fakeSystem) { f.before.info.gid = 1000 },
		"writable by group":    func(f *fakeSystem) { f.before.info.mode |= 0020 },
		"wrong file type":      func(f *fakeSystem) { f.before.info.mode = unix.S_IFREG | 0600 },
		"unexpected fd mount":  func(f *fakeSystem) { f.before.info.mountID = 10 },
		"unexpected fd device": func(f *fakeSystem) { f.before.info.minor = 10 },
		"path symlink":         func(f *fakeSystem) { f.openErr = unix.ELOOP },
		"bad mount table":      func(f *fakeSystem) { f.readErr = errors.New("malformed table") },
		"readdir failure":      func(f *fakeSystem) { f.before.emptyErr = unix.EIO },
		"inspect failure":      func(f *fakeSystem) { f.before.inspectErr = unix.EIO },
		"mount appeared during setup": func(f *fakeSystem) {
			f.onRead = func(n int) {
				if n == 2 {
					f.mounted = true
				}
			}
		},
		"cover replaced during setup": func(f *fakeSystem) {
			f.onRead = func(n int) {
				if n == 2 {
					m := rootMount()
					m.id = 10
					f.entries = []mountInfo{m}
				}
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeSystem()
			change(f)
			status, err := mountVolume(context.Background(), testOptions(), f)
			if err == nil || status.Mounted || len(f.mountCalls) != 0 || f.device.closed != 1 {
				t.Fatalf("unexpected mount after conflict: %#v err=%v backend=%#v", status, err, f)
			}
		})
	}
}

func TestMountPostSuccessFailuresRemainExplicitlyMounted(t *testing.T) {
	cases := map[string]func(*fakeSystem){
		"new mount not present":    func(f *fakeSystem) { f.onMount = func() { f.entries = []mountInfo{rootMount()} } },
		"post mount table failure": func(f *fakeSystem) { f.onMount = func() { f.readErr = unix.EIO } },
		"reopen failure":           func(f *fakeSystem) { f.onMount = func() { f.openErr = unix.EACCES } },
		"foreign root owner":       func(f *fakeSystem) { f.after.info.uid = 1000 },
		"foreign root group":       func(f *fakeSystem) { f.after.info.gid = 1000 },
		"root writable by others":  func(f *fakeSystem) { f.after.info.mode |= 0002 },
		"wrong mount ID":           func(f *fakeSystem) { f.after.info.mountID = 100 },
		"wrong device":             func(f *fakeSystem) { f.after.info.minor = 1 },
		"wrong fs":                 func(f *fakeSystem) { f.after.info.fsType = unix.TMPFS_MAGIC },
		"statfs readonly":          func(f *fakeSystem) { f.after.info.fsFlags |= unix.ST_RDONLY },
		"statfs noexec":            func(f *fakeSystem) { f.after.info.fsFlags |= unix.ST_NOEXEC },
		"statfs suid":              func(f *fakeSystem) { f.after.info.fsFlags &^= unix.ST_NOSUID },
		"statfs devices":           func(f *fakeSystem) { f.after.info.fsFlags &^= unix.ST_NODEV },
		"directory fsync failure":  func(f *fakeSystem) { f.after.secureErr = unix.EIO },
		"mount disappeared at final check": func(f *fakeSystem) {
			f.onRead = func(n int) {
				if n == 4 {
					f.entries = []mountInfo{rootMount()}
				}
			}
		},
		"mount replaced at final check": func(f *fakeSystem) {
			f.onRead = func(n int) {
				if n == 4 {
					m := stateMount()
					m.id = 3
					f.entries = []mountInfo{rootMount(), m}
				}
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeSystem()
			change(f)
			status, err := mountVolume(context.Background(), testOptions(), f)
			if err == nil || !status.Mounted || status.AlreadyMounted || len(f.mountCalls) != 1 || f.device.closed != 1 {
				t.Fatalf("post-mount failure concealed: %#v err=%v backend=%#v", status, err, f)
			}
		})
	}
}

func TestCancellationAfterMountCompletesPrivateRootSetup(t *testing.T) {
	f := newFakeSystem()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.onMount = cancel
	status, err := mountVolume(ctx, testOptions(), f)
	if !errors.Is(err, context.Canceled) || !status.Mounted || status.MountID != 2 || f.after.secured != 1 || f.after.info.mode&07777 != 0700 {
		t.Fatalf("canceled mount skipped postconditions: %#v, %v; %#v", status, err, f)
	}
}

func TestMountSyscallFailureAndExistingMountSyncFailure(t *testing.T) {
	f := newFakeSystem()
	f.mountErr = unix.EINVAL
	status, err := mountVolume(context.Background(), testOptions(), f)
	if !errors.Is(err, unix.EINVAL) || status.Mounted || f.before.closed != 1 || f.after.secured != 0 {
		t.Fatalf("syscall failure: %#v %v", status, err)
	}
	f = newFakeSystem()
	f.mounted = true
	f.after.secureErr = unix.EIO
	status, err = mountVolume(context.Background(), testOptions(), f)
	if !errors.Is(err, unix.EIO) || !status.Mounted || !status.AlreadyMounted || len(f.mountCalls) != 0 {
		t.Fatalf("existing sync failure: %#v %v", status, err)
	}
}
