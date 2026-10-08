package main

/*
#cgo CFLAGS: -fobjc-arc -mmacosx-version-min=13.0 -I/opt/homebrew/include -I/usr/local/include
#cgo LDFLAGS: -framework Foundation -framework ScreenCaptureKit -framework CoreMedia -L/opt/homebrew/lib -L/usr/local/lib -lmp3lame
#include <stdlib.h>
#include <lame/lame.h>
#include "capture.h"
*/
import "C"

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	sampleRate = 48000
	maxQueue   = sampleRate / 2 // keep at most 0.5 s per source (drift protection)
)

// ---------- mixer ----------

type queue struct{ l, r []float32 }

type mixer struct {
	mu   sync.Mutex
	q    [2]queue // 0 = system, 1 = microphone
	gain [2]float32
}

var mix = &mixer{gain: [2]float32{1.0, 1.0}}

//export goOnAudio
func goOnAudio(src C.int, l, r *C.float, n C.int) {
	ls := unsafe.Slice((*float32)(unsafe.Pointer(l)), int(n))
	rs := unsafe.Slice((*float32)(unsafe.Pointer(r)), int(n))
	mix.mu.Lock()
	q := &mix.q[src]
	q.l = append(q.l, ls...)
	q.r = append(q.r, rs...)
	if over := len(q.l) - maxQueue; over > 0 {
		q.l, q.r = q.l[over:], q.r[over:]
	}
	mix.mu.Unlock()
}

// pull returns n mixed frames; missing data is padded with silence.
func (m *mixer) pull(n int) (l, r []float32) {
	l, r = make([]float32, n), make([]float32, n)
	m.mu.Lock()
	defer m.mu.Unlock()
	for s := range m.q {
		q := &m.q[s]
		k := min(n, len(q.l))
		g := m.gain[s]
		for i := 0; i < k; i++ {
			l[i] += q.l[i] * g
			r[i] += q.r[i] * g
		}
		q.l, q.r = q.l[k:], q.r[k:]
	}
	for i := range l {
		l[i] = clip(l[i])
		r[i] = clip(r[i])
	}
	return
}

func clip(v float32) float32 {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}

// ---------- mp3 encoder ----------

type encoder struct {
	gfp *C.lame_global_flags
	f   *os.File
	buf []byte
}

func newEncoder(path string, kbps int) (*encoder, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	gfp := C.lame_init()
	C.lame_set_in_samplerate(gfp, sampleRate)
	C.lame_set_num_channels(gfp, 2)
	C.lame_set_brate(gfp, C.int(kbps))
	C.lame_set_quality(gfp, 2)
	if C.lame_init_params(gfp) < 0 {
		f.Close()
		return nil, fmt.Errorf("lame_init_params failed")
	}
	return &encoder{gfp: gfp, f: f, buf: make([]byte, 1<<16)}, nil
}

func (e *encoder) encode(l, r []float32) error {
	if len(l) == 0 {
		return nil
	}
	need := len(l)*5/4 + 7200
	if len(e.buf) < need {
		e.buf = make([]byte, need)
	}
	n := C.lame_encode_buffer_ieee_float(e.gfp,
		(*C.float)(unsafe.Pointer(&l[0])), (*C.float)(unsafe.Pointer(&r[0])),
		C.int(len(l)), (*C.uchar)(unsafe.Pointer(&e.buf[0])), C.int(len(e.buf)))
	if n < 0 {
		return fmt.Errorf("lame encode error %d", n)
	}
	_, err := e.f.Write(e.buf[:n])
	return err
}

func (e *encoder) close() error {
	n := C.lame_encode_flush(e.gfp, (*C.uchar)(unsafe.Pointer(&e.buf[0])), C.int(len(e.buf)))
	if n > 0 {
		e.f.Write(e.buf[:n])
	}
	C.lame_close(e.gfp)
	return e.f.Close()
}

// ---------- main ----------

func main() {
	out := flag.String("o", "call-"+time.Now().Format("2006-01-02_15-04-05")+".mp3", "output file")
	withMic := flag.Bool("mic", true, "also record microphone (macOS 15+)")
	kbps := flag.Int("bitrate", 128, "MP3 bitrate, kbps")
	sysGain := flag.Float64("sys-gain", 1.0, "system audio gain")
	micGain := flag.Float64("mic-gain", 1.0, "microphone gain")
	flag.Parse()
	mix.gain = [2]float32{float32(*sysGain), float32(*micGain)}

	enc, err := newEncoder(*out, *kbps)
	if err != nil {
		log.Fatal(err)
	}

	micFlag := C.int(0)
	if *withMic {
		micFlag = 1
	}
	var cerr *C.char
	if C.sc_start(micFlag, &cerr) != 0 {
		msg := C.GoString(cerr)
		C.free(unsafe.Pointer(cerr))
		enc.close()
		os.Remove(*out)
		log.Fatalf("capture start failed: %s\n(check System Settings → Privacy & Security → Screen & System Audio Recording)", msg)
	}
	fmt.Printf("Recording to %s — press Ctrl+C to stop\n", *out)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	start := time.Now()
	var written int64
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	// lag the output clock by 200 ms so that callbacks have time to deliver data
	const lag = 200 * time.Millisecond
	step := func() {
		target := int64((time.Since(start) - lag).Seconds() * sampleRate)
		if n := target - written; n > 0 {
			l, r := mix.pull(int(n))
			if err := enc.encode(l, r); err != nil {
				log.Println(err)
			}
			written += n
		}
	}

loop:
	for {
		select {
		case <-tick.C:
			step()
			if written%(sampleRate*5) < sampleRate/10 {
				fmt.Printf("\r%s", time.Duration(written*int64(time.Second)/sampleRate).Truncate(time.Second))
			}
		case <-sig:
			break loop
		}
	}

	C.sc_stop()
	time.Sleep(lag)
	step()
	if err := enc.close(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\nSaved %s (%s)\n", *out, time.Duration(written*int64(time.Second)/sampleRate).Truncate(time.Second))
}
