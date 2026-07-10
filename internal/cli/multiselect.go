package cli

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// selectOne renders an interactive single-choice list and returns the
// picked index (raw-terminal arrow menu); enter picks
// the cursor row. Returns ok=false on cancel / dumb terminal.
func selectOne(title string, lines []string) (int, bool) {
	if len(lines) == 0 {
		return 0, false
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return 0, false
	}
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return 0, false
	}
	defer term.Restore(fd, oldState)

	cur := 0
	const maxRows = 15
	win := maxRows
	if len(lines) < win {
		win = len(lines)
	}
	top := 0
	painted := win + 2

	render := func(first bool) {
		var b strings.Builder
		if !first {
			fmt.Fprintf(&b, "\x1b[%dA", painted)
		}
		if cur < top {
			top = cur
		}
		if cur >= top+win {
			top = cur - win + 1
		}
		fmt.Fprintf(&b, "\r\x1b[2K%s\r\n", title)
		for i := top; i < top+win; i++ {
			b.WriteString("\x1b[2K")
			cursor := "  "
			if i == cur {
				cursor = "> "
			}
			line := cursor + lines[i]
			if i == cur {
				line = "\x1b[7m" + line + "\x1b[0m"
			}
			b.WriteString(line + "\r\n")
		}
		more := ""
		if len(lines) > win {
			more = fmt.Sprintf(" (%d more, scroll with ↑↓)", len(lines)-win)
		}
		fmt.Fprintf(&b, "\x1b[2K  ↑↓ move · enter select · q cancel%s\r\n", more)
		os.Stdout.WriteString(b.String())
	}

	render(true)
	buf := make([]byte, 8)
	for {
		n, rerr := os.Stdin.Read(buf)
		if rerr != nil || n == 0 {
			return 0, false
		}
		switch {
		case n == 1 && (buf[0] == 'q' || buf[0] == 3 || buf[0] == 27):
			return 0, false
		case n == 1 && (buf[0] == '\r' || buf[0] == '\n'):
			return cur, true
		case (n == 1 && buf[0] == 'k') || (n >= 3 && buf[0] == 27 && buf[1] == '[' && buf[2] == 'A'):
			if cur > 0 {
				cur--
			}
		case (n == 1 && buf[0] == 'j') || (n >= 3 && buf[0] == 27 && buf[1] == '[' && buf[2] == 'B'):
			if cur < len(lines)-1 {
				cur++
			}
		}
		render(false)
	}
}
