// Copyright 2020 Google Inc. All Rights Reserved.
// This file is available under the Apache license.

package logstream

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/golang/glog"
	"github.com/google/mtail/internal/logline"
	"github.com/google/mtail/internal/waker"
)

type pipeStream struct {
	ctx   context.Context
	lines chan<- *logline.LogLine

	pathname string // Given name for the underlying named pipe on the filesystem

	mu           sync.RWMutex // protects following fields
	completed    bool         // This pipestream is completed and can no longer be used.
	lastReadTime time.Time    // Last time a log line was read from this named pipe

	stopOnce sync.Once     // Ensure stopChan only closed once.
	stopChan chan struct{} // Close to start graceful shutdown.
}

func newPipeStream(ctx context.Context, wg *sync.WaitGroup, waker waker.Waker, pathname string, fi os.FileInfo, lines chan<- *logline.LogLine) (LogStream, error) {
	ps := &pipeStream{ctx: ctx, pathname: pathname, lastReadTime: time.Now(), lines: lines, stopChan: make(chan struct{})}
	if err := ps.stream(ctx, wg, waker, fi); err != nil {
		return nil, err
	}
	return ps, nil
}

func (ps *pipeStream) LastReadTime() time.Time {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.lastReadTime
}

func (ps *pipeStream) stream(ctx context.Context, wg *sync.WaitGroup, waker waker.Waker, fi os.FileInfo) error {
	// Open in nonblocking mode because the write end of the pipe may not have started yet.
	fd, err := os.OpenFile(ps.pathname, os.O_RDONLY|syscall.O_NONBLOCK, 0600)
	if err != nil {
		logErrors.Add(ps.pathname, 1)
		return err
	}
	glog.V(2).Infof("opened new pipe %v", fd)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			err := fd.Close()
			if err != nil {
				logErrors.Add(ps.pathname, 1)
				glog.Info(err)
			}
			ps.mu.Lock()
			ps.completed = true
			ps.mu.Unlock()
		}()
		b := make([]byte, 0, defaultReadBufferSize)
		capB := cap(b)
		partial := bytes.NewBufferString("")
		for {
			// Set idle timeout
			if err := fd.SetReadDeadline(time.Now().Add(defaultReadTimeout)); err != nil {
				logErrors.Add(ps.pathname, 1)
				glog.V(2).Infof("%s: %s", ps.pathname, err)
			}
			n, err := fd.Read(b[:capB])
			if e, ok := err.(*os.PathError); ok && e.Timeout() && n == 0 {
				// Named Pipes EOF when the writer has closed, so we look for a
				// timeout on read to detect a writer stall and thus let us check
				// below for cancellation.
				goto Sleep
			}
			// Per pipe(7): If all file descriptors referring to the write end
			// of a pipe have been closed, then an attempt to read(2) from the
			// pipe will see end-of-file (read(2) will return 0).
			// All other errors also finish the stream and are counted.
			if err != nil {
				if err != io.EOF {
					glog.Info(err)
					logErrors.Add(ps.pathname, 1)
				}
				return
			}

			if n > 0 {
				decodeAndSend(ps.ctx, ps.lines, ps.pathname, n, b[:n], partial)
				// Update the last read time if we were able to read anything.
				ps.lastReadTime = time.Now()
			}
		Sleep:
			select {
			case <-ps.stopChan:
				ps.mu.Lock()
				ps.completed = true
				ps.mu.Unlock()
				return
			case <-ctx.Done():
				ps.mu.Lock()
				ps.completed = true
				ps.mu.Unlock()
				return
			case <-waker.Wake():
				// sleep until next Wake()
			}
		}
	}()
	return nil
}

func (ps *pipeStream) IsComplete() bool {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.completed
}

func (ps *pipeStream) Stop() {
	ps.stopOnce.Do(func() {
		close(ps.stopChan)
	})
}
