package progress

import (
	"context"
	"io"
	"os"
	"sync"

	"github.com/containerd/console"
	"github.com/docker/buildx/util/logutil"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	PrinterModeAuto  = "auto"
	PrinterModeTty   = "tty"
	PrinterModePlain = "plain"
	PrinterModeQuiet = "quiet"
)

type Printer struct {
	status chan *client.SolveStatus

	ready  chan struct{}
	done   chan struct{}
	paused chan struct{}

	err          error
	warnings     []client.VertexWarning
	logMu        sync.Mutex
	logSourceMap map[digest.Digest]interface{}
}

func (p *Printer) Wait() error {
	close(p.status)
	<-p.done
	return p.err
}

func (p *Printer) Pause() error {
	p.paused = make(chan struct{})
	return p.Wait()
}

func (p *Printer) Unpause() {
	close(p.paused)
	<-p.ready
}

func (p *Printer) Write(s *client.SolveStatus) {
	p.status <- s
}

func (p *Printer) Warnings() []client.VertexWarning {
	return p.warnings
}

func (p *Printer) ValidateLogSource(dgst digest.Digest, v interface{}) bool {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	src, ok := p.logSourceMap[dgst]
	if ok {
		if src == v {
			return true
		}
	} else {
		p.logSourceMap[dgst] = v
		return true
	}
	return false
}

func (p *Printer) ClearLogSource(v interface{}) {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	for d := range p.logSourceMap {
		if p.logSourceMap[d] == v {
			delete(p.logSourceMap, d)
		}
	}
}

func NewPrinter(ctx context.Context, w io.Writer, out console.File, mode string, opts ...PrinterOpt) (*Printer, error) {
	opt := &printerOpts{}
	for _, o := range opts {
		o(opt)
	}

	if v := os.Getenv("BUILDKIT_PROGRESS"); v != "" && mode == PrinterModeAuto {
		mode = v
	}

	var c console.Console
	switch mode {
	case PrinterModeQuiet:
		w = io.Discard
	case PrinterModeAuto, PrinterModeTty:
		if cons, err := console.ConsoleFromFile(out); err == nil {
			c = cons
		} else {
			if mode == PrinterModeTty {
				return nil, errors.Wrap(err, "failed to get console")
			}
		}
	}

	pw := &Printer{
		ready: make(chan struct{}),
	}
	go func() {
		for {
			pw.status = make(chan *client.SolveStatus)
			pw.done = make(chan struct{})

			pw.logMu.Lock()
			pw.logSourceMap = map[digest.Digest]interface{}{}
			pw.logMu.Unlock()

			close(pw.ready)

			resumeLogs := logutil.Pause(logrus.StandardLogger())
			// not using shared context to not disrupt display but let is finish reporting errors
			pw.warnings, pw.err = progressui.DisplaySolveStatus(ctx, c, w, pw.status, opt.displayOpts...)
			resumeLogs()
			close(pw.done)

			if opt.onclose != nil {
				opt.onclose()
			}
			if pw.paused == nil {
				break
			}

			pw.ready = make(chan struct{})
			<-pw.paused
			pw.paused = nil
		}
	}()
	<-pw.ready
	return pw, nil
}

type printerOpts struct {
	displayOpts []progressui.DisplaySolveStatusOpt

	onclose func()
}

type PrinterOpt func(b *printerOpts)

func WithPhase(phase string) PrinterOpt {
	return func(opt *printerOpts) {
		opt.displayOpts = append(opt.displayOpts, progressui.WithPhase(phase))
	}
}

func WithDesc(text string, console string) PrinterOpt {
	return func(opt *printerOpts) {
		opt.displayOpts = append(opt.displayOpts, progressui.WithDesc(text, console))
	}
}

func WithOnClose(onclose func()) PrinterOpt {
	return func(opt *printerOpts) {
		opt.onclose = onclose
	}
}
