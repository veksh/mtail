// Copyright 2020 Google Inc. All Rights Reserved.
// This file is available under the Apache license.

package logstream

import (
	"bytes"
	"context"
	"expvar"
	"io"
	"os"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/google/mtail/internal/logline"
	"github.com/google/mtail/internal/waker"
)

var (
	// fileRotations counts the rotations of a file stream
	fileRotations = expvar.NewMap("file_rotations_total")
	// fileTruncates counts the truncations of a file stream
	fileTruncates = expvar.NewMap("file_truncates_total")
)

// fileStream streams log lines from a regular file on the file system.  These
// log files are appended to by another process, and are either rotated or
// truncated by that (or yet another) process.  Rotation implies that a new
// inode with the same name has been created, the old file descriptor will be
// valid until EOF at which point it's considered completed.  A truncation means
// the same file descriptor is used but the file offset will be reset to 0.
// The latter is potentially lossy as far as mtail is concerned, if the last
// logs are not read before truncation occurs.  When an EOF is read, the
// goroutine tests for both truncation and inode change and resets or spins off
// a new goroutine and closes itself down.  The shared context is used for
// cancellation.
type fileStream struct {
	ctx   context.Context
	lines chan<- *logline.LogLine

	pathname string // Given name for the underlying file on the filesystem

	mu           sync.RWMutex // protects following fields.
	lastReadTime time.Time    // Last time a log line was read from this file
	completed    bool         // The filestream is completed and can no longer be used.

	stopOnce sync.Once     // Ensure stopChan only closed once.
	stopChan chan struct{} // Close to start graceful shutdown.
}

// newFileStream creates a new log stream from a regular file.
func newFileStream(ctx context.Context, wg *sync.WaitGroup, waker waker.Waker, pathname string, fi os.FileInfo, lines chan<- *logline.LogLine, streamFromStart bool) (LogStream, error) {
	fs := &fileStream{ctx: ctx, pathname: pathname, lastReadTime: time.Now(), lines: lines, stopChan: make(chan struct{})}
	if err := fs.stream(ctx, wg, waker, fi, streamFromStart); err != nil {
		return nil, err
	}
	return fs, nil
}

func (fs *fileStream) LastReadTime() time.Time {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.lastReadTime
}

func (fs *fileStream) stream(ctx context.Context, wg *sync.WaitGroup, waker waker.Waker, fi os.FileInfo, streamFromStart bool) error {
	fd, err := os.OpenFile(fs.pathname, os.O_RDONLY, 0600)
	if err != nil {
		logErrors.Add(fs.pathname, 1)
		return err
	}
	glog.V(2).Infof("%v: opened new file", fd)
	if !streamFromStart {
		if _, err := fd.Seek(0, io.SeekEnd); err != nil {
			logErrors.Add(fs.pathname, 1)
			if err := fd.Close(); err != nil {
				logErrors.Add(fs.pathname, 1)
				glog.Info(err)
			}
			return err
		}
		glog.V(2).Infof("%v: seeked to end", fd)
	}
	b := make([]byte, defaultReadBufferSize)
	partial := bytes.NewBufferString("")
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			glog.V(2).Infof("%v: closing file descriptor", fd)
			if err := fd.Close(); err != nil {
				logErrors.Add(fs.pathname, 1)
				glog.Info(err)
			}
		}()
		for {
			// Blocking read but regular files will return EOF straight away.
			count, err := fd.Read(b)
			glog.V(2).Infof("%v: read %d bytes, err is %v", fd, count, err)

			if err != nil && err != io.EOF {
				glog.Info(err)
				logErrors.Add(fs.pathname, 1)
			}

			if count > 0 {
				glog.V(2).Infof("%v: decode and send", fd)
				decodeAndSend(ctx, fs.lines, fs.pathname, count, b[:count], partial)
				fs.mu.Lock()
				fs.lastReadTime = time.Now()
				fs.mu.Unlock()
			}

			// If we have read no bytes and are at EOF, check for truncation and rotation.
			if err == io.EOF && count == 0 {
				glog.V(2).Infof("%v: eof an no bytes", fd)
				// Both rotation and truncation need to stat, so check for rotation first.  It is assumed that rotation is the more common change pattern anyway
				newfi, serr := os.Stat(fs.pathname)
				if serr != nil {
					glog.Info(serr)
					// If this is a NotExist error, the file has been either
					// deleted or renamed.  In the first case, we can expect
					// that we're going to be told to Stop by the Tailer.  In
					// the latter we're in the middle of a log rotation between
					// rename and create, which we'll detect with the Stat
					// above in the next pass.  In both cases, go to sleep to
					// find out what to do next.
					if !os.IsNotExist(serr) {
						logErrors.Add(fs.pathname, 1)
					}
					goto Sleep
				}
				// TODO existing logstream finished race bug on delete
				if !os.SameFile(fi, newfi) {
					glog.V(2).Infof("%v: adding a new file routine", fd)
					fileRotations.Add(fs.pathname, 1)
					if err := fs.stream(ctx, wg, waker, newfi, true); err != nil {
						glog.Info(err)
					}
					// We're at EOF so there's nothing left to read here.
					return
				}
				currentOffset, serr := fd.Seek(0, io.SeekCurrent)
				if serr != nil {
					logErrors.Add(fs.pathname, 1)
					glog.Info(serr)
					continue
				}
				glog.V(2).Infof("%v: current seek is %d", fd, currentOffset)
				// We know that newfi is the same file here.
				if currentOffset != 0 && newfi.Size() < currentOffset {
					glog.V(2).Infof("%v: truncate? currentoffset is %d and size is %d", fd, currentOffset, newfi.Size())
					// About to lose all remaining data because of the truncate so flush the accumulator.
					if partial.Len() > 0 {
						sendLine(ctx, fs.pathname, partial, fs.lines)
					}
					p, serr := fd.Seek(0, io.SeekStart)
					if serr != nil {
						logErrors.Add(fs.pathname, 1)
						glog.Info(serr)
					}
					glog.V(2).Infof("%v: Seeked to %d", fd, p)
					fileTruncates.Add(fs.pathname, 1)
					continue
				}
			}

			// No error implies there is more to read in this file, unless it
			// looks like we're cancelled.
			if err == nil && ctx.Err() == nil {
				continue
			}

		Sleep:
			// If we have stalled or it looks like we're cancelled, then test to see if it's time to exit.
			if err == io.EOF || ctx.Err() != nil {
				select {
				case <-fs.stopChan:
					glog.V(2).Infof("%v: stream has been stopped, exiting", fd)
					if partial.Len() > 0 {
						sendLine(ctx, fs.pathname, partial, fs.lines)
					}
					fs.mu.Lock()
					fs.completed = true
					fs.mu.Unlock()
					return
				case <-ctx.Done():
					glog.V(2).Infof("%v: stream has been cancelled, exiting", fd)
					if partial.Len() > 0 {
						sendLine(ctx, fs.pathname, partial, fs.lines)
					}
					fs.mu.Lock()
					fs.completed = true
					fs.mu.Unlock()
					return
				default:
					// keep going
				}
			}

			// Time to yield and wait for a termination signal or wakeup.
			glog.V(2).Infof("%v: waiting", fd)
			select {
			case <-fs.stopChan:
				// We may have started waiting here when the stop signal
				// arrives, but since that wait the file may have been
				// written to.  The file is not technically yet at EOF so
				// we need to go back and try one more read.  We'll exit
				// the stream in the select stanza above.
				glog.V(2).Infof("%v: Stopping after next read", fd)
			case <-ctx.Done():
				// Same for cancellation; this makes tests stable, but
				// could argue exiting immediately is less surprising.
				// Assumption is that this doesn't make a difference in
				// production.
				glog.V(2).Infof("%v: Cancelled after next read", fd)
			case <-waker.Wake():
				// sleep until next Wake()
				glog.V(2).Infof("%v: Wake received", fd)
			}
		}
	}()

	return nil
}

func (fs *fileStream) IsComplete() bool {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.completed
}

func (fs *fileStream) Stop() {
	fs.stopOnce.Do(func() {
		glog.Info("stopping at next EOF")
		close(fs.stopChan)
	})
}
