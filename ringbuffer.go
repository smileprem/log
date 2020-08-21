package log

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// RingbufferWriter is an io.WriteCloser that writes asynchronously.
type RingbufferWriter struct {
	// RingSize is the size of the ring buffer.
	RingSize int

	// PollInterval sets the interval at which the diode is queried for new data.
	PollInterval time.Duration

	// Alert is used to report how many values were overwritten since the last write.
	Alert func(missed int)

	// Writer specifies the writer of output.
	Writer io.Writer

	once   sync.Once
	dioder *diode
	cancel context.CancelFunc
	done   chan struct{}
}

// Close implements io.Closer, and closes the underlying Writer.
func (w *RingbufferWriter) Close() (err error) {
	w.cancel()
	<-w.done
	if closer, ok := w.Writer.(io.Closer); ok {
		err = closer.Close()
	}
	return
}

// Write implements io.Writer.
func (w *RingbufferWriter) Write(p []byte) (n int, err error) {
	w.once.Do(func() {
		if w.RingSize <= 0 {
			panic("invalid ring buffer size")
		}
		if w.PollInterval <= 0 {
			panic("invalid ring buffer poll interval")
		}
		// diode
		ctx, cancel := context.WithCancel(context.Background())
		w.cancel = cancel
		w.done = make(chan struct{})
		w.dioder = newDiode(w.RingSize, w.Alert, w.PollInterval, ctx)
		go func(w *RingbufferWriter) {
			for {
				v := w.dioder.Next()
				if v == nil {
					close(w.done)
					return
				}
				p := *(*[]byte)(v)
				w.Writer.Write(p)
				if cap(p) <= bbcap {
					a1kpool.Put(p[:0])
				}
			}
		}(w)
	})

	// copy and sends data
	p = append(a1kpool.Get().([]byte), p...)
	w.dioder.Set(&p)
	return len(p), nil
}

// diodeDataType is the data type the diodes operate on.
type diodeDataType *[]byte

// diode is optimal for many writers (go-routines B-n) and a single
// reader (go-routine A). It is not thread safe for multiple readers.
type diode struct {
	writeIndex uint64
	readIndex  uint64
	buffer     []unsafe.Pointer
	alert      func(missed int)
	interval   time.Duration
	ctx        context.Context
}

// Newdiode creates a new diode (ring buffer). The diode diode
// is optimzed for many writers (on go-routines B-n) and a single reader
// (on go-routine A). The alert is invoked on the read's go-routine. It is
// called when it notices that the writer go-routine has passed it and wrote
// over data. A nil can be used to ignore alerts.
func newDiode(size int, alert func(missed int), interval time.Duration, ctx context.Context) *diode {
	if alert == nil {
		alert = func(int) {}
	}

	d := &diode{
		buffer: make([]unsafe.Pointer, size),
		alert:  alert,
	}

	// Start write index at the value before 0
	// to allow the first write to use AddUint64
	// and still have a beginning index of 0
	d.writeIndex = ^d.writeIndex

	d.interval = interval
	d.ctx = ctx

	return d
}

type bucket struct {
	data diodeDataType
	seq  uint64 // seq is the recorded write index at the time of writing
}

// Set sets the data in the next slot of the ring buffer.
func (d *diode) Set(data diodeDataType) {
	for {
		writeIndex := atomic.AddUint64(&d.writeIndex, 1)
		idx := writeIndex % uint64(len(d.buffer))
		old := atomic.LoadPointer(&d.buffer[idx])

		if old != nil &&
			(*bucket)(old) != nil &&
			(*bucket)(old).seq > writeIndex-uint64(len(d.buffer)) {
			// println("Diode set collision: consider using a larger diode")
			continue
		}

		newBucket := &bucket{
			data: data,
			seq:  writeIndex,
		}

		if !atomic.CompareAndSwapPointer(&d.buffer[idx], old, unsafe.Pointer(newBucket)) {
			// println("Diode set collision: consider using a larger diode")
			continue
		}

		return
	}
}

// TryNext will attempt to read from the next slot of the ring buffer.
// If there is not data available, it will return (nil, false).
func (d *diode) TryNext() (data diodeDataType, ok bool) {
	// Read a value from the ring buffer based on the readIndex.
	idx := d.readIndex % uint64(len(d.buffer))
	result := (*bucket)(atomic.SwapPointer(&d.buffer[idx], nil))

	// When the result is nil that means the writer has not had the
	// opportunity to write a value into the diode. This value must be ignored
	// and the read head must not increment.
	if result == nil {
		return nil, false
	}

	// When the seq value is less than the current read index that means a
	// value was read from idx that was previously written but has since has
	// been dropped. This value must be ignored and the read head must not
	// increment.
	//
	// The simulation for this scenario assumes the fast forward occurred as
	// detailed below.
	//
	// 5. The reader reads again getting seq 5. It then reads again expecting
	//    seq 6 but gets seq 2. This is a read of a stale value that was
	//    effectively "dropped" so the read fails and the read head stays put.
	//    `| 4 | 5 | 2 | 3 |` r: 7, w: 6
	//
	if result.seq < d.readIndex {
		return nil, false
	}

	// When the seq value is greater than the current read index that means a
	// value was read from idx that overwrote the value that was expected to
	// be at this idx. This happens when the writer has lapped the reader. The
	// reader needs to catch up to the writer so it moves its write head to
	// the new seq, effectively dropping the messages that were not read in
	// between the two values.
	//
	// Here is a simulation of this scenario:
	//
	// 1. Both the read and write heads start at 0.
	//    `| nil | nil | nil | nil |` r: 0, w: 0
	// 2. The writer fills the buffer.
	//    `| 0 | 1 | 2 | 3 |` r: 0, w: 4
	// 3. The writer laps the read head.
	//    `| 4 | 5 | 2 | 3 |` r: 0, w: 6
	// 4. The reader reads the first value, expecting a seq of 0 but reads 4,
	//    this forces the reader to fast forward to 5.
	//    `| 4 | 5 | 2 | 3 |` r: 5, w: 6
	//
	if result.seq > d.readIndex {
		dropped := result.seq - d.readIndex
		d.readIndex = result.seq
		d.alert(int(dropped))
	}

	// Only increment read index if a regular read occurred (where seq was
	// equal to readIndex) or a value was read that caused a fast forward
	// (where seq was greater than readIndex).
	//
	d.readIndex++
	return result.data, true
}

// Next polls the diode until data is available or until the context is done.
// If the context is done, then nil will be returned.
func (d *diode) Next() diodeDataType {
	for {
		data, ok := d.TryNext()
		if !ok {
			if d.isDone() {
				return nil
			}

			time.Sleep(d.interval)
			continue
		}
		return data
	}
}

func (d *diode) isDone() bool {
	select {
	case <-d.ctx.Done():
		return true
	default:
		return false
	}
}
