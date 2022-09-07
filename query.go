package decap

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
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
	img      []byte
}

func (res *Result) Type() string {
	if len(res.img) != 0 {
		return "png"
	}
	return "json"
}

func (res *Result) ImgBuffer() []byte {
	return res.img
}

type QueryBlock struct {
	Actions    []Action `json:"actions"`
	Repeat     *int     `json:"repeat"`
	While      *Action  `json:"while"`
	cdpActions []chromedp.Action
	cdpWhile   chromedp.Action
	cont       bool
	pos        int
}

type Request struct {
	Query            []*QueryBlock `json:"query"`
	EmulateViewport  []string      `json:"emulate_viewport"`
	ForwardUserAgent bool          `json:"forward_user_agent"`
	RenderDelay      string        `json:"global_render_delay"`
	ReuseTab         bool          `json:"reuse_tab"`
	ReuseWindow      bool          `json:"reuse_window"`
	SessionID        string        `json:"sessionid"`
	Timeout          string        `json:"timeout"`
	oldTabID         string
	pos              int
	renderDelay      time.Duration
	res              Result
	timeout          time.Duration
}

func (r *Request) Execute() (*Result, error) {
	var tab session

	if r.newTab() {
		window := loadWindow(r.SessionID, r.timeout)
		r.SessionID = window.id
		tab = window.createSiblingTabWithTimeout(r.timeout)
	} else {
		tab = loadTab(r.oldTabID)
		if tab.id != r.oldTabID {
			return nil, fmt.Errorf("tab with id \"%s\" doesn't exist", r.oldTabID)
		}
	}
	if r.ReuseWindow {
		r.res.WindowID = r.SessionID
	}
	if r.ReuseTab {
		r.res.TabID = tab.id
		defer tab.saveTab()
	} else {
		defer tab.shutdown()
	}

	var err error
	var block *QueryBlock
	for r.pos, block = range r.Query {

		fmt.Fprintf(os.Stderr, "%s Query %d/%d (session %s)\n",
			time.Now().Format("[15:04:05]"), r.pos+1, len(r.Query), r.SessionID)

		for i := 0; i < *block.Repeat; i++ {
			err = block.cdpWhile.Do(tab.ctx)
			if err != nil {
				return nil, err
			}
			if !block.cont {
				break
			}
			err = chromedp.Run(tab.ctx, block.cdpActions...)
			if err != nil {
				return nil, err
			}
		}
	}

	return &r.res, nil
}

func (r *Request) ParseRequest(body io.Reader) error {
	err := json.NewDecoder(body).Decode(&r)
	if err != nil {
		return fmt.Errorf("JSON parsing error: %s", err)
	}
	if r.ForwardUserAgent {
		// TODO: Implement user agent forwarding in execute()
		return fmt.Errorf("value \"true\" is not supported for init.forward_user_agent")
	}

	err = r.parseEmulateViewport()
	if err != nil {
		return err
	}
	err = r.parseRenderDelay()
	if err != nil {
		return err
	}
	err = r.parseTimeout()
	if err != nil {
		return err
	}
	err = r.parseQueryBlocks()
	if err != nil {
		return err
	}
	return nil
}

func (r *Request) parseEmulateViewport() error {
	switch len(r.EmulateViewport) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("emulate_viewport: width arg has no matching height arg")
	}

	var height, width int64
	var err error
	width, err = strconv.ParseInt(r.EmulateViewport[0], 10, 64)
	if err != nil {
		return fmt.Errorf("emulate_viewport[0]: width must be an integer")
	}
	height, err = strconv.ParseInt(r.EmulateViewport[1], 10, 64)
	if err != nil {
		return fmt.Errorf("emulate_viewport[1]: width must be an integer")
	}

	var options []chromedp.EmulateViewportOption
	for i, option := range r.EmulateViewport[2:] {
		switch option {
		case "landscape":
			options = append(options, chromedp.EmulateLandscape)
		case "mobile":
			options = append(options, chromedp.EmulateMobile)
		case "portrait":
			options = append(options, chromedp.EmulatePortrait)
		default:
			return fmt.Errorf(`emulate_viewport[%d]: unknown option "%s"`, i+2, option)
		}
	}
	r.appendActions(chromedp.EmulateViewport(width, height, options...))
	return nil
}

func (r *Request) parseRenderDelay() error {
	if r.RenderDelay == "" {
		return fmt.Errorf("global_render_delay is empty or missing")
	}
	delay, err := time.ParseDuration(r.RenderDelay)
	if err != nil {
		return fmt.Errorf("invalid global_render_delay: %s", err)
	}
	if delay > MaxRenderDelay {
		delay = MaxRenderDelay
	}
	r.renderDelay = delay
	return nil
}

func (r *Request) parseTimeout() error {
	if r.Timeout == "" {
		r.timeout = 20 * time.Second
		return nil
	}
	timeout, err := time.ParseDuration(r.Timeout)
	if err != nil {
		return fmt.Errorf("invalid timeout: %s", err)
	}
	if timeout > MaxTimeout {
		timeout = MaxTimeout
	}
	r.timeout = timeout
	return nil
}

func (r *Request) parseQueryBlocks() error {

	if len(r.Query) == 0 {
		return fmt.Errorf("query[0] must contain at least one action block")
	}
	if len(r.Query[0].Actions) < 1 {
		return fmt.Errorf("query[0].actions must contain at least one action")
	}
	switch r.Query[0].Actions[0].Name() {
	case "load_tab":
		r.oldTabID = r.Query[0].Actions[0].Arg(1)
		r.Query[0].Actions = r.Query[0].Actions[1:]
		prefix, _, err := parseTabID(r.oldTabID)
		if err != nil {
			return fmt.Errorf("load_tab: %s", err)
		}
		switch r.SessionID {
		case "":
			r.SessionID = prefix
			fmt.Fprintf(os.Stderr, "Request want tab %s, inferring window %s\n",
				r.oldTabID, r.SessionID)
		case prefix:
			fmt.Fprintf(os.Stderr, "Request want tab %s and window %s\n", r.oldTabID, r.SessionID)
		default:
			return fmt.Errorf("tab %s is not part of window session %s", r.oldTabID, r.SessionID)
		}
	case "navigate":
		if len(r.Query[0].Actions) < 2 {
			msg := `query[0].actions must contain at least one other action besides "navigate"`
			return fmt.Errorf(msg)
		}
	default:
		return fmt.Errorf(`query[0].actions[0] must begin with either "load_tab" or "navigate"`)
	}

	if r.hasListeningEvents() {
		r.appendActions(network.Enable(), enableLifecycleEvents())
	}

	r.res.Err = make([]string, len(r.Query))
	r.res.Out = make([][]string, len(r.Query))

	var err error
	var block *QueryBlock
	for r.pos, block = range r.Query {

		// ensure non-nil empty return slices in JSON response
		// r.res.Err[r.pos] = make([]string, 0)
		r.res.Out[r.pos] = make([]string, 0)

		if len(block.Actions) == 0 && r.newTab() {
			return fmt.Errorf("query[%d].actions can't be empty", r.pos)
		}
		const efmt = "query[%d].actions[%v]: %s"

		var xa Action
		for block.pos, xa = range block.Actions {
			err = r.parseAction(xa)
			if err != nil {
				return fmt.Errorf(efmt, r.pos, block.pos, err)
			}
		}

		if err = r.parseRepeat(); err != nil {
			return fmt.Errorf("query[%d].repeat: %s", r.pos, err)
		}
		if err = r.parseWhile(block.While); err != nil {
			return fmt.Errorf("query[%d].while: %s", r.pos, err)
		}

	}

	return nil
}

func (r *Request) hasListeningEvents() bool {
	for _, block := range r.Query {
		for _, xa := range block.Actions {
			if xa.Name() == "listen" {
				return true
			}
		}
	}
	return false
}

func (r *Request) parseRepeat() error {
	block := r.Query[r.pos]
	if block.Repeat == nil {
		var defaultRepeat = 1
		block.Repeat = &defaultRepeat
	}
	if *block.Repeat < 0 {
		return fmt.Errorf("negative value (%d) not allowed", *block.Repeat)
	}
	return nil
}

func (r *Request) parseWhile(xa *Action) error {
	block := r.Query[r.pos]

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
	case "element_visible":
		if err = xa.MustArgCount(1); err != nil {
			return err
		}
		sel := xa.Arg(1)
		if strings.Contains(sel, "'") {
			return fmt.Errorf(`element_visible selector contains "'"`)
		}
		block.cdpWhile = elementVisible(xa.Arg(1), &block.cont)

	default:
		return fmt.Errorf("unknown while action \"%s\"", xa.Name())
	}

	return nil
}

func (r *Request) parseAction(xa Action) error {
	var err error
	if err = xa.MustBeNonEmpty(); err != nil {
		return err
	}

	switch xa.Name() {

	case "click":
		if err = xa.MustArgCount(1); err != nil {
			return err
		}
		r.appendActions(click(xa.Arg(1)))

	case "eval":
		if err = xa.MustArgCount(1); err != nil {
			return err
		}
		// TODO: Append the appropriate action
		return fmt.Errorf("eval not implemented")

	case "hide_navigation", "remove_navigation":
		if err = xa.MustArgCount(0); err != nil {
			return err
		}
		r.appendActions(hideElements(oldNavigationSelector))

	case "hide_nav_buttons":
		if err = xa.MustArgCount(0); err != nil {
			return err
		}
		r.appendActions(hideElements(navButtonSelector))

	case "listen":
		events := xa.Args()
		events, err = parseEvents(events)
		if err != nil {
			return fmt.Errorf("listen: %s", err)
		}
		r.appendActions(listen(&r.SessionID, events...))

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
		r.appendActions(navigate(xurl))

	case "outer_html":
		if err = xa.MustArgCount(0); err != nil {
			return err
		}
		r.appendActions(outerHTML(&r.res.Out[r.pos]))

	case "remove":
		if len(xa.Args()) == 0 {
			return fmt.Errorf("remove: expected at least one argument")
		}
		for i, sel := range xa.Args() {
			if strings.Contains(sel, "'") {
				return fmt.Errorf(`remove[%d]: selector contains "'"`, i)
			}
		}
		r.appendActions(removeElements(strings.Join(xa.Args(), ", ")))

	case "remove_cookie_info", "remove_info_boxes":
		if err = xa.MustArgCount(0); err != nil {
			return err
		}
		r.appendActions(removeElements(infoBoxSelector))

	case "remove_nav_sections":
		if err = xa.MustArgCount(0); err != nil {
			return err
		}
		r.appendActions(removeElements(navSectionSelector))

	case "screenshot":
		args, err := xa.NamedArgs(1)
		if err != nil {
			return err
		}
		element, ok := args["element"]
		if ok && strings.Contains(element, "'") {
			return fmt.Errorf(`element contains "'"`)
		}
		padding, ok := args["padding"]
		if ok && strings.Contains(padding, "'") {
			return fmt.Errorf(`padding contains "'"`)
		}
		r.appendActions(screenshot(args, &r.res.img))

	case "scroll":
		if err = xa.MustArgCount(0, 1); err != nil {
			return err
		}
		if len(xa.Args()) == 0 {
			r.appendActions(scrollToBottom())
		} else {
			r.appendActions(chromedp.ScrollIntoView(xa.Arg(1), chromedp.ByQuery))
		}

	case "sleep":
		if err = xa.MustArgCount(0, 1); err != nil {
			return err
		}
		var delay time.Duration
		if len(xa.Args()) == 0 {
			delay = r.renderDelay
		} else {
			delay, err = time.ParseDuration(xa.Arg(1))
			if err != nil {
				return fmt.Errorf("sleep: invalid duration: %s", err)
			}
		}
		r.appendActions(chromedp.Sleep(delay))

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

func (r *Request) appendActions(actions ...chromedp.Action) {
	block := r.Query[r.pos]
	block.cdpActions = append(block.cdpActions, actions...)
}

func (r *Request) newTab() bool {
	return r.oldTabID == ""
}

type Action []string

func NewAction(list ...string) Action {
	return Action(list)
}

func (xa Action) Arg(n int) string {
	if n < 0 || len(xa) <= n {
		return ""
	}
	return xa[n]
}

func (xa Action) Args() []string {
	if len(xa) == 0 {
		return nil
	}
	return xa[1:]
}

func (xa Action) Name() string {
	return xa.Arg(0)
}

func (xa Action) NamedArgs(offset int) (map[string]string, error) {
	if len(xa) < offset {
		return nil, fmt.Errorf("%s: offset larger than arg list", xa.Name())
	}
	xb := xa[offset:]
	if len(xb)%2 != 0 {
		return nil, fmt.Errorf("%s: expected even number of args", xa.Name())
	}
	args := make(map[string]string)
	for i := 0; i+1 < len(xb); i += 2 {
		args[xb[i]] = xb[i+1]
	}
	return args, nil
}

func (xa Action) MustArgCount(ns ...int) error {
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

func (xa Action) MustBeNonEmpty() error {
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
