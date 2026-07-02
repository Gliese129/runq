package cli

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// multiSelect renders an interactive checkbox list on the terminal and
// returns the indices the user confirmed. All entries start selected.
//
// Keys: ↑/k ↓/j move · space toggle · a all · n none · enter confirm ·
// q/esc/ctrl-c cancel.
//
// Returns ok=false when the user cancels or when the terminal can't do raw
// mode (caller should fall back to a plain yes/no confirmation).
func multiSelect(title string, lines []string) (selected []int, ok bool) {
	if len(lines) == 0 {
		return nil, false
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, false
	}
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return nil, false
	}
	defer term.Restore(fd, oldState)

	sel := make([]bool, len(lines))
	for i := range sel {
		sel[i] = true
	}
	cur := 0

	// Windowed rendering: show at most maxRows entries, scrolled around the
	// cursor, so long lists don't overflow the terminal (cursor-up repaints
	// break once output scrolls past the top of the screen).
	const maxRows = 15
	win := maxRows
	if len(lines) < win {
		win = len(lines)
	}
	top := 0

	// Total painted rows: title + entries + footer.
	painted := win + 2

	render := func(first bool) {
		var b strings.Builder
		if !first {
			fmt.Fprintf(&b, "\x1b[%dA", painted) // cursor up to repaint block
		}
		// Keep cursor inside the window.
		if cur < top {
			top = cur
		}
		if cur >= top+win {
			top = cur - win + 1
		}
		nSel := 0
		for _, s := range sel {
			if s {
				nSel++
			}
		}
		fmt.Fprintf(&b, "\r\x1b[2K%s (%d/%d selected)\r\n", title, nSel, len(lines))
		for i := top; i < top+win; i++ {
			b.WriteString("\x1b[2K")
			cursor := "  "
			if i == cur {
				cursor = "> "
			}
			mark := "[ ]"
			if sel[i] {
				mark = "[x]"
			}
			line := cursor + mark + " " + lines[i]
			if i == cur {
				line = "\x1b[7m" + line + "\x1b[0m" // reverse video on cursor row
			}
			b.WriteString(line + "\r\n")
		}
		more := ""
		if len(lines) > win {
			more = fmt.Sprintf(" (%d more above/below, scroll with ↑↓)", len(lines)-win)
		}
		fmt.Fprintf(&b, "\x1b[2K  space toggle · a all · n none · enter confirm · q cancel%s\r\n", more)
		os.Stdout.WriteString(b.String())
	}

	render(true)
	buf := make([]byte, 8)
	for {
		n, rerr := os.Stdin.Read(buf)
		if rerr != nil || n == 0 {
			return nil, false
		}
		switch {
		case n == 1 && (buf[0] == 'q' || buf[0] == 3 || buf[0] == 27): // q / ctrl-c / bare esc
			return nil, false
		case n == 1 && (buf[0] == '\r' || buf[0] == '\n'):
			for i, s := range sel {
				if s {
					selected = append(selected, i)
				}
			}
			return selected, true
		case n == 1 && buf[0] == ' ':
			sel[cur] = !sel[cur]
		case n == 1 && buf[0] == 'a':
			for i := range sel {
				sel[i] = true
			}
		case n == 1 && buf[0] == 'n':
			for i := range sel {
				sel[i] = false
			}
		case (n == 1 && buf[0] == 'k') || (n >= 3 && buf[0] == 27 && buf[1] == '[' && buf[2] == 'A'): // up
			if cur > 0 {
				cur--
			}
		case (n == 1 && buf[0] == 'j') || (n >= 3 && buf[0] == 27 && buf[1] == '[' && buf[2] == 'B'): // down
			if cur < len(lines)-1 {
				cur++
			}
		}
		render(false)
	}
}
