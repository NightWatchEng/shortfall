package statsd

import (
	"bufio"
	"io"
	"net"
)

// writerSink writes newline-delimited metric lines to an io.Writer. Used for
// files (a StatsD sidecar reading a fifo), tests, and the conformance
// harness. Buffered, so Close flushes.
type writerSink struct {
	w    io.Writer
	bw   *bufio.Writer
	once bool
}

func (s *writerSink) writeMetric(line string) error {
	if !s.once {
		s.bw = bufio.NewWriter(s.w)
		s.once = true
	}
	if _, err := s.bw.WriteString(line); err != nil {
		return err
	}
	return s.bw.WriteByte('\n')
}

func (s *writerSink) Close() error {
	if s.bw == nil {
		return nil
	}
	return s.bw.Flush()
}

// packetSink sends one UDP datagram per metric line — StatsD is fire-and-
// forget, and one metric per datagram sidesteps MTU concerns entirely (no
// multi-metric packet can exceed the link MTU). No newline: a single-metric
// datagram needs none.
type packetSink struct {
	conn net.Conn
}

func newPacketSink(address string) (*packetSink, error) {
	conn, err := net.Dial("udp", address)
	if err != nil {
		return nil, err
	}
	return &packetSink{conn: conn}, nil
}

func (s *packetSink) writeMetric(line string) error {
	_, err := s.conn.Write([]byte(line))
	return err
}

func (s *packetSink) Close() error { return s.conn.Close() }

// WithAddress sends metrics as UDP datagrams to a StatsD agent at address
// (host:port). Dials immediately so a bad address fails at New, not on the
// first export. Mutually exclusive with WithWriter; the last one set wins.
func WithAddress(address string) func(*Options) {
	return func(o *Options) {
		s, err := newPacketSink(address)
		if err != nil {
			o.dialErr = err
			return
		}
		o.sink = s
	}
}
