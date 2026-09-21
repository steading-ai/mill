package server_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/server"
)

var _ = Describe("The observer", Label("unit"), func() {
	It("wakes a waiter signalled on its own key, not another's", func(ctx SpecContext) {
		o := server.NewObserver(time.Hour)
		mine := o.Subscribe("run-a")
		other := o.Subscribe("run-b")

		o.Signal("run-a")

		Eventually(mine).Should(BeClosed())
		Consistently(other, 20*time.Millisecond).ShouldNot(BeClosed())
	})

	It("a signal that lands after Subscribe but before Wait is not missed", func(ctx SpecContext) {
		o := server.NewObserver(time.Hour)
		ch := o.Subscribe("run-a")
		o.Signal("run-a")

		waited := make(chan struct{})
		go func() {
			o.Wait(ctx, ch)
			close(waited)
		}()
		Eventually(waited, time.Second).Should(BeClosed())
	})

	It("an unsignalled waiter unblocks once the fallback elapses", func(ctx SpecContext) {
		o := server.NewObserver(30 * time.Millisecond)
		ch := o.Subscribe("run-a")

		start := time.Now()
		waited := make(chan struct{})
		go func() {
			o.Wait(ctx, ch)
			close(waited)
		}()
		Eventually(waited, time.Second).Should(BeClosed())
		Expect(time.Since(start)).To(BeNumerically(">=", 30*time.Millisecond))
	})

	It("a signalled waiter returns well before the fallback elapses", func(ctx SpecContext) {
		o := server.NewObserver(2 * time.Second)
		ch := o.Subscribe("run-a")

		start := time.Now()
		waited := make(chan struct{})
		go func() {
			o.Wait(ctx, ch)
			close(waited)
		}()
		time.Sleep(10 * time.Millisecond)
		o.Signal("run-a")
		Eventually(waited, time.Second).Should(BeClosed())
		Expect(time.Since(start)).To(BeNumerically("<", 2*time.Second))
	})

	It("Wait returns when ctx is done even with no signal and a long fallback", func() {
		o := server.NewObserver(time.Hour)
		ch := o.Subscribe("run-a")
		waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		waited := make(chan struct{})
		go func() {
			o.Wait(waitCtx, ch)
			close(waited)
		}()
		Eventually(waited, time.Second).Should(BeClosed())
	})
})
