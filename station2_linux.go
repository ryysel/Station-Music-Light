package main

// Station 2 has no /sys/class/leds ring: its 89-LED ring is an 89x1 XRGB
// framebuffer. maind --platformd owns that framebuffer through its
// led-animator thread and keeps repainting the stock idle pattern, so plain
// framebuffer writes lose the race. This backend pauses only that thread with
// ptrace while AirPlay owns the ring, paints whole frames, and detaches to
// hand the ring back. If the supervisor dies the kernel detaches the tracee,
// so the station's own indication resumes without a reboot.
//
// Station 2 is special: every sysfs model keeps using YAM's /led API.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const (
	station2FBPath  = "/dev/fb0"
	station2FBSize  = "/sys/class/graphics/fb0/virtual_size"
	station2FBDepth = "/sys/class/graphics/fb0/bits_per_pixel"
	station2LEDs    = 89
)

type framebufferLED struct {
	mu            sync.Mutex
	file          *os.File
	bytesPerPixel int
	lastFrame     []byte
	pid, tid      int
	// lease pins every ptrace call to one OS thread. Linux makes the attaching
	// thread the tracer, so attaching in the spectrum goroutine and detaching
	// in the control loop would silently leave led-animator stopped.
	lease *traceLease
	// attach is a test hook. Production always uses attachAnimator.
	attach func() error
}

type traceLease struct {
	requests chan func() error
	results  chan error
	done     chan struct{}
}

func newTraceLease() *traceLease {
	l := &traceLease{
		requests: make(chan func() error),
		results:  make(chan error),
		done:     make(chan struct{}),
	}
	go l.loop()
	return l
}

func (l *traceLease) loop() {
	defer close(l.done)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	for fn := range l.requests {
		l.results <- fn()
	}
}

func (l *traceLease) run(fn func() error) error {
	select {
	case l.requests <- fn:
		return <-l.results
	case <-l.done:
		return errors.New("ptrace lease is closed")
	}
}

func (l *traceLease) close() {
	close(l.requests)
	<-l.done
}

var errFramebufferBusy = errors.New("station ring is owned by the firmware")

func openStation2LED() (*framebufferLED, error) {
	return openFramebufferLED(station2FBPath, station2FBSize, station2FBDepth)
}

// openFramebufferLED verifies the exact Station 2 geometry before exposing the
// device. Tests substitute temporary paths and disable the ptrace lease.
func openFramebufferLED(path, sizePath, depthPath string) (*framebufferLED, error) {
	size, err := os.ReadFile(sizePath)
	if err != nil {
		return nil, err
	}
	if geometry := strings.TrimSpace(string(size)); geometry != "89,1" {
		return nil, fmt.Errorf("unexpected Station 2 framebuffer geometry %q", geometry)
	}
	depth, err := os.ReadFile(depthPath)
	if err != nil {
		return nil, err
	}
	bytesPerPixel := 0
	switch strings.TrimSpace(string(depth)) {
	case "24":
		bytesPerPixel = 3
	case "32":
		bytesPerPixel = 4
	default:
		return nil, fmt.Errorf("unsupported Station 2 framebuffer depth %q", strings.TrimSpace(string(depth)))
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	led := &framebufferLED{file: f, bytesPerPixel: bytesPerPixel}
	led.attach = led.attachAnimator
	return led, nil
}

// ensureLocked takes the ring before the first write. It is a no-op when the
// test hook is disabled. A missing or foreign led-animator is reported as busy
// instead of racing the vendor indication.
func (f *framebufferLED) ensureLocked() error {
	if f.attach == nil {
		return nil
	}
	if f.tid != 0 && threadMatches(f.pid, f.tid) {
		return nil
	}
	if f.tid != 0 {
		_ = f.detachAnimator()
	}
	if err := f.attach(); err != nil {
		return errFramebufferBusy
	}
	return nil
}

// attachAnimator freezes only the platformd LED thread. SIGSTOP would stop the
// whole process, including buttons and cloud handling; ptrace is per thread.
func (f *framebufferLED) attachAnimator() error {
	_ = f.detachAnimator()
	if f.lease == nil {
		f.lease = newTraceLease()
	}
	return f.lease.run(func() error {
		pid, tid, err := findLEDAnimator()
		if err != nil {
			return err
		}
		if err = syscall.PtraceAttach(tid); err != nil {
			return err
		}
		if err = waitTraceStop(tid); err != nil {
			_ = syscall.PtraceDetach(tid)
			return err
		}
		f.pid, f.tid = pid, tid
		f.lastFrame = nil
		return nil
	})
}

// detachAnimator resumes the station's own indication. It never paints black:
// the firmware repaints its idle frame as soon as it is released.
func (f *framebufferLED) detachAnimator() error {
	if f.tid == 0 {
		return nil
	}
	if f.lease == nil {
		f.pid, f.tid = 0, 0
		f.lastFrame = nil
		return nil
	}
	tid := f.tid
	return f.lease.run(func() error {
		err := syscall.PtraceDetach(tid)
		if err != nil && err != syscall.ESRCH {
			return err
		}
		f.pid, f.tid = 0, 0
		f.lastFrame = nil
		return nil
	})
}

func waitTraceStop(tid int) error {
	for {
		var status syscall.WaitStatus
		_, err := syscall.Wait4(tid, &status, 0, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if status.Stopped() {
			return nil
		}
		if status.Exited() || status.Signaled() {
			return errors.New("led-animator exited while attaching")
		}
	}
}

// findLEDAnimator is intentionally strict: only a maind --platformd process
// with a thread named led-animator qualifies. A wrong PID would freeze an
// unrelated service.
func findLEDAnimator() (int, int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, 0, err
	}
	self := os.Getpid()
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self {
			continue
		}
		comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		if err != nil || strings.TrimSpace(string(comm)) != "maind" {
			continue
		}
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil || !bytes.Contains(cmdline, []byte("--platformd")) {
			continue
		}
		tasks, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
		if err != nil {
			continue
		}
		for _, task := range tasks {
			tid, err := strconv.Atoi(task.Name())
			if err != nil {
				continue
			}
			name, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/comm", pid, tid))
			if err == nil && strings.TrimSpace(string(name)) == "led-animator" {
				return pid, tid, nil
			}
		}
	}
	return 0, 0, errors.New("led-animator not found")
}

func threadMatches(pid, tid int) bool {
	if pid <= 0 || tid <= 0 {
		return false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || !bytes.Contains(cmdline, []byte("--platformd")) {
		return false
	}
	name, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/comm", pid, tid))
	return err == nil && strings.TrimSpace(string(name)) == "led-animator"
}

func station2FrameFromColors(colors []string) ([]byte, error) {
	if len(colors) != station2LEDs {
		return nil, fmt.Errorf("Station 2 needs %d LED colours, got %d", station2LEDs, len(colors))
	}
	frame := make([]byte, 0, station2LEDs*3)
	for _, color := range colors {
		r, g, b, err := parseStation2Color(color)
		if err != nil {
			return nil, err
		}
		frame = append(frame, r, g, b)
	}
	return frame, nil
}

func parseStation2Color(value string) (byte, byte, byte, error) {
	if len(value) != 7 || value[0] != '#' {
		return 0, 0, 0, fmt.Errorf("invalid LED colour %q", value)
	}
	n, err := strconv.ParseUint(value[1:], 16, 24)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid LED colour %q", value)
	}
	return byte(n >> 16), byte(n >> 8), byte(n), nil
}

func (f *framebufferLED) encodeRGB(frame []byte) ([]byte, error) {
	if len(frame) != station2LEDs*3 {
		return nil, fmt.Errorf("unexpected Station 2 LED frame length %d", len(frame))
	}
	pixels := make([]byte, 0, station2LEDs*f.bytesPerPixel)
	for i := 0; i < len(frame); i += 3 {
		r, g, b := frame[i], frame[i+1], frame[i+2]
		if f.bytesPerPixel == 4 {
			// The LED panel is 84 RGBW dies plus the logo row: the FPGA takes
			// the four pixel bytes as R, G, B, W, not as little-endian XRGB.
			// Writing 0xff into the fourth byte lit the white die at full
			// power and washed every colour out; the stock animator keeps it 0.
			pixels = append(pixels, r, g, b, 0x00)
		} else {
			pixels = append(pixels, r, g, b)
		}
	}
	return pixels, nil
}

func (f *framebufferLED) writeLocked(frame []byte) error {
	if f.file == nil {
		return os.ErrClosed
	}
	pixels, err := f.encodeRGB(frame)
	if err != nil {
		return err
	}
	if bytes.Equal(pixels, f.lastFrame) {
		return nil
	}
	n, err := f.file.WriteAt(pixels, 0)
	if err != nil {
		return err
	}
	if n != len(pixels) {
		return io.ErrShortWrite
	}
	f.lastFrame = pixels
	return nil
}

func (f *framebufferLED) Colors(_ context.Context, colors []string) error {
	frame, err := station2FrameFromColors(colors)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.ensureLocked(); err != nil {
		return err
	}
	return f.writeLocked(frame)
}

func (f *framebufferLED) Release(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.detachAnimator()
}

func (f *framebufferLED) Off(ctx context.Context) error { return f.Release(ctx) }

func (f *framebufferLED) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.detachAnimator()
	if f.lease != nil {
		f.lease.close()
		f.lease = nil
	}
	if f.file == nil {
		return err
	}
	closeErr := f.file.Close()
	f.file = nil
	if err == nil {
		err = closeErr
	}
	return err
}
