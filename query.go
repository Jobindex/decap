package main

import (
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
	MaxTimeout     = 120 * time.Second
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
	TabID    string     `json:"tab_id"`
	WindowID string     `json:"window_id"`
}

type QueryBlock struct {
	Actions    []ExternalAction `json:"actions"`
	Repeat     *int             `json:"repeat"`
	While      *ExternalAction  `json:"while"`
	cdpActions []chromedp.Action
	cdpWhile   chromedp.Action
	cont       bool
	pos        int
}

type Query struct {
	Blocks           []*QueryBlock `json:"query"`
	ForwardUserAgent bool          `json:"forward_user_agent"`
	RenderDelayRaw   string        `json:"global_render_delay"`
	ReuseTab         bool          `json:"reuse_tab"`
	ReuseWindow      bool          `json:"reuse_window"`
	SessionID        string        `json:"sessionid"`
	TimeoutRaw       string        `json:"timeout"`
	oldTabID         string
	pos              int
	renderDelay      time.Duration
	res              Result
	timeout          time.Duration
	userAgent        string
	version          string
}

func (q *Query) execute() error {
	var tab session

	if q.newTab() {
		window := loadWindow(q.SessionID, q.timeout)
		q.SessionID = window.id
		tab = window.createSiblingTabWithTimeout(q.timeout)
	} else {
		tab = loadTab(q.oldTabID)
		if tab.id != q.oldTabID {
			return fmt.Errorf("tab with id \"%s\" doesn't exist", q.oldTabID)
		}
	}
	if q.ReuseWindow {
		q.res.WindowID = q.SessionID
	}
	if q.ReuseTab {
		q.res.TabID = tab.id
		defer tab.saveTab()
	} else {
		defer tab.shutdown()
	}

	var err error
	var block *QueryBlock
	for q.pos, block = range q.Blocks {

		fmt.Fprintf(os.Stderr, "%s Query %d/%d (session %s)\n",
			time.Now().Format("[15:04:05]"), q.pos+1, len(q.Blocks), q.SessionID)

		for i := 0; i < *block.Repeat; i++ {
			err = block.cdpWhile.Do(tab.ctx)
			if err != nil {
				return err
			}
			if !block.cont {
				break
			}
			err = chromedp.Run(tab.ctx, block.cdpActions...)
			if err != nil {
				return err
			}
		}
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
	err = q.parseTimeout()
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

func (q *Query) parseTimeout() error {
	if q.TimeoutRaw == "" {
		q.timeout = 20 * time.Second
		return nil
	}
	timeout, err := time.ParseDuration(q.TimeoutRaw)
	if err != nil {
		return fmt.Errorf("invalid timeout: %s", err)
	}
	if timeout > MaxTimeout {
		timeout = MaxTimeout
	}
	q.timeout = timeout
	return nil
}

func (q *Query) parseQueryBlocks() error {

	if len(q.Blocks) == 0 {
		return fmt.Errorf("query[0] must contain at least one action block")
	}
	if len(q.Blocks[0].Actions) < 1 {
		return fmt.Errorf("query[0].actions must contain at least one action")
	}
	switch q.Blocks[0].Actions[0].Name() {
	case "load_tab":
		q.oldTabID = q.Blocks[0].Actions[0].Arg(1)
		q.Blocks[0].Actions = q.Blocks[0].Actions[1:]
		prefix, _, err := parseTabID(q.oldTabID)
		if err != nil {
			return fmt.Errorf("load_tab: %s", err)
		}
		switch q.SessionID {
		case "":
			q.SessionID = prefix
			fmt.Fprintf(os.Stderr, "Request want tab %s, inferring window %s\n",
				q.oldTabID, q.SessionID)
		case prefix:
			fmt.Fprintf(os.Stderr, "Request want tab %s and window %s\n", q.oldTabID, q.SessionID)
		default:
			return fmt.Errorf("tab %s is not part of window session %s", q.oldTabID, q.SessionID)
		}
	case "navigate":
		if len(q.Blocks[0].Actions) < 2 {
			msg := `query[0].actions must contain at least one other action besides "navigate"`
			return fmt.Errorf(msg)
		}
	default:
		return fmt.Errorf(`query[0].actions[0] must begin with either "load_tab" or "navigate"`)
	}

	if q.hasListeningEvents() {
		q.appendActions(network.Enable(), enableLifecycleEvents())
	}

	q.res.Err = make([]string, len(q.Blocks))
	q.res.Out = make([][]string, len(q.Blocks))

	var err error
	var block *QueryBlock
	for q.pos, block = range q.Blocks {

		// ensure non-nil empty return slices in JSON response
		// q.res.Err[q.pos] = make([]string, 0)
		q.res.Out[q.pos] = make([]string, 0)

		if len(block.Actions) == 0 && q.newTab() {
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

		if err = q.parseRepeat(); err != nil {
			return fmt.Errorf("query[%d].repeat: %s", q.pos, err)
		}
		if err = q.parseWhile(block.While); err != nil {
			return fmt.Errorf("query[%d].while: %s", q.pos, err)
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

func (q *Query) parseRepeat() error {
	block := q.Blocks[q.pos]
	if block.Repeat == nil {
		var defaultRepeat = 1
		block.Repeat = &defaultRepeat
	}
	if *block.Repeat < 0 {
		return fmt.Errorf("negative value (%d) not allowed", *block.Repeat)
	}
	return nil
}

func (q *Query) parseWhile(xa *ExternalAction) error {
	block := q.Blocks[q.pos]

	if xa == nil {
		block.cdpWhile = defaultWhile(&block.cont)
		return nil
	}

	var err error
	if err = xa.MustBeNonEmpty(); err != nil {
		return err
	}

	switch xa.Name() {
	case "element_exists":
		if err = xa.MustArgCount(1); err != nil {
			return err
		}
		block.cdpWhile = elementExists(xa.Arg(1), &block.cont)

	default:
		return fmt.Errorf("unknown while action \"%s\"", xa.Name())
	}

	return nil
}

func (q *Query) parseAction(xa ExternalAction) error {
	var err error
	if err = xa.MustBeNonEmpty(); err != nil {
		return err
	}

	switch xa.Name() {
	case "click":
		if err = xa.MustArgCount(1); err != nil {
			return err
		}
		q.appendActions(click(xa.Arg(1)))
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
		q.appendActions(listen(&q.SessionID, events...))

	case "load_tab":
		if err = xa.MustArgCount(1); err != nil {
			return err
		}
		return fmt.Errorf("load_tab must be the first action of the first action block")

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

	case "scroll":
		if err = xa.MustArgCount(0, 1); err != nil {
			return err
		}
		if len(xa.Args()) == 0 {
			q.appendActions(scrollToBottom())
		} else {
			q.appendActions(chromedp.ScrollIntoView(xa.Arg(1), chromedp.ByQuery))
		}

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
	block := q.Blocks[q.pos]
	block.cdpActions = append(block.cdpActions, actions...)
}

func (q *Query) newTab() bool {
	return q.oldTabID == ""
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

func (xa ExternalAction) MustBeNonEmpty() error {
	if xa.Name() == "" {
		return fmt.Errorf("[0] must contain the name of an action")
	}
	for i, arg := range xa.Args() {
		if arg == "" {
			return fmt.Errorf("[%d] must contain a non-empty argument", i+1)
		}
	}
	return nil
}
