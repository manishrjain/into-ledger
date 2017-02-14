package main

import "io"

type state int

const (
	// most common state. Outside of quoted field.
	start state = iota
	// in quoted field
	quoted
	// in quoted field and that previous character was a backslash
	escape
)

type converter struct {
	delegate  io.Reader
	buf       []byte // place we read into
	remaining []byte // what is still left to be read from
	escaped   []byte // if non-empty, contains raw bytes ready to be copied to output, before remaining
	s         state
}

func newConverter(r io.Reader) *converter {
	return &converter{
		delegate: r,
		buf:      make([]byte, 4092),
	}
}

func (c *converter) Read(p []byte) (n int, err error) {
	if len(c.escaped) != 0 {
		n = copy(p, c.escaped)
		c.escaped = c.escaped[n:]
		return n, nil
	}

	if len(c.remaining) == 0 {
		n, err = c.delegate.Read(c.buf)
		if n == 0 {
			return n, err
		}
		c.remaining = c.buf[:n]
	}

	i := 0 // cursor to p
	for i < len(p) && len(c.remaining) != 0 {
		next := c.remaining[0]
		c.remaining = c.remaining[1:]
		switch c.s {
		case start:
			p[i] = next
			i++
			if next == '"' {
				c.s = quoted
			}
		case quoted:
			switch next {
			case '"':
				p[i] = next
				i++
				c.s = start
			case '\\':
				c.s = escape
			default:
				p[i] = next
				i++
			}
		case escape:
			switch next {
			case '"':
				c.escaped = []byte{'"', '"'}
			case 'n':
				c.escaped = []byte{'\n'}
			default:
				c.escaped = []byte{next}
			}
			c.s = quoted
			return i, err
		}
	}

	return i, err
}
