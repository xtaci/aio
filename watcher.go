//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly
// +build linux darwin netbsd freebsd openbsd dragonfly

// Package gaio is an Async-IO library for Golang.
//
// gaio acts in proactor mode, https://en.wikipedia.org/wiki/Proactor_pattern.
// User submit async IO operations and waits for IO-completion signal.
package gaio

import (
	"container/heap"
	"io"
	"net"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	aiocbPool sync.Pool
)

func init() {
	aiocbPool.New = func() interface{} {
		return new(aiocb)
	}
}

// fdDesc contains all data structures associated to fd
type fdDesc struct {
	readers cbList // all read/write requests
	writers cbList
	ptr     uintptr // pointer to net.Conn
	r_armed bool
	w_armed bool
}

// watcher will monitor events and process async-io request(s),
type watcher struct {
	// poll fd
	pfd *poller

	// netpoll events
	chEventNotify chan pollerEvents

	// events from user
	chPendingNotify   chan struct{}
	pendingCreate     []*aiocb
	pendingProcessing []*aiocb // swaped pending
	pendingMutex      sync.Mutex
	recycles          []*aiocb

	// IO-completion events to user
	chResults chan *aiocb

	// internal buffer for reading
	swapSize         int // swap buffer capacity, triple buffer
	swapBufferFront  []byte
	swapBufferMiddle []byte
	swapBufferBack   []byte
	bufferOffset     int   // bufferOffset for current using one
	shouldSwap       int32 // atomic mark for swap

	// loop cpu affinity
	chCPUID chan int32

	// loop related data structure
	descs      map[int]*fdDesc // all descriptors
	connIdents map[uintptr]int // we must not hold net.Conn as key, for GC purpose
	// for timeout operations which
	// aiocb has non-zero deadline, either exists
	// in timeouts & queue at any time
	// or in neither of them.
	timeouts timedHeap
	timer    *time.Timer
	// for garbage collector
	gc       []net.Conn
	gcMutex  sync.Mutex
	gcNotify chan struct{}

	die     chan struct{}
	dieOnce sync.Once
}

// NewWatcher creates a management object for monitoring file descriptors
// with default internal buffer size - 64KB
func NewWatcher() (*Watcher, error) {
	return NewWatcherSize(defaultInternalBufferSize)
}

// NewWatcherSize creates a management object for monitoring file descriptors.
// 'bufsize' sets the internal swap buffer size for Read() with nil, 2 slices with'bufsize'
// will be allocated for performance.
func NewWatcherSize(bufsize int) (*Watcher, error) {
	w := new(watcher)
	pfd, err := openPoll()
	if err != nil {
		return nil, err
	}
	w.pfd = pfd

	// loop related chan
	w.chCPUID = make(chan int32)
	w.chEventNotify = make(chan pollerEvents)
	w.chPendingNotify = make(chan struct{}, 1)
	w.chResults = make(chan *aiocb, maxEvents*4)
	w.die = make(chan struct{})

	// swapBuffer for shared reading
	w.swapSize = bufsize
	w.swapBufferFront = make([]byte, bufsize)
	w.swapBufferMiddle = make([]byte, bufsize)
	w.swapBufferBack = make([]byte, bufsize)

	// init loop related data structures
	w.descs = make(map[int]*fdDesc)
	w.connIdents = make(map[uintptr]int)
	w.gcNotify = make(chan struct{}, 1)
	w.timer = time.NewTimer(0)

	go w.pfd.Wait(w.chEventNotify)
	go w.loop()

	// watcher finalizer for system resources
	wrapper := &Watcher{watcher: w}
	runtime.SetFinalizer(wrapper, func(wrapper *Watcher) {
		wrapper.Close()
	})

	return wrapper, nil
}

// Set Poller Affinity for Epoll/Kqueue
func (w *watcher) SetPollerAffinity(cpuid int) (err error) {
	if cpuid >= runtime.NumCPU() {
		return ErrCPUID
	}

	// store and wakeup
	atomic.StoreInt32(&w.pfd.cpuid, int32(cpuid))
	w.pfd.wakeup()
	return nil
}

// Set Loop Affinity for syscall.Read/syscall.Write
func (w *watcher) SetLoopAffinity(cpuid int) (err error) {
	if cpuid >= runtime.NumCPU() {
		return ErrCPUID
	}

	// sendchan
	select {
	case w.chCPUID <- int32(cpuid):
	case <-w.die:
		return ErrConnClosed
	}
	return nil
}

// Close stops monitoring on events for all connections
func (w *watcher) Close() (err error) {
	w.dieOnce.Do(func() {
		close(w.die)
		err = w.pfd.Close()
	})
	return err
}

// notify new operations pending
func (w *watcher) notifyPending() {
	select {
	case w.chPendingNotify <- struct{}{}:
	default:
	}
}

// WaitIO blocks until any read/write completion, or error.
// An internal 'buf' returned or 'r []OpResult' are safe to use BEFORE next call to WaitIO().
func (w *watcher) WaitIO() (r []OpResult, err error) {
	// recycle previous aiocb
	for k := range w.recycles {
		aiocbPool.Put(w.recycles[k])
		// avoid memory leak
		w.recycles[k] = nil
	}
	w.recycles = w.recycles[:0]

	for {
		select {
		case pcb := <-w.chResults:
			r = append(r, OpResult{Operation: pcb.op, Conn: pcb.conn, IsSwapBuffer: pcb.useSwap, Buffer: pcb.buffer, Size: pcb.size, Error: pcb.err, Context: pcb.ctx})
			// avoid memory leak
			pcb.ctx = nil
			w.recycles = append(w.recycles, pcb)
			for len(w.chResults) > 0 {
				pcb := <-w.chResults
				r = append(r, OpResult{Operation: pcb.op, Conn: pcb.conn, IsSwapBuffer: pcb.useSwap, Buffer: pcb.buffer, Size: pcb.size, Error: pcb.err, Context: pcb.ctx})
				// avoid memory leak
				pcb.ctx = nil
				w.recycles = append(w.recycles, pcb)
			}

			atomic.CompareAndSwapInt32(&w.shouldSwap, 0, 1)

			return r, nil
		case <-w.die:
			return nil, ErrWatcherClosed
		}
	}
}

// Read submits an async read request on 'fd' with context 'ctx', using buffer 'buf'.
// 'buf' can be set to nil to use internal buffer.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func (w *watcher) Read(ctx interface{}, conn net.Conn, buf []byte) error {
	return w.aioCreate(ctx, OpRead, conn, buf, zeroTime, false)
}

// ReadTimeout submits an async read request on 'fd' with context 'ctx', using buffer 'buf', and
// expects to read some bytes into the buffer before 'deadline'.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func (w *watcher) ReadTimeout(ctx interface{}, conn net.Conn, buf []byte, deadline time.Time) error {
	return w.aioCreate(ctx, OpRead, conn, buf, deadline, false)
}

// ReadFull submits an async read request on 'fd' with context 'ctx', using buffer 'buf', and
// expects to fill the buffer before 'deadline'.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
// 'buf' can't be nil in ReadFull.
func (w *watcher) ReadFull(ctx interface{}, conn net.Conn, buf []byte, deadline time.Time) error {
	if len(buf) == 0 {
		return ErrEmptyBuffer
	}
	return w.aioCreate(ctx, OpRead, conn, buf, deadline, true)
}

// Write submits an async write request on 'fd' with context 'ctx', using buffer 'buf'.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func (w *watcher) Write(ctx interface{}, conn net.Conn, buf []byte) error {
	if len(buf) == 0 {
		return ErrEmptyBuffer
	}
	return w.aioCreate(ctx, OpWrite, conn, buf, zeroTime, false)
}

// WriteTimeout submits an async write request on 'fd' with context 'ctx', using buffer 'buf', and
// expects to complete writing the buffer before 'deadline', 'buf' can be set to nil to use internal buffer.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func (w *watcher) WriteTimeout(ctx interface{}, conn net.Conn, buf []byte, deadline time.Time) error {
	if len(buf) == 0 {
		return ErrEmptyBuffer
	}
	return w.aioCreate(ctx, OpWrite, conn, buf, deadline, false)
}

// Free let the watcher to release resources related to this conn immediately,
// like socket file descriptors.
func (w *watcher) Free(conn net.Conn) error {
	return w.aioCreate(nil, opDelete, conn, nil, zeroTime, false)
}

// core async-io creation
func (w *watcher) aioCreate(ctx interface{}, op OpType, conn net.Conn, buf []byte, deadline time.Time, readfull bool) error {
	select {
	case <-w.die:
		return ErrWatcherClosed
	default:
		var ptr uintptr
		if conn != nil && reflect.TypeOf(conn).Kind() == reflect.Ptr {
			ptr = reflect.ValueOf(conn).Pointer()
		} else {
			return ErrUnsupported
		}

		cb := aiocbPool.Get().(*aiocb)
		*cb = aiocb{op: op, ptr: ptr, size: 0, ctx: ctx, conn: conn, buffer: buf, deadline: deadline, readFull: readfull, idx: -1}

		w.pendingMutex.Lock()
		w.pendingCreate = append(w.pendingCreate, cb)
		w.pendingMutex.Unlock()

		w.notifyPending()
		return nil
	}
}

// tryRead will try to read data on aiocb and notify
func (w *watcher) tryRead(fd int, pcb *aiocb) bool {
	// step 1. bind to proper buffer
	buf := pcb.buffer

	useSwap := false
	backBuffer := false

	if buf == nil { // internal or backBuffer
		if atomic.CompareAndSwapInt32(&w.shouldSwap, 1, 0) {
			w.swapBufferFront, w.swapBufferMiddle, w.swapBufferBack = w.swapBufferMiddle, w.swapBufferBack, w.swapBufferFront
			w.bufferOffset = 0
		}

		buf = w.swapBufferFront[w.bufferOffset:]
		if len(buf) > 0 {
			useSwap = true
		} else {
			backBuffer = true
			buf = pcb.backBuffer[:]
		}
	}

	// step 2. read into buffer
	for {
		nr, er := rawRead(fd, buf[pcb.size:])
		if er == syscall.EAGAIN {
			return false
		}

		// On MacOS we can see EINTR here if the user
		// pressed ^Z.
		if er == syscall.EINTR {
			continue
		}

		// if er is nil, accumulate bytes read
		if er == nil {
			pcb.size += nr
		}

		pcb.err = er
		// proper setting of EOF
		if nr == 0 && er == nil {
			pcb.err = io.EOF
		}

		break
	}

	// step 3.check read full operation
	// 	the buffer of readfull operation is guaranteed from caller
	if pcb.readFull { // read full operation
		if pcb.err != nil {
			return true
		}
		if pcb.size == len(pcb.buffer) {
			return true
		}
		return false
	}

	// step 4. non read-full operations
	if useSwap { // IO completed with internal buffer
		pcb.useSwap = true
		pcb.buffer = buf[:pcb.size] // set len to pcb.size
		w.bufferOffset += pcb.size
	} else if backBuffer { // internal buffer exhausted
		pcb.buffer = buf
	}
	return true
}

func (w *watcher) tryWrite(fd int, pcb *aiocb) bool {
	var nw int
	var ew error

	if pcb.buffer != nil {
		for {
			nw, ew = rawWrite(fd, pcb.buffer[pcb.size:])
			pcb.err = ew
			if ew == syscall.EAGAIN {
				return false
			}

			if ew == syscall.EINTR {
				continue
			}

			// if ew is nil, accumulate bytes written
			if ew == nil {
				pcb.size += nw
			}
			break
		}
	}

	// all bytes written or has error
	// nil buffer still returns
	if pcb.size == len(pcb.buffer) || ew != nil {
		return true
	}
	return false
}

// release connection related resources
func (w *watcher) releaseConn(ident int) {
	if desc, ok := w.descs[ident]; ok {
		// delete from heap
		for _, tcb := range desc.readers {
			// notify caller
			tcb.err = io.ErrClosedPipe
			w.deliver(tcb)
		}

		for _, tcb := range desc.writers {
			tcb.err = io.ErrClosedPipe
			w.deliver(tcb)
		}

		delete(w.descs, ident)
		delete(w.connIdents, desc.ptr)
		// close socket file descriptor duplicated from net.Conn
		syscall.Close(ident)
	}
}

// deliver function will try best to aggregate results for batch delivery
func (w *watcher) deliver(pcb *aiocb) {
	if pcb.idx != -1 {
		heap.Remove(&w.timeouts, pcb.idx)
	}

	select {
	case w.chResults <- pcb:
	case <-w.die:
	}
}

// the core event loop of this watcher
func (w *watcher) loop() {
	// defer function to release all resources
	defer func() {
		for ident := range w.descs {
			w.releaseConn(ident)
		}
	}()

	for {
		select {
		case <-w.chPendingNotify:
			// swap w.pending with w.pending2
			w.pendingMutex.Lock()
			w.pendingCreate, w.pendingProcessing = w.pendingProcessing, w.pendingCreate
			for i := 0; i < len(w.pendingCreate); i++ {
				w.pendingCreate[i] = nil
			}
			w.pendingCreate = w.pendingCreate[:0]
			w.pendingMutex.Unlock()
			w.handlePending(w.pendingProcessing)

		case pe := <-w.chEventNotify: // poller events
			w.handleEvents(pe)

		case <-w.timer.C: // timeout heap
			for w.timeouts.Len() > 0 {
				now := time.Now()
				pcb := w.timeouts[0]
				if now.After(pcb.deadline) {
					// ErrDeadline
					pcb.err = ErrDeadline
					// remove from list
					pcb.l.Remove(pcb)
					w.deliver(pcb)
				} else {
					w.timer.Reset(pcb.deadline.Sub(now))
					break
				}
			}

		case <-w.gcNotify: // gc recycled net.Conn
			w.gcMutex.Lock()
			for i, c := range w.gc {
				ptr := reflect.ValueOf(c).Pointer()
				if ident, ok := w.connIdents[ptr]; ok {
					// since it's gc-ed, queue is impossible to hold net.Conn
					// we don't have to send to chIOCompletion,just release here
					w.releaseConn(ident)
				}
				w.gc[i] = nil
			}
			w.gc = w.gc[:0]
			w.gcMutex.Unlock()

		case cpuid := <-w.chCPUID:
			setAffinity(cpuid)

		case <-w.die:
			return
		}
	}
}

// for loop handling pending requests
func (w *watcher) handlePending(pending []*aiocb) {
	for _, pcb := range pending {
		ident, ok := w.connIdents[pcb.ptr]
		// resource releasing operation
		if pcb.op == opDelete && ok {
			w.releaseConn(ident)
			continue
		}

		// handling new connection
		var desc *fdDesc
		if ok {
			desc = w.descs[ident]
		} else {
			if dupfd, err := dupconn(pcb.conn); err != nil {
				// unexpected situation, should notify caller if we cannot dup(2)
				pcb.err = err
				w.deliver(pcb)
				continue
			} else {
				// as we duplicated successfully, we're safe to
				// close the original connection
				pcb.conn.Close()
				// assign idents
				ident = dupfd

				werr := w.pfd.Watch(ident)
				if werr != nil {
					pcb.err = werr
					w.deliver(pcb)
					continue
				}

				// file description bindings
				desc = &fdDesc{ptr: pcb.ptr}
				w.descs[ident] = desc
				w.connIdents[pcb.ptr] = ident

				// the conn is still useful for GC finalizer.
				// note finalizer function cannot hold reference to net.Conn,
				// if not it will never be GC-ed.
				runtime.SetFinalizer(pcb.conn, func(c net.Conn) {
					w.gcMutex.Lock()
					w.gc = append(w.gc, c)
					w.gcMutex.Unlock()

					// notify gc processor
					select {
					case w.gcNotify <- struct{}{}:
					default:
					}
				})
			}
		}

		// operations splitted into different buckets
		switch pcb.op {
		case OpRead:
			// try immediately queue is empty
			if len(desc.readers) == 0 {
				if w.tryRead(ident, pcb) {
					w.deliver(pcb)
					continue
				}
			}
			// enqueue for poller events
			pcb.l = &desc.readers
			pcb.l.PushBack(pcb)

			if !desc.r_armed {
				desc.r_armed = true
			}
		case OpWrite:
			if len(desc.writers) == 0 {
				if w.tryWrite(ident, pcb) {
					w.deliver(pcb)
					continue
				}
			}

			pcb.l = &desc.writers
			pcb.l.PushBack(pcb)

			if !desc.w_armed {
				desc.w_armed = true
			}
		}

		// try rearm descriptor
		w.pfd.Rearm(ident, desc.r_armed, desc.w_armed)

		// push to heap for timeout operation
		if !pcb.deadline.IsZero() {
			heap.Push(&w.timeouts, pcb)
			if w.timeouts.Len() == 1 {
				w.timer.Reset(time.Until(pcb.deadline))
			}
		}
	}
}

// handle poller events
func (w *watcher) handleEvents(pe pollerEvents) {
	// suppose fd(s) being polled is closed by conn.Close() from outside after chanrecv,
	// and a new conn has re-opened with the same handler number(fd). The read and write
	// on this fd is fatal.
	//
	// Note poller will remove closed fd automatically epoll(7), kqueue(2) and silently.
	// To solve this problem watcher will dup() a new fd from net.Conn, which uniquely
	// identified by 'e.ident', all library operation will be based on 'e.ident',
	// then IO operation is impossible to misread or miswrite on re-created fd.
	//log.Println(e)
	for _, e := range pe {
		if desc, ok := w.descs[e.ident]; ok {
			if e.ev&EV_READ != 0 {
				desc.r_armed = false
				count := 0
				for _, pcb := range desc.readers {
					if w.tryRead(e.ident, pcb) {
						w.deliver(pcb)
						count++
					} else {
						break
					}
				}
				desc.readers.RemoveHeadN(count)

				if len(desc.readers) > 0 {
					desc.r_armed = true
				}
			}

			if e.ev&EV_WRITE != 0 {
				desc.w_armed = false
				count := 0
				for _, pcb := range desc.writers {
					if w.tryWrite(e.ident, pcb) {
						w.deliver(pcb)
						count++
					} else {
						break
					}
				}
				desc.writers.RemoveHeadN(count)

				if len(desc.writers) > 0 {
					desc.w_armed = true
				}
			}

			if desc.r_armed || desc.w_armed {
				w.pfd.Rearm(e.ident, desc.r_armed, desc.w_armed)
			}
		}
	}
}
