package progress

import (
	"sync"
	"time"
)

var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// Spinner advances frames on a fixed timer and triggers a render callback.
type Spinner struct {
	mu sync.Mutex

	index    int
	interval time.Duration
	render   func()

	stop chan struct{}
	done chan struct{}

	stopOnce sync.Once
}

func NewSpinner(interval time.Duration, render func()) *Spinner {
	if interval <= 0 {
		interval = 80 * time.Millisecond
	}
	if render == nil {
		render = func() {}
	}

	s := &Spinner{
		interval: interval,
		render:   render,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go s.run()
	return s
}

func (s *Spinner) Frame() rune {
	if len(spinnerFrames) == 0 {
		return ' '
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return spinnerFrames[s.index%len(spinnerFrames)]
}

func (s *Spinner) Stop() {
	s.stopOnce.Do(func() {
		close(s.stop)
		<-s.done
	})
}

func (s *Spinner) run() {
	ticker := time.NewTicker(s.interval)
	defer func() {
		ticker.Stop()
		close(s.done)
	}()

	for {
		select {
		case <-ticker.C:
			s.mu.Lock()
			if len(spinnerFrames) > 0 {
				s.index = (s.index + 1) % len(spinnerFrames)
			}
			s.mu.Unlock()
			s.render()
		case <-s.stop:
			return
		}
	}
}
