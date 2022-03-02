package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"regexp"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

var (
	tabLoadQuery = make(chan string)
	tabLoadReply = make(chan session)
	tabSave      = make(chan session)
	windowClose  = make(chan string)
	windowQuery  = make(chan session)
	windowReply  = make(chan session)
	tabRegexp    = regexp.MustCompile(`^([[:xdigit:]]{8,})_([[:xdigit:]]{8})$`)
)

type session struct {
	ctx     context.Context
	cancel  context.CancelFunc
	id      string
	last    time.Time
	timeout time.Duration
}

func loadWindow(id string, timeout time.Duration) session {
	windowQuery <- session{id: id, timeout: timeout}
	return <-windowReply
}

func closeWindow(id string) {
	windowClose <- id
}

func loadTab(id string) session {
	tabLoadQuery <- id
	return <-tabLoadReply
}

func (ses session) saveTab() {
	tabSave <- ses
}

func allocateSessions() {
	GCInterval := time.NewTicker(2 * time.Second)
	rand.Seed(time.Now().UnixNano())

	windows := make(map[string]session)
	tabs := make(map[string]session)

	for {
		select {
		case q := <-windowQuery:
			w, ok := windows[q.id]
			if !ok {
				w = createWindow(q.id)
				w.timeout = 30 * time.Second
			}
			if q.timeout > w.timeout {
				w.timeout = q.timeout
			}
			w.last = time.Now()
			windowReply <- w
			windows[w.id] = w

		case id := <-windowClose:
			if w, ok := windows[id]; ok {
				w.shutdown()
				delete(windows, id)
			}

		case t := <-tabSave:
			tabs[t.id] = t

		case id := <-tabLoadQuery:
			tabLoadReply <- tabs[id]
			delete(tabs, id)

			prefix, _, err := parseTabID(id)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Tab ID parse error: %s\n", err)
				break
			}
			if w, ok := windows[prefix]; ok {
				w.last = time.Now()
				windows[prefix] = w
			} else {
				fmt.Fprintf(os.Stderr, "Tab ID (%s) didn't match any window\n", id)
			}

		case <-GCInterval.C:
			for _, w := range windows {
				if elapsed := time.Since(w.last); elapsed > w.timeout {
					fmt.Fprintf(os.Stderr,
						"Window (session %s) was last requested %.1f seconds ago, closing it\n",
						w.id, elapsed.Seconds())
					w.shutdown()
					msg := removeWindow(w.id, &windows, &tabs)
					fmt.Fprintln(os.Stderr, msg)
				}
			}
		}
	}
}

func parseTabID(id string) (prefix, suffix string, err error) {
	m := tabRegexp.FindStringSubmatch(id)
	if len(m) < 3 {
		err = fmt.Errorf(`illegal tab ID format "%s"`, id)
		return
	}
	prefix, suffix = m[1], m[2]
	return
}

func removeWindow(id string, windows, tabs *map[string]session) string {
	delete(*windows, id)
	var tabLog []string
	for tid := range *tabs {
		prefix, suffix, _ := parseTabID(tid)
		if prefix == id {
			tabLog = append(tabLog, fmt.Sprintf("_%s", suffix))
			delete(*tabs, tid)
		}
	}
	if len(tabLog) == 0 {
		return fmt.Sprintf("Deleting window %s", id)
	}
	return fmt.Sprintf("Deleting window %s including tabs %v", id, tabLog)
}

func createWindow(id string) session {
	ctx, cancel := chromedp.NewExecAllocator(
		context.Background(),
	)
	var w session
	w.cancel = cancel
	if len(id) < 8 {
		w.id = createSessionID()
	} else {
		w.id = id
	}

	// create a persistent dummy tab to keep the window open
	w.ctx, _ = chromedp.NewContext(ctx)
	chromedp.Run(w.ctx, chromedp.Navigate("about:blank"))

	return w
}

func createSessionID() string {
	return fmt.Sprintf("%08x", rand.Int63()&0xffffffff)
}

func (ses session) createSiblingTabWithTimeout(timeout time.Duration) session {
	if timeout > ses.timeout {
		ses = loadWindow(ses.id, timeout)
	}
	id := fmt.Sprintf("%s_%s", ses.id, createSessionID())
	sibling := session{id: id, timeout: timeout}
	sibling.ctx, _ = chromedp.NewContext(ses.ctx)
	sibling.ctx, _ = context.WithTimeout(sibling.ctx, timeout)
	sibling.cancel = context.CancelFunc(func() {
		chromedp.Run(sibling.ctx, page.Close())
	})
	return sibling
}

func (ses *session) shutdown() {
	if ses.cancel == nil {
		msg := "Expected non-nil cancelFunc when shutting down tab/window (session %s)\n"
		fmt.Fprintf(os.Stderr, msg, ses.id)
		return
	}
	ses.cancel()
}

func click(sel string) chromedp.ActionFunc {
	return func(ctx context.Context) error {
		return chromedp.Click(sel, chromedp.NodeVisible).Do(ctx)
	}
}

func elementExists(sel string, res *bool) chromedp.ActionFunc {
	return func(ctx context.Context) error {
		var nodes []*cdp.Node
		err := chromedp.Run(ctx, chromedp.Nodes(sel, &nodes, chromedp.AtLeast(0)))
		*res = len(nodes) > 0
		return err
	}
}

func defaultWhile(res *bool) chromedp.ActionFunc {
	return func(ctx context.Context) error {
		*res = true
		return nil
	}
}

func enableLifecycleEvents() chromedp.ActionFunc {
	return func(ctx context.Context) error {
		err := page.Enable().Do(ctx)
		if err != nil {
			return err
		}
		return page.SetLifecycleEventsEnabled(true).Do(ctx)
	}
}

func navigate(url string) chromedp.ActionFunc {
	return func(ctx context.Context) error {
		_, _, _, err := page.Navigate(url).Do(ctx)
		return err
	}
}

func outerHTML(out *[]string) chromedp.ActionFunc {
	return func(ctx context.Context) error {
		var ids []cdp.NodeID
		chromedp.NodeIDs("document", &ids, chromedp.ByJSPath).Do(ctx)
		if len(ids) == 0 {
			return fmt.Errorf("couldn't locate \"document\" node")
		}
		html, err := dom.GetOuterHTML().WithNodeID(ids[0]).Do(ctx)
		*out = append(*out, html)
		return err
	}
}

func scrollToBottom() chromedp.ActionFunc {
	return func(ctx context.Context) error {
		cmd := `document.body.scrollTo(0,document.body.scrollHeight);`
		return chromedp.Evaluate(cmd, nil).Do(ctx)
	}
}

func listen(id *string, events ...string) chromedp.ActionFunc {
	return func(ctx context.Context) error {
		mustEvents := make(map[string]bool)
		for _, event := range events {
			mustEvents[event] = true
		}

		ch := make(chan struct{})
		cctx, cancel := context.WithCancel(ctx)
		chromedp.ListenTarget(cctx, func(ev interface{}) {
			switch e := ev.(type) {
			case *page.EventLifecycleEvent:
				if ok := mustEvents[e.Name]; ok {
					fmt.Fprintf(os.Stderr, "%s Tab event (session %s): Caught %s\n",
						time.Now().Format("[15:04:05]"), *id, e.Name)
					delete(mustEvents, e.Name)
					if len(mustEvents) == 0 {
						cancel()
						close(ch)
					}
				} else {
					fmt.Fprintf(os.Stderr, "%s Tab event (session %s): Ignored %s\n",
						time.Now().Format("[15:04:05]"), *id, e.Name)
				}
			}
		})
		select {
		case <-ch:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
