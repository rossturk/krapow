package incus

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// SpamEnter attaches to a VM's serial console and writes "\r" once per second
// for `duration`, then closes. Used during the Windows install bake to push
// past OVMF's firmware Boot Manager menu — OVMF lands there after the
// virtio-scsi CD-ROM probe times out (Incus's default CD bus) and the PXE
// entry fails, and the Boot Manager won't proceed without a keypress on the
// console. Pressing Enter on the menu re-selects the highlighted entry, which
// at this point includes the auto-discovered IDE CD-ROMs attached via
// raw.qemu, so the VM boots into Windows Setup.
//
// Mirrors antifob/incus-windows tools/click.py — same idea (PTY + Enter
// pump), reimplemented here so we don't shell out to Python.
//
// Always returns nil; failure to attach is logged via the package's
// StreamErr writer and treated as best-effort (the bake may still succeed
// if the user happens to be watching VGA and presses Enter themselves).
func SpamEnter(name string, duration time.Duration) error {
	master, slave, err := openPTY()
	if err != nil {
		fmt.Fprintf(StreamErr, "SpamEnter %s: openpty: %v\n", name, err)
		return nil
	}
	defer master.Close()

	cmd := exec.Command("incus", "console", "--force", name)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	// New session so Ctrl-C in the parent doesn't tear down the child.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		slave.Close()
		fmt.Fprintf(StreamErr, "SpamEnter %s: start: %v\n", name, err)
		return nil
	}
	// Child has its own copy of the slave fd; parent doesn't need it.
	_ = slave.Close()

	// Continuously drain the master so the kernel's PTY buffer doesn't
	// fill up and block the child writing console output.
	go func() { _, _ = io.Copy(io.Discard, master) }()

	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		if _, err := master.Write([]byte("\r")); err != nil {
			break
		}
		time.Sleep(1 * time.Second)
	}

	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
	return nil
}

// openPTY allocates a new pseudoterminal and returns (master, slave). The
// caller passes `slave` to a child process's stdio and writes/reads via
// `master`. Caller is responsible for closing both files.
func openPTY() (*os.File, *os.File, error) {
	mfd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	// Unlock the slave side so we can open /dev/pts/N.
	if err := unix.IoctlSetPointerInt(mfd, unix.TIOCSPTLCK, 0); err != nil {
		_ = unix.Close(mfd)
		return nil, nil, fmt.Errorf("TIOCSPTLCK: %w", err)
	}
	n, err := unix.IoctlGetInt(mfd, unix.TIOCGPTN)
	if err != nil {
		_ = unix.Close(mfd)
		return nil, nil, fmt.Errorf("TIOCGPTN: %w", err)
	}
	slavePath := fmt.Sprintf("/dev/pts/%d", n)
	sfd, err := unix.Open(slavePath, unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		_ = unix.Close(mfd)
		return nil, nil, fmt.Errorf("open %s: %w", slavePath, err)
	}
	return os.NewFile(uintptr(mfd), "ptmx"),
		os.NewFile(uintptr(sfd), slavePath),
		nil
}
