// Package buffer provides an io.Writer as a 1:N on-disk buffer,
// publishing flushed files to a channel for processing.
//
// Files may be flushed via interval, write count, or byte size.
//
// All exported methods are thread-safe.
package buffer

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// PID for unique filename.
var pid = os.Getpid()

// Ids for unique filename.
var ids = int64(0)

// Reason for flush.
type Reason string

// Flush reasons.
const (
	Forced   Reason = "forced"
	Writes   Reason = "writes"
	Bytes    Reason = "bytes"
	Interval Reason = "interval"
)

// Flush represents a flushed file.
type Flush struct {
	Reason Reason        `json:"reason"`
	Path   string        `json:"path"`
	Writes int64         `json:"writes"`
	Bytes  int64         `json:"bytes"`
	Opened time.Time     `json:"opened"`
	Closed time.Time     `json:"closed"`
	Age    time.Duration `json:"age"`
}

// Config for disk buffer.
type Config struct {
	FlushWrites   int64         // Flush after N writes, zero to disable
	FlushBytes    int64         // Flush after N bytes, zero to disable
	FlushInterval time.Duration // Flush after duration, zero to disable
	BufferSize    int           // Buffer size for writes
	Queue         chan *Flush   // Queue of flushed files
	Verbosity     int           // Verbosity level, 0-3
	Logger        *log.Logger   // Logger instance
}

// Validate the configuration.
func (c *Config) Validate() error {
	switch {
	case c.FlushBytes == 0 && c.FlushWrites == 0 && c.FlushInterval == 0:
		return fmt.Errorf("at least one flush mechanism must be non-zero")
	default:
		return nil
	}
}

// Buffer represents a 1:N on-disk buffer.
type Buffer struct {
	*Config

	verbosity int
	path      string
	ids       int64
	id        int64

	sync.RWMutex
	buf    *bufio.Writer
	opened time.Time
	writes int64
	bytes  int64
	file   *os.File
	tick   *time.Ticker
}

// New buffer at `path`. The path given is used for the base
// of the filenames created, which append ".{pid}.{id}.{fid}".
func New(path string, config *Config) (*Buffer, error) {
	id := atomic.AddInt64(&ids, 1)

	b := &Buffer{
		Config:    config,
		path:      path,
		id:        id,
		verbosity: 1,
	}

	if b.Logger == nil {
		prefix := fmt.Sprintf("buffer #%d %q ", b.id, path)
		b.Logger = log.New(os.Stderr, prefix, log.LstdFlags)
	}

	if b.Queue == nil {
		b.Queue = make(chan *Flush)
	}

	if b.FlushInterval != 0 {
		b.tick = time.NewTicker(config.FlushInterval)
		go b.loop()
	}

	err := config.Validate()
	if err != nil {
		return nil, err
	}

	return b, b.open()
}

// Write implements io.Writer.
func (b *Buffer) Write(data []byte) (int, error) {
	b.log(3, "write %s", data)

	b.Lock()
	defer b.Unlock()

	n, err := b.write(data)
	if err != nil {
		return n, err
	}

	if b.FlushWrites != 0 && b.writes >= b.FlushWrites {
		err := b.flush(Writes)
		if err != nil {
			return n, err
		}
	}

	if b.FlushBytes != 0 && b.bytes >= b.FlushBytes {
		err := b.flush(Bytes)
		if err != nil {
			return n, err
		}
	}

	return n, err
}

// Close the underlying file after flushing.
func (b *Buffer) Close() error {
	b.Lock()
	defer b.Unlock()

	if b.tick != nil {
		b.tick.Stop()
	}

	return b.flush(Forced)
}

// Flush forces a flush.
func (b *Buffer) Flush() error {
	b.Lock()
	defer b.Unlock()
	return b.flush(Forced)
}

// Writes returns the number of writes made to the current file.
func (b *Buffer) Writes() int64 {
	b.RLock()
	defer b.RUnlock()
	return b.writes
}

// Bytes returns the number of bytes made to the current file.
func (b *Buffer) Bytes() int64 {
	b.RLock()
	defer b.RUnlock()
	return b.bytes
}

// Loop for flush interval.
func (b *Buffer) loop() {
	for range b.tick.C {
		b.Lock()
		b.flush(Interval)
		b.Unlock()
	}
}

// Open a new buffer.
func (b *Buffer) open() error {
	path := b.pathname()

	b.log(1, "opening %s", path)
	f, err := os.Create(path)
	if err != nil {
		return err
	}

	b.log(2, "buffer size %d", b.BufferSize)
	if b.BufferSize != 0 {
		b.buf = bufio.NewWriterSize(f, b.BufferSize)
	}

	b.log(2, "reset state")
	b.opened = time.Now()
	b.writes = 0
	b.bytes = 0
	b.file = f

	return nil
}

// Write with metrics.
func (b *Buffer) write(data []byte) (int, error) {
	b.writes++
	b.bytes += int64(len(data))

	if b.BufferSize != 0 {
		return b.buf.Write(data)
	}

	return b.file.Write(data)
}

// Flush for the given reason and re-open.
func (b *Buffer) flush(reason Reason) error {
	b.log(1, "flushing (%s)", reason)

	if b.writes == 0 {
		b.log(2, "nothing to flush")
		return nil
	}

	err := b.close()
	if err != nil {
		return err
	}

	b.Queue <- &Flush{
		Reason: reason,
		Writes: b.writes,
		Bytes:  b.bytes,
		Opened: b.opened,
		Closed: time.Now(),
		Path:   b.file.Name() + ".closed",
		Age:    time.Since(b.opened),
	}

	return b.open()
}

// Close existing file after a rename.
func (b *Buffer) close() error {
	if b.file == nil {
		return nil
	}

	path := b.file.Name()

	b.log(2, "renaming %q", path)
	err := os.Rename(path, path+".closed")
	if err != nil {
		return err
	}

	if b.BufferSize != 0 {
		b.log(2, "flushing %q", path)
		err = b.buf.Flush()
		if err != nil {
			return err
		}
	}

	b.log(2, "closing %q", path)
	return b.file.Close()
}

// Pathname for a new buffer.
func (b *Buffer) pathname() string {
	fid := atomic.AddInt64(&b.ids, 1)
	return fmt.Sprintf("%s.%d.%d.%d", b.path, pid, b.id, fid)
}

// Log helper.
func (b *Buffer) log(n int, msg string, args ...interface{}) {
	if b.Verbosity >= n {
		b.Logger.Printf(msg, args...)
	}
}
