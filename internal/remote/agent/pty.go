package agent

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// control runs fn with the file descriptor of f without changing the file's
// blocking mode, which calling f.Fd would do.
func control(f *os.File, fn func(fd int) error) error {
	raw, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var inner error
	if err := raw.Control(func(fd uintptr) { inner = fn(int(fd)) }); err != nil {
		return err
	}
	return inner
}

// openPTY allocates a pseudo-terminal pair with the given size.
func openPTY(rows, cols uint16) (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	var number int
	err = control(master, func(fd int) error {
		if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
			return fmt.Errorf("unlock pseudo-terminal: %w", err)
		}
		n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
		if err != nil {
			return fmt.Errorf("pseudo-terminal number: %w", err)
		}
		number = n
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
	})
	if err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("open pseudo-terminal slave: %w", err)
	}
	return master, slave, nil
}

// resizePTY sets the terminal size through the master side.
func resizePTY(master *os.File, rows, cols uint16) error {
	return control(master, func(fd int) error {
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
	})
}
