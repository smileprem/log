package log

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"testing"
	"time"
)

type nopCloseWriter struct {
	io.Writer
}

func (w nopCloseWriter) Close() (err error) {
	return
}

func TestRingbufferWriter(t *testing.T) {
	w := &RingbufferWriter{
		RingSize:     1000,
		PollInterval: 10 * time.Millisecond,
		Writer:       nopCloseWriter{os.Stderr},
	}
	fmt.Fprintf(w, "%s, before ringbuffer writer close\n", timeNow())
	w.Close()
}

func BenchmarkRingbufferWriterPoll(b *testing.B) {
	w := &RingbufferWriter{
		RingSize:     20000,
		PollInterval: 10 * time.Millisecond,
		Writer:       ioutil.Discard,
	}

	b.SetParallelism(1000)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(b *testing.PB) {
		p := []byte(`{"time":"2019-07-10T05:35:54.277Z","level":"info","caller":"pretty.go:42","error":"i am test error","foo":"bar","n":42,"a":[1,2,3],"obj":{"a":[1], "b":{}},"message":"hello json writer"}`)
		for b.Next() {
			w.Write(p)
		}
	})
}
