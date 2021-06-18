package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

const (
	MaxRenderDelay = 10 * time.Second
)

var (
	DefaultPageloadEvents = []string{
		"DOMContentLoaded",
		"firstMeaningfulPaint",
		"load",
		"networkAlmostIdle",
	}
)

type Result struct {
	Err      []string   `json:"err"`
	Out      [][]string `json:"out"`
	TabId    string     `json:"tab_id"`
	WindowId string     `json:"window_id"`
}

type QueryBlock struct {
	Actions    []ExternalAction `json:"actions"`
	Repeat     string           `json:"repeat"`
	cdpActions []chromedp.Action
	pos        int
	repeat     bool
	repeatFunc func() bool
}

type Query struct {
	ForwardUserAgent bool         `json:"forward_user_agent"`
	RenderDelayRaw   string       `json:"global_render_delay"`
	ReuseTab         bool         `json:"reuse_tab"`
	ReuseWindow      bool         `json:"reuse_window"`
	Blocks           []QueryBlock `json:"query"`
	newTab           bool
	pos              int
	renderDelay      time.Duration
	res              Result
	sessionid        string
	userAgent        string
	version          string
}

func (q *Query) execute() error {
	var ctx context.Context
	var ses session

	// set up tab

	if q.newTab {
		ses = loadWindow(q.sessionid)
		q.sessionid = ses.id
		if q.ReuseWindow {
			q.res.WindowId = ses.id
		}
		// TODO: Allow passing tab-lifetime as a parameter
		ctx = ses.createSiblingTabWithTimeout(20 * time.Second)
		if q.ReuseTab {
			// TODO: Implement tab-saving
			// TODO: Save returned tab id in res
			return fmt.Errorf("reuse_tab isn't implemented")
		} else {
			defer closeSiblingTab(ctx)
		}

	} else {
		return fmt.Errorf("tab-resuming is not implementet")
	}

	var err error
	var block QueryBlock
	for q.pos, block = range q.Blocks {
		fmt.Fprintf(os.Stderr, "%s Query %d/%d (session %s)\n",
			time.Now().Format("[15:04:05]"), q.pos+1, len(q.Blocks), q.sessionid)
		err = chromedp.Run(ctx, block.cdpActions...)
	}

	return err
}

func (q *Query) parseRequest(body io.Reader) error {
	err := json.NewDecoder(body).Decode(&q)
	if err != nil {
		return fmt.Errorf("JSON parsing error: %s", err)
	}
	if q.ForwardUserAgent {
		// TODO: Implement user agent forwarding in execute()
		return fmt.Errorf("value \"true\" is not supported for init.forward_user_agent")
	}

	err = q.parseRenderDelay()
	if err != nil {
		return err
	}
	err = q.parseQueryBlocks()
	if err != nil {
		return err
	}
	return nil
}

func (q *Query) parseRenderDelay() error {
	if q.RenderDelayRaw == "" {
		return fmt.Errorf("global_render_delay is empty or missing")
	}
	delay, err := time.ParseDuration(q.RenderDelayRaw)
	if err != nil {
		return fmt.Errorf("invalid global_render_delay: %s", err)
	}
	if delay > MaxRenderDelay {
		delay = MaxRenderDelay
	}
	q.renderDelay = delay
	return nil
}

func (q *Query) parseQueryBlocks() error {

	if len(q.Blocks) == 0 {
		return fmt.Errorf("query[0] must contain at least one action block")
	}
	if len(q.Blocks[0].Actions) < 2 {
		return fmt.Errorf("query[0].actions[0] must contain at least two actions")
	}
	switch q.Blocks[0].Actions[0].Name() {
	case "load_tab":
		q.newTab = false
	case "navigate":
		q.newTab = true
	default:
		return fmt.Errorf("query[0].actions[0] must begin with either \"load_tab\" or \"navigate\"")
	}

	if q.hasListeningEvents() {
		q.appendActions(network.Enable(), enableLifecycleEvents())
	}

	q.res.Err = make([]string, len(q.Blocks))
	q.res.Out = make([][]string, len(q.Blocks))

	var err error
	var block QueryBlock
	for q.pos, block = range q.Blocks {

		// ensure non-nil empty return slices in JSON response
		// q.res.Err[q.pos] = make([]string, 0)
		q.res.Out[q.pos] = make([]string, 0)

		if len(block.Actions) == 0 {
			return fmt.Errorf("query[%d].actions can't be empty", q.pos)
		}
		const efmt = "query[%d].actions[%v]: %s"

		var xa ExternalAction
		for block.pos, xa = range block.Actions {
			err = q.parseAction(xa)
			if err != nil {
				return fmt.Errorf(efmt, q.pos, block.pos, err)
			}
		}

		if len(block.Repeat) == 0 {
			q.Blocks[q.pos].repeat = false
			q.Blocks[q.pos].repeatFunc = nil
		} else {
			q.Blocks[q.pos].repeat = true
			return fmt.Errorf("query[...].repeat is not implementet, must be \"\"")
		}
	}

	return nil
}

func (q *Query) hasListeningEvents() bool {
	for _, block := range q.Blocks {
		for _, xa := range block.Actions {
			if xa.Name() == "listen" {
				return true
			}
		}
	}
	return false
}

func (q *Query) parseAction(xa ExternalAction) error {
	if xa.Name() == "" {
		return fmt.Errorf("[0] must contain the name of an action")
	}
	for i, arg := range xa.Args() {
		if arg == "" {
			return fmt.Errorf("[%d] must contain a non-empty argument", i+1)
		}
	}

	var err error
	switch xa.Name() {
	case "eval":
		if err = xa.MustArgCount(1); err != nil {
			return err
		}
		// TODO: Append the appropriate action
		return fmt.Errorf("eval not implemented")

	case "listen":
		events := xa.Args()
		events, err = parseEvents(events)
		if err != nil {
			return fmt.Errorf("listen: %s", err)
		}
		q.appendActions(listen(&q.sessionid, events...))

	case "load_tab":
		if err = xa.MustArgCount(1); err != nil {
			return err
		}
		// TODO: Append the appropriate action
		return fmt.Errorf("load_tab not implemented")

	case "navigate":
		if err = xa.MustArgCount(1); err != nil {
			return err
		}
		xurl := xa.Arg(1)
		_, err = url.ParseRequestURI(xurl)
		if err != nil {
			return fmt.Errorf("navigate: non-URL argument: %s", err)
		}
		q.appendActions(navigate(xurl))

	case "outer_html":
		if err = xa.MustArgCount(0); err != nil {
			return err
		}
		q.appendActions(outerHTML(&q.res.Out[q.pos]))

	case "sleep":
		if err = xa.MustArgCount(0, 1); err != nil {
			return err
		}
		var delay time.Duration
		if len(xa.Args()) == 0 {
			delay = q.renderDelay
		} else {
			delay, err = time.ParseDuration(xa.Arg(1))
			if err != nil {
				return fmt.Errorf("sleep: invalid duration: %s", err)
			}
		}
		q.appendActions(chromedp.Sleep(delay))

	default:
		return fmt.Errorf("unknown action name \"%s\"", xa.Name())
	}
	return nil
}

func parseEvents(events []string) ([]string, error) {
	if len(events) == 0 {
		return defaultPageloadEvents(), nil
	}
	for i, event := range events {
		if !validEvent(event) {
			return events, fmt.Errorf("arg %d contains unknown event \"%s\"", i, event)
		}
	}
	return events, nil
}

func defaultPageloadEvents() []string {
	events := make([]string, len(DefaultPageloadEvents))
	copy(events, DefaultPageloadEvents)
	return events
}

func validEvent(event string) bool {
	switch event {
	case "DOMContentLoaded":
	case "firstContentfulPaint":
	case "firstImagePaint":
	case "firstMeaningfulPaint":
	case "firstMeaningfulPaintCandidate":
	case "firstPaint":
	case "init":
	case "load":
	case "networkAlmostIdle":
	case "networkIdle":
	default:
		return false
	}
	return true
}

func (q *Query) appendActions(actions ...chromedp.Action) {
	block := &q.Blocks[q.pos]
	block.cdpActions = append(block.cdpActions, actions...)
}

type ExternalAction []string

func (xa ExternalAction) Arg(n int) string {
	if n < 0 || len(xa) <= n {
		return ""
	}
	return xa[n]
}

func (xa ExternalAction) Args() []string {
	if len(xa) == 0 {
		return nil
	}
	return xa[1:]
}

func (xa ExternalAction) Name() string {
	return xa.Arg(0)
}

func (xa ExternalAction) MustArgCount(ns ...int) error {
	switch len(ns) {
	case 0:
		if len(xa) == 0 {
			return fmt.Errorf("%s: not enough arguments", xa.Name())
		}
		return nil
	case 1:
		n := ns[0]
		if len(xa.Args()) < n {
			return fmt.Errorf("%s: not enough arguments", xa.Name())
		}
		if len(xa.Args()) > n {
			return fmt.Errorf("%s: too many arguments (\"%s\")", xa.Name(), xa.Arg(n+1))
		}
	default:
		for _, n := range ns {
			if n == len(xa.Args()) {
				return nil
			}
		}
		seq := strings.ReplaceAll(strings.Trim(fmt.Sprint(ns[:len(ns)-1]), "[]"), " ", ", ")
		return fmt.Errorf("%s: needs %s or %d arguments", xa.Name(), seq, ns[len(ns)-1])
	}
	return nil
}
